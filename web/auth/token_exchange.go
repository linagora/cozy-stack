package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	oidcbinding "github.com/cozy/cozy-stack/model/oidc/binding"
	oidcprovider "github.com/cozy/cozy-stack/model/oidc/provider"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

const (
	tokenExchangeAdminRole             = "admin"
	tokenExchangeOwnerRole             = "owner"
	tokenExchangeTypeAdmin             = "admin"
	tokenExchangeTypeApp               = "app"
	tokenExchangeOAuthClientName       = "Twake Admin Panel"
	tokenExchangeOAuthClientSoftwareID = "twake-admin-panel"
)

var tokenExchangeAllowedScopes = map[string]struct{}{
	"io.cozy.files":           {},
	"io.cozy.contacts":        {},
	"io.cozy.contacts.groups": {},
	"io.cozy.apps":            {},
	"io.cozy.sharings":        {},
}

// errTokenExchangeDisabled is returned to clients when token exchange is not
// configured for the instance.
var errTokenExchangeDisabled = errors.New("this endpoint is not enabled")

type tokenExchangeRequest struct {
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
	ExchangeType string `json:"exchange_type"`
}

type tokenExchangeResponse struct {
	AccessTokenReponse
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"client_secret"`
	RegistrationToken string `json:"registration_access_token"`
}

type tokenExchangeValidatedToken struct {
	Claims    jwt.MapClaims
	AppConfig *config.OIDCAppTokenExchangeAppConfig
}

// tokenExchangeOAuthClientParams carries the per-exchange information needed
// to register a Cozy OAuth client.
type tokenExchangeOAuthClientParams struct {
	AppSlug    string
	ClientName string
	SoftwareID string
	// SoftwareIDPrevalidated lets the trusted internal app-exchange path skip
	// the registry probe in CheckSoftwareID once the installed manifest has
	// already been verified locally.
	SoftwareIDPrevalidated bool
}

// executeTokenExchange runs the token exchange flow for a request
func executeTokenExchange(c echo.Context, inst *instance.Instance, req tokenExchangeRequest) (*tokenExchangeResponse, error) {
	validated, err := validateTokenExchangeIDToken(inst, req.IDToken, req.ExchangeType)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	var client *oauth.Client
	var scope string
	if req.ExchangeType == tokenExchangeTypeApp {
		if validated.AppConfig == nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, "invalid token audience")
		}
		client, scope, err = executeTokenExchangeApp(c, inst, req, *validated.AppConfig)
	} else {
		client, scope, err = executeTokenExchangeAdmin(c, inst, req)
	}
	if err != nil {
		return nil, err
	}

	defer LockOAuthClient(inst, client.ClientID)()

	if err := bindTokenExchangeOIDCSession(inst, client, validated.Claims); err != nil {
		if delErr := client.Delete(inst); delErr != nil {
			inst.Logger().WithNamespace("oidc").Warnf("Cannot delete orphaned OAuth client %s: %s", client.CouchID, delErr.Description)
		}
		return nil, err
	}

	return buildTokenExchangeResponse(inst, client, scope)
}

func executeTokenExchangeAdmin(c echo.Context, inst *instance.Instance, req tokenExchangeRequest) (*oauth.Client, string, error) {
	if err := validateTokenExchangeScope(req.Scope); err != nil {
		return nil, "", echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	client, err := createTokenExchangeOAuthClient(c, inst, tokenExchangeOAuthClientParams{
		ClientName: tokenExchangeOAuthClientName,
		SoftwareID: tokenExchangeOAuthClientSoftwareID,
	})
	if err != nil {
		return nil, "", err
	}
	return client, req.Scope, nil
}

func executeTokenExchangeApp(c echo.Context, inst *instance.Instance, req tokenExchangeRequest, appConfig config.OIDCAppTokenExchangeAppConfig) (*oauth.Client, string, error) {
	if req.Scope != "" {
		return nil, "", echo.NewHTTPError(http.StatusBadRequest, "scope is not allowed for app token exchange")
	}

	slug := oauth.GetLinkedAppSlug(appConfig.SoftwareID)
	if slug == "" {
		return nil, "", echo.NewHTTPError(http.StatusBadRequest, "software_id must be a linked app")
	}
	manifest, err := app.GetWebappBySlug(inst, slug)
	if err != nil {
		return nil, "", echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is not installed", slug))
	}
	if err := tokenExchangeAssertManifestTrusted(manifest, slug); err != nil {
		return nil, "", err
	}
	client, err := createTokenExchangeOAuthClient(c, inst, tokenExchangeOAuthClientParams{
		AppSlug:                slug,
		ClientName:             tokenExchangeLinkedAppClientName(manifest, slug),
		SoftwareID:             appConfig.SoftwareID,
		SoftwareIDPrevalidated: true,
	})
	if err != nil {
		return nil, "", err
	}

	return client, oauth.BuildLinkedAppScope(slug), nil
}

// tokenExchangeAssertManifestTrusted confirms the locally installed manifest
// really came from the registry path the config maps the audience to. This
// is the local equivalent of CheckSoftwareID's registry probe, and lets the
// flow proceed when the registry is unreachable.
func tokenExchangeAssertManifestTrusted(manifest *app.WebappManifest, slug string) error {
	source := strings.TrimSpace(manifest.Source())
	if !strings.HasPrefix(source, "registry://") {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is not a registry application", slug))
	}
	if oauth.GetLinkedAppSlug(source) != slug {
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("%s is not the installed application", slug))
	}
	return nil
}

func tokenExchangeRequestExchangeType(raw string) (string, error) {
	exchangeType := strings.TrimSpace(raw)
	if exchangeType == "" {
		return tokenExchangeTypeAdmin, nil
	}
	switch exchangeType {
	case tokenExchangeTypeAdmin, tokenExchangeTypeApp:
		return exchangeType, nil
	default:
		return "", fmt.Errorf("exchange_type %q is not allowed", raw)
	}
}

func validateTokenExchangeScope(scope string) error {
	if scope == "" {
		return errors.New("scope is required")
	}
	for _, s := range strings.Split(scope, " ") {
		if _, ok := tokenExchangeAllowedScopes[s]; !ok {
			return fmt.Errorf("scope %q is not allowed", s)
		}
	}
	return nil
}

func validateTokenExchangeIDToken(inst *instance.Instance, raw, exchangeType string) (*tokenExchangeValidatedToken, error) {
	if inst == nil {
		return nil, errors.New("instance is missing")
	}
	conf, err := oidcprovider.LoadConfig(
		inst.ContextName,
		oidcprovider.RequireIDTokenKeyURL,
		oidcprovider.RequireIssuerOrTokenURL,
	)
	if err != nil {
		inst.Logger().WithNamespace("oidc").
			Warnf("token exchange disabled: OIDC config invalid for context %s: %s", inst.ContextName, err)
		return nil, errTokenExchangeDisabled
	}

	claims, err := oidcprovider.VerifyIDToken(raw, conf)
	if err != nil {
		return nil, errors.New("invalid token")
	}

	expectedIssuer, err := oidcprovider.GetIssuer(inst.ContextName, conf)
	if err != nil {
		inst.Logger().WithNamespace("oidc").Errorf("Cannot get OIDC issuer for context %s: %s", inst.ContextName, err)
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "internal server error")
	}
	issuer, err := claims.GetIssuer()
	if err != nil || issuer == "" || issuer != expectedIssuer {
		return nil, errors.New("invalid token issuer")
	}
	issuedAt, err := claims.GetIssuedAt()
	if err != nil || issuedAt == nil {
		return nil, errors.New("invalid token")
	}
	if issuedAt.Time.After(time.Now().Add(5 * time.Minute)) {
		return nil, errors.New("invalid token")
	}
	expiresAt, err := claims.GetExpirationTime()
	if err != nil || expiresAt == nil {
		return nil, errors.New("invalid token")
	}
	if time.Now().After(expiresAt.Time) {
		return nil, errors.New("invalid token")
	}
	sid, _ := tokenExchangeClaimString(claims, "sid")
	if sid == "" {
		return nil, errors.New("sid claim is required")
	}

	validated := &tokenExchangeValidatedToken{
		Claims: claims,
	}
	switch exchangeType {
	case tokenExchangeTypeAdmin:
		if err := validateTokenExchangeAdminToken(inst, conf, claims); err != nil {
			return nil, err
		}
	case tokenExchangeTypeApp:
		appConfig, err := validateTokenExchangeAppToken(inst, conf, claims)
		if err != nil {
			return nil, err
		}
		validated.AppConfig = appConfig
	default:
		return nil, fmt.Errorf("exchange_type %q is not allowed", exchangeType)
	}
	return validated, nil
}

func validateTokenExchangeAdminToken(inst *instance.Instance, conf *oidcprovider.Config, claims jwt.MapClaims) error {
	if !conf.AllowOAuthToken {
		inst.Logger().WithNamespace("oidc").
			Warnf("admin token exchange disabled for context %s: allow_oauth_token=false", inst.ContextName)
		return errTokenExchangeDisabled
	}
	if conf.ClientID == "" {
		inst.Logger().WithNamespace("oidc").
			Warnf("admin token exchange disabled for context %s: missing client_id in OIDC config", inst.ContextName)
		return errTokenExchangeDisabled
	}
	if !tokenExchangeAudienceMatches(claims, conf.ClientID) {
		return errors.New("invalid token audience")
	}
	if !tokenExchangeHasAdminRole(claims) {
		return errors.New("admin authorization is required")
	}

	orgID, _ := tokenExchangeClaimString(claims, "org_id")
	if orgID == "" || subtle.ConstantTimeCompare([]byte(orgID), []byte(inst.OrgID)) == 0 {
		return errors.New("org_id mismatch")
	}
	if inst.OrgDomain != "" {
		orgDomain, ok := tokenExchangeClaimString(claims, "org_domain")
		if !ok || orgDomain == "" {
			return errors.New("org_domain claim is required")
		}
		if subtle.ConstantTimeCompare([]byte(orgDomain), []byte(inst.OrgDomain)) == 0 {
			return errors.New("org_domain mismatch")
		}
	}

	return nil
}

func validateTokenExchangeAppToken(inst *instance.Instance, conf *oidcprovider.Config, claims jwt.MapClaims) (*config.OIDCAppTokenExchangeAppConfig, error) {
	appExchangeConfig, err := config.GetOIDCAppTokenExchange(inst.ContextName)
	if err != nil {
		inst.Logger().WithNamespace("oidc").
			Warnf("app token exchange config invalid for context %s: %s", inst.ContextName, err)
		return nil, errTokenExchangeDisabled
	}
	if !appExchangeConfig.Enabled {
		inst.Logger().WithNamespace("oidc").
			Warnf("app token exchange disabled for context %s: allow_app_token_exchange=false", inst.ContextName)
		return nil, errTokenExchangeDisabled
	}
	if len(appExchangeConfig.Apps) == 0 {
		inst.Logger().WithNamespace("oidc").
			Warnf("app token exchange disabled for context %s: no app_token_exchange entries configured", inst.ContextName)
		return nil, errTokenExchangeDisabled
	}
	appConfig, ok := tokenExchangeMatchingAppConfig(claims, appExchangeConfig.Apps)
	if !ok {
		return nil, errors.New("invalid token audience")
	}
	if err := tokenExchangeCheckAppInstance(conf, inst, claims, *appConfig); err != nil {
		return nil, err
	}
	return appConfig, nil
}

func tokenExchangeCheckAppInstance(conf *oidcprovider.Config, inst *instance.Instance, claims jwt.MapClaims, appConfig config.OIDCAppTokenExchangeAppConfig) error {
	if appConfig.InstanceClaim == "" {
		return oidcprovider.CheckClaimsForInstance(conf, inst, claims)
	}

	rawDomain, ok := tokenExchangeClaimString(claims, appConfig.InstanceClaim)
	if !ok || rawDomain == "" {
		inst.Logger().WithNamespace("oidc").
			Warnf("Cannot extract instance claim %s", appConfig.InstanceClaim)
		return oidcprovider.ErrInstanceAuthenticationFailed
	}

	domain := utils.NormalizeDomain(rawDomain)
	if domain == "" {
		inst.Logger().WithNamespace("oidc").
			Warnf("Cannot extract domain from instance claim %s", appConfig.InstanceClaim)
		return oidcprovider.ErrInstanceAuthenticationFailed
	}

	expectedDomain := utils.NormalizeDomain(inst.Domain)
	if domain != expectedDomain {
		inst.Logger().WithNamespace("oidc").
			Errorf("Invalid domains: %s != %s", domain, inst.Domain)
		return &oidcprovider.InstanceMismatchError{ExpectedDomain: inst.Domain, ActualDomain: domain}
	}
	return nil
}

func createTokenExchangeOAuthClient(c echo.Context, inst *instance.Instance, params tokenExchangeOAuthClientParams) (*oauth.Client, error) {
	redirectURI, err := tokenExchangeRedirectURI(c, inst, params.AppSlug)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := config.GetRateLimiter().CheckRateLimit(inst, limits.OAuthClientType); limits.IsLimitReachedOrExceeded(err) {
		return nil, echo.NewHTTPError(http.StatusNotFound, "Not found")
	}

	client := &oauth.Client{
		RedirectURIs: []string{redirectURI},
		ClientName:   params.ClientName,
		ClientKind:   "browser",
		SoftwareID:   params.SoftwareID,
	}
	var opts []oauth.CreateOptions
	if params.SoftwareIDPrevalidated {
		opts = append(opts, oauth.SoftwareIDPrevalidated)
	}
	if regErr := client.Create(inst, opts...); regErr != nil {
		return nil, echo.NewHTTPError(regErr.Code, regErr.Description)
	}

	storedClient, err := oauth.FindClient(inst, client.ClientID)
	if err != nil {
		return nil, err
	}
	storedClient.RegistrationToken = client.RegistrationToken
	return storedClient, nil
}

func tokenExchangeLinkedAppClientName(manifest *app.WebappManifest, slug string) string {
	name := strings.TrimSpace(manifest.Name())
	if name == "" {
		return slug
	}
	prefix := strings.TrimSpace(manifest.NamePrefix())
	if prefix == "" {
		return name
	}
	return prefix + " " + name
}

func bindTokenExchangeOIDCSession(inst *instance.Instance, client *oauth.Client, claims jwt.MapClaims) error {
	sessionID, _ := tokenExchangeClaimString(claims, "sid")
	if sessionID != "" {
		client.OIDCSessionID = sessionID
	}

	client.Pending = false
	client.ClientID = ""
	if err := couchdb.UpdateDoc(inst, client); err != nil {
		inst.Logger().WithNamespace("oidc").Warnf("Cannot update OAuth client %s: %s", client.CouchID, err)
		return err
	}

	if sessionID != "" {
		if err := oidcbinding.BindOAuthClient(inst.ContextName, inst.Domain, sessionID, client.CouchID); err != nil {
			inst.Logger().WithNamespace("oidc").Errorf("Cannot bind OIDC session %s to OAuth client %s: %s", sessionID, client.CouchID, err)
			return fmt.Errorf("cannot bind OIDC session to OAuth client: %w", err)
		}
	}

	client.ClientID = client.CouchID
	return nil
}

func buildTokenExchangeResponse(inst *instance.Instance, client *oauth.Client, scope string) (*tokenExchangeResponse, error) {
	out := &tokenExchangeResponse{
		AccessTokenReponse: AccessTokenReponse{
			Type:  "bearer",
			Scope: scope,
		},
		ClientID:          client.ClientID,
		ClientSecret:      client.ClientSecret,
		RegistrationToken: client.RegistrationToken,
	}

	refreshToken, err := client.CreateJWT(inst, consts.RefreshTokenAudience, scope)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "Can't generate refresh token")
	}
	out.Refresh = refreshToken

	accessToken, err := client.CreateJWT(inst, consts.AccessTokenAudience, scope)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusInternalServerError, "Can't generate access token")
	}
	out.Access = accessToken

	client.LastRefreshedAt = time.Now()
	if err := couchdb.UpdateDoc(inst, client); err != nil {
		inst.Logger().WithNamespace("oidc").Warnf("Cannot update LastRefreshedAt for client %s: %s", client.CouchID, err)
	}

	return out, nil
}

func tokenExchangeAudienceMatches(claims jwt.MapClaims, clientID string) bool {
	aud, err := claims.GetAudience()
	if err == nil && len(aud) > 0 {
		for _, value := range aud {
			if value == clientID {
				return true
			}
		}
		return false
	}
	azp, _ := tokenExchangeClaimString(claims, "azp")
	return azp == clientID
}

// tokenExchangeMatchingAppConfig returns the first app exchange config whose
// configured audience appears in the token's `aud` claim. If multiple audiences
// match, the order in the token's `aud` slice wins.
func tokenExchangeMatchingAppConfig(claims jwt.MapClaims, appConfigs map[string]config.OIDCAppTokenExchangeAppConfig) (*config.OIDCAppTokenExchangeAppConfig, bool) {
	audiences, err := claims.GetAudience()
	if err != nil {
		return nil, false
	}
	for _, audience := range audiences {
		if appConfig, ok := appConfigs[audience]; ok {
			return &appConfig, true
		}
	}
	return nil, false
}

func tokenExchangeAppSlugAllowed(contextName, slug string) bool {
	appExchangeConfig, err := config.GetOIDCAppTokenExchange(contextName)
	if err != nil || !appExchangeConfig.Enabled {
		return false
	}
	for _, appConfig := range appExchangeConfig.Apps {
		if oauth.GetLinkedAppSlug(appConfig.SoftwareID) == slug {
			return true
		}
	}
	return false
}

func tokenExchangeHasAdminRole(claims jwt.MapClaims) bool {
	orgRole, ok := tokenExchangeClaimString(claims, "org_role")
	return ok && (strings.EqualFold(orgRole, tokenExchangeAdminRole) ||
		strings.EqualFold(orgRole, tokenExchangeOwnerRole))
}

func tokenExchangeClaimString(claims jwt.MapClaims, key string) (string, bool) {
	raw, ok := claims[key]
	if !ok || raw == nil {
		return "", false
	}
	value, ok := raw.(string)
	return value, ok
}

// tokenExchangeRedirectURI returns the redirect URI to attach to the
// exchanged OAuth client.
//
// For app exchanges (appSlug != ""), the redirect URI is always the app's
// own subdomain on this instance: callers cannot influence it via the
// Origin header because executeTokenExchangeApp has already enforced that
// the Origin (if any) is exactly that subdomain.
//
// For admin exchanges, the Origin header is honoured when it points
// somewhere other than the instance itself, which lets the admin panel host
// its own redirect target.
func tokenExchangeRedirectURI(c echo.Context, inst *instance.Instance, appSlug string) (string, error) {
	if inst != nil && appSlug != "" {
		u := inst.SubDomain(appSlug)
		u.Path = ""
		return u.String(), nil
	}

	origin := c.Request().Header.Get(echo.HeaderOrigin)
	if origin != "" && inst != nil {
		u, err := url.Parse(origin)
		if err == nil && u.Scheme != "" && u.Host != "" && utils.StripPort(u.Host) != inst.Domain {
			u.Path = ""
			u.RawQuery = ""
			u.Fragment = ""
			return u.String(), nil
		}
	}
	if inst != nil && inst.OrgDomain != "" {
		return "https://admin." + inst.OrgDomain, nil
	}
	return "", errors.New("cannot determine redirect URI")
}
