package auth

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

const (
	// SessionCodeSourceAppTokenExchange marks session codes minted from app token exchange access tokens.
	SessionCodeSourceAppTokenExchange = "app_token_exchange"
	// SessionCodeSourceFlagship marks session codes minted from flagship access tokens.
	SessionCodeSourceFlagship = "flagship"
	// SessionCodeSourcePassword marks session codes minted after a password or passphrase check.
	SessionCodeSourcePassword = "password"
)

// SessionCodeGrant authorizes a token caller to mint a session code.
type SessionCodeGrant struct {
	Permission *permission.Permission
	Source     string
}

// CreateSessionCode is the handler for creating a session code by the flagship
// app.
func CreateSessionCode(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	result, source := canCreateSessionCode(c, inst)
	switch result {
	case allowedToCreateSessionCode:
		return ReturnSessionCode(c, http.StatusCreated, inst, source)
	case need2FAToCreateSessionCode:
		twoFactorToken, err := lifecycle.SendTwoFactorPasscode(inst)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusForbidden, echo.Map{
			"error":            "two factor needed",
			"two_factor_token": string(twoFactorToken),
		})
	default:
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "Not authorized",
		})
	}
}

func ReturnSessionCode(c echo.Context, statusCode int, inst *instance.Instance, source string) error {
	code, err := MintSessionCode(c, inst, source)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err,
		})
	}

	return c.JSON(statusCode, echo.Map{
		"session_code": code,
	})
}

// MintSessionCode mints a session code and logs its audit source.
func MintSessionCode(c echo.Context, inst *instance.Instance, source string) (string, error) {
	code, err := inst.CreateSessionCode()
	if err != nil {
		return "", err
	}

	req := c.Request()
	var ip string
	if forwardedFor := req.Header.Get(echo.HeaderXForwardedFor); forwardedFor != "" {
		ip = strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
	}
	if ip == "" {
		ip = strings.Split(req.RemoteAddr, ":")[0]
	}
	inst.Logger().WithField("nspace", "loginaudit").
		WithField("source", source).
		Infof("New session_code created from %s at %s", ip, time.Now())

	return code, nil
}

type sessionCodeParameters struct {
	Passphrase     string `json:"passphrase"`
	TwoFactorToken string `json:"two_factor_token"`
	TwoFactorCode  string `json:"two_factor_passcode"`
}

type canCreateSessionCodeResult int

const (
	allowedToCreateSessionCode canCreateSessionCodeResult = iota
	cannotCreateSessionCode
	need2FAToCreateSessionCode
)

func canCreateSessionCode(c echo.Context, inst *instance.Instance) (canCreateSessionCodeResult, string) {
	if grant, ok := AuthorizeSessionCodeToken(c, inst); ok {
		return allowedToCreateSessionCode, grant.Source
	}

	var args sessionCodeParameters
	if err := c.Bind(&args); err != nil {
		return cannotCreateSessionCode, ""
	}
	if err := instance.CheckPassphrase(inst, []byte(args.Passphrase)); err != nil {
		return cannotCreateSessionCode, ""
	}

	if inst.HasAuthMode(instance.TwoFactorMail) {
		token := []byte(args.TwoFactorToken)
		if ok := inst.ValidateTwoFactorPasscode(token, args.TwoFactorCode); !ok {
			return need2FAToCreateSessionCode, ""
		}
	}
	return allowedToCreateSessionCode, SessionCodeSourcePassword
}

// AuthorizeSessionCodeToken returns a grant for token flows allowed to mint a
// session code. It intentionally does not use the passphrase fallback accepted
// by the /auth/session_code endpoint.
func AuthorizeSessionCodeToken(c echo.Context, inst *instance.Instance) (SessionCodeGrant, bool) {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return SessionCodeGrant{}, false
	}
	if pdoc.Permissions.IsMaximal() {
		return SessionCodeGrant{Permission: pdoc, Source: SessionCodeSourceFlagship}, true
	}
	if claims, ok := c.Get("claims").(permission.Claims); ok && isAppTokenExchangeToken(inst, pdoc, claims) {
		return SessionCodeGrant{Permission: pdoc, Source: SessionCodeSourceAppTokenExchange}, true
	}
	return SessionCodeGrant{Permission: pdoc}, false
}

func isAppTokenExchangeToken(inst *instance.Instance, pdoc *permission.Permission, claims permission.Claims) bool {
	if pdoc.Type != permission.TypeOauth || pdoc.Client == nil {
		return false
	}
	client, ok := pdoc.Client.(*oauth.Client)
	if !ok {
		return false
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != consts.AccessTokenAudience {
		return false
	}
	if claims.Subject == "" || claims.Subject != pdoc.SourceID {
		return false
	}

	slug := oauth.GetLinkedAppSlug(client.SoftwareID)
	if slug == "" {
		return false
	}
	if claims.Scope != oauth.BuildLinkedAppScope(slug) {
		return false
	}
	if !tokenExchangeAppSlugAllowed(inst.ContextName, slug) {
		return false
	}
	if _, err := app.GetWebappBySlug(inst, slug); err != nil {
		return false
	}
	return true
}

func postChallenge(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	err := config.GetRateLimiter().CheckRateLimit(inst, limits.OAuthClientType)
	if limits.IsLimitReachedOrExceeded(err) {
		return echo.NewHTTPError(http.StatusNotFound, "Not found")
	}
	client := c.Get("client").(*oauth.Client)
	nonce, err := client.CreateChallenge(inst)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{"error": err.Error()})
	}
	return c.JSON(http.StatusCreated, echo.Map{"nonce": nonce})
}

func postAttestation(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	client, err := oauth.FindClient(inst, c.Param("client-id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "Client not found",
		})
	}
	var data oauth.AttestationRequest
	if err := json.NewDecoder(c.Request().Body).Decode(&data); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": err.Error(),
		})
	}
	if err := client.Attest(inst, data); err != nil {
		inst.Logger().Infof("Cannot attest %s client: %s", client.ID(), err.Error())
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": err.Error(),
		})
	}
	return c.NoContent(http.StatusNoContent)
}

func confirmFlagship(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	client, err := oauth.FindClient(inst, c.Param("client-id"))
	if err != nil {
		return c.JSON(http.StatusNotFound, echo.Map{
			"error": "Client not found",
		})
	}

	err = config.GetRateLimiter().CheckRateLimit(inst, limits.ConfirmFlagshipType)
	if limits.IsLimitReachedOrExceeded(err) {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": inst.Translate("Confirm Flagship Invalid code"),
		})
	}

	clientID := c.Param("client-id")
	code := c.FormValue("code")
	token := []byte(c.FormValue("token"))
	if ok := oauth.CheckFlagshipCode(inst, clientID, token, code); !ok {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": inst.Translate("Confirm Flagship Invalid code"),
		})
	}

	if err := client.SetFlagship(inst); err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error,
		})
	}
	return c.NoContent(http.StatusNoContent)
}

type loginFlagshipParameters struct {
	ClientID          string `json:"client_id"`
	ClientSecret      string `json:"client_secret"`
	Passphrase        string `json:"passphrase"`
	TwoFactorPasscode string `json:"two_factor_passcode"`
	TwoFactorToken    string `json:"two_factor_token"`
	EmailVerifiedCode string `json:"email_verified_code"`
}

func loginFlagship(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	var args loginFlagshipParameters
	if err := c.Bind(&args); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if instance.CheckPassphrase(inst, []byte(args.Passphrase)) != nil {
		err := config.GetRateLimiter().CheckRateLimit(inst, limits.AuthType)
		if limits.IsLimitReachedOrExceeded(err) {
			if err = LoginRateExceeded(inst); err != nil {
				inst.Logger().WithNamespace("auth").Warn(err.Error())
			}
		}
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": inst.Translate(CredentialsErrorKey),
		})
	}

	if inst.HasAuthMode(instance.TwoFactorMail) && !inst.CheckEmailVerifiedCode(args.EmailVerifiedCode) {
		if len(args.TwoFactorToken) == 0 {
			twoFactorToken, err := lifecycle.SendTwoFactorPasscode(inst)
			if err != nil {
				return err
			}
			return c.JSON(http.StatusUnauthorized, echo.Map{
				"two_factor_token": string(twoFactorToken),
			})
		}
		twoFactorToken := []byte(args.TwoFactorToken)
		if !inst.ValidateTwoFactorPasscode(twoFactorToken, args.TwoFactorPasscode) {
			return c.JSON(http.StatusForbidden, echo.Map{
				"error": inst.Translate(TwoFactorErrorKey),
			})
		}
	}

	client, err := oauth.FindClient(inst, args.ClientID)
	if err != nil {
		if couchErr, isCouchErr := couchdb.IsCouchError(err); isCouchErr && couchErr.StatusCode >= 500 {
			return err
		}
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "the client must be registered",
		})
	}
	if subtle.ConstantTimeCompare([]byte(args.ClientSecret), []byte(client.ClientSecret)) == 0 {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid client_secret",
		})
	}

	if !client.Flagship {
		return ReturnSessionCode(c, http.StatusAccepted, inst, SessionCodeSourcePassword)
	}

	if client.Pending {
		client.Pending = false
		client.ClientID = ""
		_ = couchdb.UpdateDoc(inst, client)
		client.ClientID = client.CouchID
	}

	out := AccessTokenReponse{
		Type:  "bearer",
		Scope: "*",
	}
	out.Refresh, err = client.CreateJWT(inst, consts.RefreshTokenAudience, out.Scope)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate refresh token",
		})
	}
	out.Access, err = client.CreateJWT(inst, consts.AccessTokenAudience, out.Scope)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate access token",
		})
	}
	return c.JSON(http.StatusOK, out)
}
