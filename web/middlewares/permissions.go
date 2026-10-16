// Package middlewares is used for the HTTP middlewares, ie functions that
// takes an echo context to do stuff like checking permissions or caching
// requests.
package middlewares

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/logger"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

const bearerAuthScheme = "Bearer "
const basicAuthScheme = "Basic "
const contextPermissionDoc = "permissions_doc"

// ErrForbidden is used to send a forbidden response when the request does not
// have the right permissions.
var ErrForbidden = echo.NewHTTPError(http.StatusForbidden)

// ErrMissingSource is used to send a bad request when the SourceURL is missing
// from the request
var ErrMissingSource = echo.NewHTTPError(http.StatusBadRequest, "No Source in request")

var errNoToken = echo.NewHTTPError(http.StatusUnauthorized, "No token in request")

// CheckRegisterToken returns true if the registerToken is set and match the
// one from the instance.
func CheckRegisterToken(c echo.Context, i *instance.Instance) bool {
	if len(i.RegisterToken) == 0 {
		return false
	}
	hexToken := c.QueryParam("registerToken")
	if hexToken == "" {
		return false
	}
	tok, err := hex.DecodeString(hexToken)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(tok, i.RegisterToken) == 1
}

// GetRequestToken retrieves the token from the incoming request.
func GetRequestToken(c echo.Context) string {
	req := c.Request()
	if header := req.Header.Get(echo.HeaderAuthorization); header != "" {
		if strings.HasPrefix(header, bearerAuthScheme) {
			return header[len(bearerAuthScheme):]
		}
		if strings.HasPrefix(header, basicAuthScheme) {
			_, pass, _ := req.BasicAuth()
			return pass
		}
	}
	return c.QueryParam("bearer_token")
}

type linkedAppScope struct {
	Doctype string
	Slug    string
}

func parseLinkedAppScope(scope string) (*linkedAppScope, error) {
	if !strings.HasPrefix(scope, "@") {
		return nil, fmt.Errorf("Scope %s is not a linked-app", scope)
	}
	splitted := strings.Split(strings.TrimPrefix(scope, "@"), "/")

	return &linkedAppScope{
		Doctype: splitted[0],
		Slug:    splitted[1],
	}, nil
}

// GetForOauth create a non-persisted permissions doc from a oauth token scopes
func GetForOauth(instance *instance.Instance, claims *permission.Claims, client *oauth.Client) (*permission.Permission, error) {
	var set permission.Set
	linkedAppScope, err := parseLinkedAppScope(claims.Scope)

	if claims.Scope == "*" {
		context := instance.ContextName
		if context == "" {
			context = config.DefaultInstanceContext
		}
		cfg := config.GetConfig().Flagship.Contexts[context]
		skipCertification := false
		if cfg, ok := cfg.(map[string]interface{}); ok {
			skipCertification = cfg["skip_certification"] == true
		}
		if !skipCertification && !client.Flagship {
			return nil, permission.ErrInvalidToken
		}
		set = permission.MaximalSet()
	} else if err == nil && linkedAppScope != nil {
		// Translate to a real scope
		at := consts.NewAppType(linkedAppScope.Doctype)
		manifest, err := app.GetBySlug(instance, linkedAppScope.Slug, at)
		if err != nil {
			return nil, err
		}
		set = manifest.Permissions()
	} else {
		set, err = permission.UnmarshalScopeString(claims.Scope)
		if err != nil {
			return nil, err
		}
	}

	pdoc := &permission.Permission{
		Type:        permission.TypeOauth,
		Permissions: set,
		SourceID:    claims.Subject,
		Client:      client,
	}
	return pdoc, nil
}

var shortCodeRegexp = regexp.MustCompile(`^(\d{6}|(\w|\d){12})\.?$`)

// ExtractClaims parse a JWT, and extracts its claims (if valid).
func ExtractClaims(c echo.Context, instance *instance.Instance, token string) (*permission.Claims, error) {
	var fullClaims permission.BitwardenClaims
	var audience string

	err := crypto.ParseJWT(token, func(token *jwt.Token) (interface{}, error) {
		audiences := token.Claims.(*permission.BitwardenClaims).Claims.Audience
		if len(audiences) != 1 {
			return nil, permission.ErrInvalidAudience
		}
		audience = audiences[0]
		return instance.PickKey(audience)
	}, &fullClaims)

	// XXX: bitwarden clients have the OAuth client ID in client_id, not subject
	claims := fullClaims.Claims
	if audience == consts.AccessTokenAudience && fullClaims.ClientID != "" && claims.Subject == instance.ID() {
		claims.Subject = fullClaims.ClientID
	}

	c.Set("claims", claims)

	if err != nil {
		logger.WithNamespace("permissions").Debugf("invalid token: %s", err)
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, permission.ErrExpiredToken
		}
		return nil, permission.ErrInvalidToken
	}
	if claims.Actor != nil || claims.IntentID != "" {
		if claims.Actor == nil || claims.Actor.Subject == "" || claims.IntentID == "" || claims.ExpiresAt == nil || claims.SessionID == "" {
			return nil, permission.ErrInvalidToken
		}
		if claims.AudienceString() != consts.AppAudience {
			return nil, permission.ErrInvalidToken
		}
	}

	// check if the claim is valid
	if claims.Issuer != instance.Domain && claims.Issuer != instance.OldDomain {
		logger.WithNamespace("permissions").
			Debugf("invalid token: bad domain %s != %s", claims.Issuer, instance.Domain)
		return nil, permission.ErrInvalidToken
	}

	if claims.Expired() {
		logger.WithNamespace("permissions").Debugf("invalid token: expired")
		return nil, permission.ErrExpiredToken
	}

	// If claims contains a SessionID, we check that we are actually authorized
	// with the corresponding session.
	if claims.SessionID != "" {
		s, ok := GetSession(c)
		if !ok || s.ID() != claims.SessionID {
			if ok {
				logger.WithNamespace("permissions").
					Debugf("invalid token: bad session %s != %s", s.ID(), claims.SessionID)
			} else {
				logger.WithNamespace("permissions").
					Debugf("invalid token: no session")
			}
			return nil, permission.ErrInvalidToken
		}
	}

	// If claims contains a security stamp, we check that the stamp is still
	// the same.
	if claims.SStamp != "" {
		settings, err := settings.Get(instance)
		if err != nil || claims.SStamp != settings.SecurityStamp {
			if err != nil {
				logger.WithNamespace("permissions").
					Debugf("could not get instance settings: %s", err)
			} else {
				logger.WithNamespace("permissions").
					Debugf("invalid token: bad security stamp %s != %s", claims.SStamp, settings.SecurityStamp)
			}
			return nil, permission.ErrInvalidToken
		}
	}

	return &claims, nil
}

// HasCookieForPassword returns true if a cookie has been set for the
// permission with a given ID if its password has been given by the user, and a
// cookie has been put for that.
func HasCookieForPassword(c echo.Context, inst *instance.Instance, permID string) bool {
	cookieName := "pass" + permID
	cookie, err := c.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return false
	}

	cfg := crypto.MACConfig{Name: cookieName, MaxLen: 256}
	id, err := crypto.DecodeAuthMessage(cfg, inst.SessionSecret(), []byte(cookie.Value), nil)
	if err != nil {
		return false
	}

	return string(id) == permID
}

// TransformShortcodeToJWT takes a token. If it is a short code, it transforms
// it to a JWT by using the associated permission. Else, it just returns the
// token.
func TransformShortcodeToJWT(inst *instance.Instance, token string) (string, error) {
	if !shortCodeRegexp.MatchString(token) {
		return token, nil
	}

	// XXX in theory, the shortcode is exactly 12 characters. But
	// somethimes, when people shares a public link with this token, they
	// can put a "." just after the link to finish their sentence, and this
	// "." can be added to the token. So, it's better to accept a shortcode
	// with a final ".", and clean it.
	token = strings.TrimSuffix(token, ".")
	return permission.GetTokenFromShortcode(inst, token)
}

// ParseJWT parses a JSON Web Token, and returns the associated permissions.
func ParseJWT(c echo.Context, instance *instance.Instance, token string) (*permission.Permission, error) {
	token, err := TransformShortcodeToJWT(instance, token)
	if err != nil {
		return nil, err
	}

	claims, err := ExtractClaims(c, instance, token)
	if err != nil {
		if errors.Is(err, permission.ErrExpiredToken) {
			c.Response().Header().Set(echo.HeaderWWWAuthenticate,
				`Bearer error="invalid_token" error_description="The access token expired"`)
		} else {
			c.Response().Header().Set(echo.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
		}
		return nil, err
	}

	pdoc, err := getPermissionForClaims(c, instance, claims, token)
	if err != nil {
		return nil, err
	}
	if claims.Actor != nil {
		return &permission.Permission{
			Type:        permission.TypeIntent,
			IntentID:    claims.IntentID,
			SourceID:    consts.Apps + "/" + claims.Actor.Subject,
			Permissions: pdoc.Permissions,
		}, nil
	}
	return pdoc, nil
}

func getPermissionForClaims(c echo.Context, instance *instance.Instance, claims *permission.Claims, token string) (*permission.Permission, error) {
	switch claims.AudienceString() {
	case consts.AccessTokenAudience:
		if err := instance.MovedError(); err != nil {
			return nil, err
		}
		// An OAuth2 token is only valid if the client has not been revoked
		client, err := oauth.FindClient(instance, claims.Subject)
		if err != nil {
			if couchdb.IsInternalServerError(err) {
				return nil, err
			}
			logger.WithNamespace("permissions").
				Debugf("invalid token: no client for OAuth - %s", err)
			c.Response().Header().Set(echo.HeaderWWWAuthenticate, `Bearer error="invalid_token"`)
			return nil, permission.ErrInvalidToken
		}
		return GetForOauth(instance, claims, client)

	case consts.CLIAudience:
		// do not check client existence
		return permission.GetForCLI(claims)

	case consts.AppAudience:
		pdoc, err := permission.GetForWebapp(instance, claims.Subject)
		if err != nil {
			logger.WithNamespace("permissions").
				Debugf("invalid token: no permission for webapp - %s", err)
			return nil, err
		}
		return pdoc, nil

	case consts.KonnectorAudience:
		pdoc, err := permission.GetForKonnector(instance, claims.Subject)
		if err != nil {
			logger.WithNamespace("permissions").
				Debugf("invalid token: no permission for konnector - %s", err)
			return nil, err
		}
		return pdoc, nil

	case consts.ShareAudience:
		pdoc, err := permission.GetForShareCode(instance, token)
		if err != nil {
			return nil, err
		}

		// Check that the password has been given for password protected share by link
		if pdoc.Password != nil && !HasCookieForPassword(c, instance, pdoc.ID()) {
			return nil, permission.ErrInvalidToken
		}

		// A share token is only valid if the user has not been revoked
		if pdoc.Type == permission.TypeSharePreview || pdoc.Type == permission.TypeShareInteract {
			sharingID := strings.Split(pdoc.SourceID, "/")
			sharingDoc, err := sharing.FindSharing(instance, sharingID[1])
			if err != nil {
				return nil, err
			}

			var member *sharing.Member
			if pdoc.Type == permission.TypeSharePreview {
				member, err = sharingDoc.FindMemberBySharecode(instance, token)
			} else {
				member, err = sharingDoc.FindMemberByInteractCode(instance, token)
			}
			if err != nil {
				return nil, err
			}

			if member.Status == sharing.MemberStatusRevoked {
				return nil, permission.ErrInvalidToken
			}

			if member.Status == sharing.MemberStatusMailNotSent ||
				member.Status == sharing.MemberStatusPendingInvitation {
				member.Status = sharing.MemberStatusSeen
				_ = couchdb.UpdateDoc(instance, sharingDoc)
			}
		}

		return pdoc, nil

	default:
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("Unrecognized token audience %v", claims.Audience))
	}
}

// GetCLIPermission tries to extract a CLI permission from the echo context
// without tampering with the response headers in case the token is invalid.
func GetCLIPermission(c echo.Context) (*permission.Permission, bool) {
	var err error

	pdoc, ok := c.Get(contextPermissionDoc).(*permission.Permission)
	if ok && pdoc != nil && pdoc.Type == permission.TypeCLI {
		return pdoc, true
	}

	instance := GetInstance(c)

	token := GetRequestToken(c)
	if token == "" {
		return nil, false
	}

	claims, err := ExtractClaims(c, instance, token)
	if err != nil {
		return nil, false
	}

	if claims.AudienceString() == consts.CLIAudience {
		if pdoc, err := permission.GetForCLI(claims); err != nil {
			c.Set(contextPermissionDoc, pdoc)
			return pdoc, true
		}
	}

	return nil, false
}

// GetPermission extracts the permission from the echo context and checks their validity
func GetPermission(c echo.Context) (*permission.Permission, error) {
	var err error

	pdoc, ok := c.Get(contextPermissionDoc).(*permission.Permission)
	if ok && pdoc != nil {
		return pdoc, nil
	}

	inst := GetInstance(c)
	if CheckRegisterToken(c, inst) {
		return permission.GetForRegisterToken(), nil
	}

	tok := GetRequestToken(c)
	if tok == "" {
		return nil, errNoToken
	}

	pdoc, err = ParseJWT(c, inst, tok)
	if err != nil {
		return nil, err
	}
	if err = ResolveRequestActor(c, inst, pdoc); err != nil {
		return nil, err
	}

	c.Set(contextPermissionDoc, pdoc)
	return pdoc, nil
}

// ForcePermission can be used to force a permission document for an
// echo.Context.
func ForcePermission(c echo.Context, pdoc *permission.Permission) {
	c.Set(contextPermissionDoc, pdoc)
}

// AllowWholeType validates that the context permission set can use a verb on
// the whold doctype
func AllowWholeType(c echo.Context, v permission.Verb, doctype string) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if !pdoc.Permissions.AllowWholeType(v, doctype) {
		return ErrForbidden
	}
	return nil
}

// Allow validates the validable object against the context permission set
func Allow(c echo.Context, v permission.Verb, o permission.Fetcher) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if !pdoc.Permissions.Allow(v, o) {
		return ErrForbidden
	}
	return nil
}

// AllowOnFields validates the validable object againt the context permission
// set and ensure the selector validates the given fields.
func AllowOnFields(c echo.Context, v permission.Verb, o permission.Fetcher, fields ...string) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if !pdoc.Permissions.AllowOnFields(v, o, fields...) {
		return ErrForbidden
	}
	return nil
}

// AllowTypeAndID validates a type & ID against the context permission set
func AllowTypeAndID(c echo.Context, v permission.Verb, doctype, id string) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if !pdoc.Permissions.AllowID(v, doctype, id) {
		return ErrForbidden
	}
	return nil
}

// AllowVFS validates a vfs.Fetcher against the context permission set
func AllowVFS(c echo.Context, v permission.Verb, o vfs.Fetcher) error {
	instance := GetInstance(c)
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if pdoc.Permissions.IsMaximal() {
		return nil
	}
	err = vfs.Allows(instance.VFS(), pdoc.Permissions, v, o)
	if err != nil {
		return ErrForbidden
	}
	return nil
}

// CanWriteToAnyDirectory checks that the context permission allows to write to
// a directory on the VFS.
func CanWriteToAnyDirectory(c echo.Context) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	for _, rule := range pdoc.Permissions {
		if permission.MatchType(rule, consts.Files) && rule.Verbs.Contains(permission.POST) {
			return nil
		}
	}
	return ErrForbidden
}

// AllowInstallApp checks that the current context is tied to the store app,
// which is the only app authorized to install or update other apps.
// It also allow the cozy-stack apps commands to work (CLI).
func AllowInstallApp(c echo.Context, appType consts.AppType, sourceURL string, v permission.Verb) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}

	if pdoc.Permissions.IsMaximal() {
		return nil
	}

	var docType string
	switch appType {
	case consts.KonnectorType:
		docType = consts.Konnectors
	case consts.WebappType:
		docType = consts.Apps
	}

	if docType == "" {
		return fmt.Errorf("unknown application type %s", appType.String())
	}
	switch pdoc.Type {
	case permission.TypeCLI:
		// OK
	case permission.TypeWebapp, permission.TypeKonnector:
		if pdoc.SourceID != consts.Apps+"/"+consts.StoreSlug {
			inst := GetInstance(c)
			ctxSettings, ok := inst.SettingsContext()
			if !ok || ctxSettings["allow_install_via_a_permission"] != true {
				return ErrForbidden
			}
		}
		// The store can only install apps and konnectors from the registry
		if !strings.HasPrefix(sourceURL, "registry://") {
			return ErrForbidden
		}
	case permission.TypeOauth:
		// If the context allows to install an app via a permission, this
		// permission can also be used by mobile apps to install apps from the
		// registry.
		inst := GetInstance(c)
		ctxSettings, ok := inst.SettingsContext()
		if !ok || ctxSettings["allow_install_via_a_permission"] != true {
			return ErrForbidden
		}
		if !strings.HasPrefix(sourceURL, "registry://") {
			return ErrForbidden
		}
	default:
		return ErrForbidden
	}
	if !pdoc.Permissions.AllowWholeType(v, docType) {
		return ErrForbidden
	}
	return nil
}

// AllowForKonnector checks that the permissions is valid and comes from the
// konnector with the given slug.
func AllowForKonnector(c echo.Context, slug string) error {
	if slug == "" {
		return ErrForbidden
	}
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if pdoc.Type != permission.TypeKonnector {
		return ErrForbidden
	}
	permSlug := strings.TrimPrefix(pdoc.SourceID, consts.Konnectors+"/")
	if permSlug != slug {
		return ErrForbidden
	}
	return nil
}

// AllowLogout checks if the current permission allows logging out.
// all apps can trigger a logout.
func AllowLogout(c echo.Context) bool {
	return HasWebAppToken(c)
}

// AllowMaximal checks that the permission is for the flagship app.
func AllowMaximal(c echo.Context) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	if !pdoc.Permissions.IsMaximal() {
		return ErrForbidden
	}
	return nil
}

// RequireSettingsApp checks that the permission is for the settings app.
func RequireSettingsApp(c echo.Context) error {
	pdoc, err := GetPermission(c)
	if err != nil {
		return err
	}
	settingsSourceID := consts.Apps + "/" + consts.SettingsSlug
	if pdoc.Type != permission.TypeWebapp || pdoc.SourceID != settingsSourceID {
		return ErrForbidden
	}
	return nil
}

// HasWebAppToken returns true if the request comes from a web app (with a token).
func HasWebAppToken(c echo.Context) bool {
	pdoc, err := GetPermission(c)
	if err != nil {
		return false
	}
	return pdoc.Type == permission.TypeWebapp
}

// GetOAuthClient returns the OAuth client used for making the HTTP request.
func GetOAuthClient(c echo.Context) (*oauth.Client, bool) {
	perm, err := GetPermission(c)
	if err != nil || perm.Type != permission.TypeOauth || perm.Client == nil {
		return nil, false
	}
	return perm.Client.(*oauth.Client), true
}
