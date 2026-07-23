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

// CreateSessionCode is the handler for creating a session code by the flagship
// app.
func CreateSessionCode(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	var source string
	switch canCreateSessionCode(c, inst) {
	case allowedToCreateSessionCode:
		source = "flagship"
		if s, ok := c.Get("session_code_source").(string); ok {
			source = s
		}
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

	return ReturnSessionCode(c, http.StatusCreated, inst, source)
}

func ReturnSessionCode(c echo.Context, statusCode int, inst *instance.Instance, source string) error {
	code, err := inst.CreateSessionCode()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err,
		})
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

	return c.JSON(statusCode, echo.Map{
		"session_code": code,
	})
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

func canCreateSessionCode(c echo.Context, inst *instance.Instance) canCreateSessionCodeResult {
	if err := middlewares.AllowMaximal(c); err == nil {
		return allowedToCreateSessionCode
	}

	if isAppTokenExchangeToken(c, inst) {
		c.Set("session_code_source", "app_token_exchange")
		return allowedToCreateSessionCode
	}

	var args sessionCodeParameters
	if err := c.Bind(&args); err != nil {
		return cannotCreateSessionCode
	}
	if err := instance.CheckPassphrase(inst, []byte(args.Passphrase)); err != nil {
		return cannotCreateSessionCode
	}

	if inst.HasAuthMode(instance.TwoFactorMail) {
		token := []byte(args.TwoFactorToken)
		if ok := inst.ValidateTwoFactorPasscode(token, args.TwoFactorCode); !ok {
			return need2FAToCreateSessionCode
		}
	}
	return allowedToCreateSessionCode
}

func isAppTokenExchangeToken(c echo.Context, inst *instance.Instance) bool {
	rawClaims := c.Get("claims")
	if rawClaims == nil {
		return false
	}
	claims, ok := rawClaims.(permission.Claims)
	if !ok {
		return false
	}
	if len(claims.Audience) != 1 || claims.Audience[0] != consts.AccessTokenAudience {
		return false
	}
	clientID := claims.Subject
	if clientID == "" {
		return false
	}

	client, err := oauth.FindClient(inst, clientID)
	if err != nil {
		return false
	}

	appExchangeConfig, err := config.GetOIDCAppTokenExchange(inst.ContextName)
	if err != nil || !appExchangeConfig.Enabled || len(appExchangeConfig.Apps) == 0 {
		return false
	}

	for _, appConfig := range appExchangeConfig.Apps {
		if appConfig.SoftwareID == client.SoftwareID {
			slug := oauth.GetLinkedAppSlug(client.SoftwareID)
			if slug == "" {
				return false
			}
			if _, err := app.GetWebappBySlug(inst, slug); err != nil {
				return false
			}
			return true
		}
	}
	return false
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
		return ReturnSessionCode(c, http.StatusAccepted, inst, "password")
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
