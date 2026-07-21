package oidc

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	oidcbinding "github.com/cozy/cozy-stack/model/oidc/binding"
	oidcprovider "github.com/cozy/cozy-stack/model/oidc/provider"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/bitwarden"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/statik"
	jwt "github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
)

var (
	ErrInvalidToken         = errors.New("invalid token")
	ErrInvalidConfiguration = errors.New("invalid configuration")
	ErrAuthenticationFailed = oidcprovider.ErrInstanceAuthenticationFailed
	ErrFranceConnectFailed  = errors.New("the FranceConnect authentication has failed")
	ErrIdentityProvider     = errors.New("error from the identity provider")
	ErrAmbiguousLogout      = errors.New("ambiguous logout context")
)

// DomainMismatchError is returned when the user tries to connect to an
// instance but has an active OIDC session for a different instance/account.
type DomainMismatchError = oidcprovider.InstanceMismatchError

// extractSessionID extracts the session ID (sid) from an id_token.
func extractSessionID(idToken string) string {
	if idToken == "" {
		return ""
	}
	claims := jwt.MapClaims{}
	_, _, _ = jwt.NewParser().ParseUnverified(idToken, claims)
	sid, _ := claims["sid"].(string)
	if sid != "" {
		logger.WithNamespace("oidc").Debugf("Extracted session ID from id_token: %s", sid)
	}
	return sid
}

// Start is the route to start the OpenID Connect dance.
func Start(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	conf, err := oidcprovider.LoadConfig(
		inst.ContextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireScope,
		oidcprovider.RequireRedirectURI,
		oidcprovider.RequireAuthorizeURL,
	)
	if err != nil {
		inst.Logger().WithNamespace("oidc").Infof("Start error: %s", err)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the context was not found.")
	}
	u, err := makeStartURL(inst, inst.Domain, c.QueryParam("redirect"), c.QueryParam("confirm_state"), "", conf)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	return c.Redirect(http.StatusSeeOther, u)
}

// StartFranceConnect is the route to start the FranceConnect dance.
func StartFranceConnect(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	conf, err := oidcprovider.LoadFranceConnectConfig(
		inst.ContextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireScope,
		oidcprovider.RequireRedirectURI,
		oidcprovider.RequireAuthorizeURL,
	)
	if err != nil {
		inst.Logger().WithNamespace("oidc").Infof("StartFranceConnect error: %s", err)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the context was not found.")
	}
	u, err := makeStartURL(inst, inst.Domain, c.QueryParam("redirect"), c.QueryParam("confirm_state"), "", conf)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	return c.Redirect(http.StatusSeeOther, u)
}

// Sharing is the route to use the SSO to accept a sharing.
func Sharing(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	return RedirectSharingWithContext(c, inst, inst.ContextName, c.QueryParam("sharingID"), c.QueryParam("state"), "")
}

// RedirectSharingWithContext starts the sharing OIDC flow with the given context.
// If loginHint is empty, the usual instance login hint is used.
func RedirectSharingWithContext(c echo.Context, inst *instance.Instance, contextName, sharingID, sharingState, loginHint string) error {
	conf, err := oidcprovider.LoadConfig(
		contextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireScope,
		oidcprovider.RequireRedirectURI,
		oidcprovider.RequireAuthorizeURL,
	)
	if err != nil {
		inst.Logger().WithNamespace("oidc").Infof("Start error: %s", err)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the context was not found.")
	}
	if loginHint == "" {
		loginHint = getLoginHint(inst)
	}
	u, err := makeSharingStartURLWithLoginHint(inst, sharingID, sharingState, conf, contextName, loginHint)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	return c.Redirect(http.StatusSeeOther, u)
}

// SharingPublic is the route to use the public Twake SSO to accept a sharing.
func SharingPublic(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	conf, contextName, err := getPublicOIDCConfig()
	if err != nil {
		inst.Logger().WithNamespace("oidc").Infof("Start error: %s", err)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	u, err := makeSharingStartURL(inst, c.QueryParam("sharingID"), c.QueryParam("state"), conf, contextName)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	return c.Redirect(http.StatusSeeOther, u)
}

func checkRedirectURIForBitwarden(redirectURI string) error {
	if redirectURI == "" {
		return errors.New("redirect URI is required")
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		return errors.New("invalid redirect URI")
	}
	if u.Scheme == "cozypass" || u.Scheme == "moz-extension" || u.Scheme == "chrome-extension" {
		return nil
	}
	if u.Scheme == "https" && u.Host == "links.mycozy.cloud" {
		return nil
	}
	return errors.New("invalid redirect URI")
}

// BitwardenStart starts the OIDC flow for Bitwarden clients
func BitwardenStart(c echo.Context) error {
	contextName := c.Param("context")
	redirectURI := c.QueryParam("redirect_uri")

	if err := checkRedirectURIForBitwarden(redirectURI); err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the redirect URI is not valid.")
	}

	conf, err := oidcprovider.LoadConfig(
		contextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireScope,
		oidcprovider.RequireRedirectURI,
		oidcprovider.RequireAuthorizeURL,
	)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the context was not found.")
	}
	u, err := makeStartURL(nil, "", redirectURI, "", contextName, conf)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	return c.Redirect(http.StatusSeeOther, u)
}

// BitwardenExchange handles the POST /oidc/bitwarden/:context route for exchanging
// a delegated code for bitwarden credentials
func BitwardenExchange(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	code := c.FormValue("code")
	pass := c.FormValue("password")
	if code == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "code parameter is required",
		})
	}

	sub := getStorage().GetSub(code)
	invalidCode := sub == ""
	if sub != inst.OIDCID && sub != inst.Domain {
		invalidCode = true
	}
	if invalidCode {
		inst.Logger().WithNamespace("oidc").Infof("BitwardenExchange invalid code: %s (%s - %s)",
			sub, inst.OIDCID, inst.Domain)
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid code",
		})
	}
	if err := instance.CheckPassphrase(inst, []byte(pass)); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid password",
		})
	}

	return bitwarden.RegisterClientAndReturnTokens(c, inst)
}

// Redirect is the route after the Identity Provider has redirected the user to
// the stack. The redirection is made to a generic domain, like
// oauthcallback.cozy.localhost and the association with an instance is made via a
// call to the UserInfo endpoint. It redirects to the cozy instance to login
// the user.
func Redirect(c echo.Context) error {
	code := c.QueryParam("code")
	stateID := c.QueryParam("state")
	state := getStorage().Find(stateID)
	if state == nil {
		logOIDCSessionExpired(c, "oidc_redirect", "state_not_found", nil, stateID)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the session has expired.")
	}

	// The delegated code flow is used for Bitwarden-style flows with a Redirect URL.
	// For sharing flows (where SharingID is set), we skip this block and use the
	// regular flow that redirects to the instance's /oidc/login endpoint.
	if state.OIDCContext != "" && state.SharingID == "" {
		conf, err := oidcprovider.LoadConfig(
			state.OIDCContext,
			oidcprovider.RequireClientID,
			oidcprovider.RequireClientSecret,
			oidcprovider.RequireRedirectURI,
			oidcprovider.RequireTokenURL,
			oidcprovider.RequireUserInfoURL,
			oidcprovider.RequireUserInfoMapping,
		)
		if err != nil {
			return renderError(c, nil, http.StatusBadRequest, "No OpenID Connect is configured.")
		}
		// Use getToken to get both access_token and id_token
		tokenResp, err := getToken(conf, c.QueryParam("code"))
		if err != nil {
			logger.WithNamespace("oidc").Errorf("Error on getToken: %s", err)
			return renderError(c, nil, http.StatusBadGateway, "Error from the identity provider.")
		}
		params, err := getUserInfo(conf, tokenResp.AccessToken)
		if err != nil {
			return err
		}
		sub, ok := params["sub"].(string)
		if !ok {
			logger.WithNamespace("oidc").Errorf("Missing sub")
			return ErrAuthenticationFailed
		}
		domain, err := oidcprovider.ExtractDomainFromClaims(conf, params)
		if err == ErrAuthenticationFailed && conf.AllowCustomInstance {
			inst, err := oidcprovider.FindInstanceBySub(sub, state.OIDCContext)
			if err != nil {
				return renderError(c, nil, http.StatusNotFound, "Sorry, the twake was not found.")
			}
			domain = inst.Domain
		} else if err != nil {
			return renderError(c, nil, http.StatusNotFound, "Sorry, the twake was not found.")
		}

		// Extract session ID from id_token for OIDC logout support
		sessionID := extractSessionID(tokenResp.IDToken)
		code := getStorage().CreateCodeData(sub, sessionID)
		u, err := url.Parse(state.Redirect)
		if err != nil {
			return renderError(c, nil, http.StatusNotFound, "Sorry, an error occurred.")
		}
		q := u.Query()
		q.Add("code", code)
		q.Add("instance", domain)
		u.RawQuery = q.Encode()
		return c.Redirect(http.StatusSeeOther, u.String())
	}

	domain := state.Instance
	if contextName, ok := FindLoginDomain(domain); ok {
		conf, err := oidcprovider.LoadConfig(
			contextName,
			oidcprovider.RequireUserInfoURL,
			oidcprovider.RequireUserInfoMapping,
		)
		if err != nil || !conf.AllowOAuthToken {
			return renderError(c, nil, http.StatusBadRequest, "No OpenID Connect is configured.")
		}
		token := c.QueryParam("access_token")
		domain, err = getDomainFromUserInfo(conf, token)
		if err != nil {
			return renderError(c, nil, http.StatusNotFound, "Sorry, the twake was not found.")
		}
	}
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the twake was not found.")
	}

	u := url.Values{
		"code":  {code},
		"state": {stateID},
	}
	if state.Redirect != "" {
		u.Add("redirect", state.Redirect)
	}
	if state.Confirm != "" {
		u.Add("confirm_state", state.Confirm)
	}
	if state.Provider == oidcprovider.FranceConnectProvider {
		u.Add("franceconnect", "true")
		if c.QueryParam("nonce") != state.Nonce {
			return renderError(c, nil, http.StatusBadRequest, "Sorry, an error occurred.")
		}
	}
	redirect := inst.PageURL("/oidc/login", u)
	return c.Redirect(http.StatusSeeOther, redirect)
}

// Login checks that the OpenID Connect has been successful and logs in the user.
func Login(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	stateID := c.QueryParam("state")
	var state *stateHolder
	if stateID != "" {
		state = getStorage().Find(stateID)
	}

	var conf *Config
	var err error
	if c.QueryParam("franceconnect") == "" {
		contextName := inst.ContextName
		if state != nil && state.OIDCContext != "" {
			contextName = state.OIDCContext
		}
		conf, err = oidcprovider.LoadConfig(contextName)
	} else {
		conf, err = oidcprovider.LoadFranceConnectConfig(inst.ContextName)
	}
	if err != nil {
		return renderError(c, inst, http.StatusBadRequest, "No OpenID Connect is configured.")
	}

	redirect := c.QueryParam("redirect")
	confirm := c.QueryParam("confirm_state")
	idToken := c.QueryParam("id_token")

	err = config.GetRateLimiter().CheckRateLimit(inst, limits.AuthType)
	if limits.IsLimitReachedOrExceeded(err) {
		if err = auth.LoginRateExceeded(inst); err != nil {
			inst.Logger().WithNamespace("oidc").Warn(err.Error())
		}
		logOIDCSessionExpired(c, "oidc_login", "rate_limited", inst, stateID)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the session has expired.")
	}

	var token string
	// Track the id_token separately for SID extraction (used for OIDC logout)
	var idTokenForSID string

	if idToken != "" && conf.IDTokenKeyURL != "" {
		if err := checkIDToken(conf, inst, idToken); err != nil {
			return renderError(c, inst, http.StatusBadRequest, err.Error())
		}
		token = idToken
		idTokenForSID = idToken
	} else {
		if conf.AllowOAuthToken {
			token = c.QueryParam("access_token")
		}
		if token == "" {
			if state == nil {
				logOIDCSessionExpired(c, "oidc_login", "state_not_found", inst, stateID)
				return renderError(c, nil, http.StatusNotFound, "Sorry, the session has expired.")
			}
			code := c.QueryParam("code")
			// Use getToken to get both access_token and id_token
			tokenResp, err := getToken(conf, code)
			if err != nil {
				logger.WithNamespace("oidc").Errorf("Error on getToken: %s", err)
				return renderError(c, inst, http.StatusBadGateway, "Error from the identity provider.")
			}
			token = tokenResp.AccessToken
			idTokenForSID = tokenResp.IDToken
			if state.SharingID != "" {
				return acceptSharing(c, inst, conf, token, state)
			}
		}

		// Check 2FA if enabled, and if yes, render an HTML page to check if
		// the browser has a trusted device token in its local storage.
		if inst.HasAuthMode(instance.TwoFactorMail) {
			return c.Render(http.StatusOK, "oidc_twofactor.html", echo.Map{
				"Domain":      inst.ContextualDomain(),
				"AccessToken": token,
				"Redirect":    redirect,
				"Confirm":     confirm,
			})
		}

		if err := checkDomainFromUserInfo(conf, inst, token); err != nil {
			extras := map[string]interface{}{}
			errMsg := err.Error()
			var dmErr *DomainMismatchError
			if errors.As(err, &dmErr) {
				extras["ErrorArgs"] = dmErr.TranslationArgs()
				errMsg = dmErr.TranslationKey()
				if logoutURL := getOIDCLogoutURL(inst.ContextName); logoutURL != "" {
					extras["Button"] = "Disconnect"
					extras["ButtonURL"] = logoutURL
				}
			}
			return renderError(c, inst, http.StatusBadRequest, errMsg, extras)
		}
	}

	// Extract SID from id_token (not access_token) for OIDC logout support
	sid := extractSessionID(idTokenForSID)
	return createSessionAndRedirect(c, inst, redirect, confirm, sid)
}

func acceptSharing(c echo.Context, ownerInst *instance.Instance, conf *Config, token string, state *stateHolder) error {
	params, err := getUserInfo(conf, token)
	if err != nil {
		return err
	}

	sub, ok := params["sub"].(string)
	if !ok {
		logger.WithNamespace("oidc").Errorf("Missing sub")
		return ErrAuthenticationFailed
	}
	oidcLog := ownerInst.Logger().WithNamespace("oidc")
	oidcLog.Infof(
		"OIDC sharing lookup: owner=%s sharing_id=%s oidc_context=%s allow_custom_instance=%t sub=%q userinfo_field=%q prefix=%q suffix=%q",
		ownerInst.Domain, state.SharingID, state.OIDCContext, conf.AllowCustomInstance,
		sub, conf.UserInfoField, conf.UserInfoPrefix, conf.UserInfoSuffix,
	)
	var memberInst *instance.Instance
	if state.OIDCContext != "" && conf.AllowCustomInstance {
		// Try direct lookup by OIDC subject first when OIDCContext is specified
		oidcLog.Infof(
			"OIDC sharing lookup by oidc_id: oidc_id=%q context=%s",
			sub, state.OIDCContext,
		)
		memberInst, err = oidcprovider.FindInstanceBySub(sub, state.OIDCContext)
		if err != nil {
			oidcLog.Warnf(
				"OIDC sharing lookup by oidc_id failed: oidc_id=%q context=%s error=%s",
				sub, state.OIDCContext, err,
			)
		}
	}
	if memberInst == nil {
		// Fall back to domain extraction from user info
		domain, err := oidcprovider.ExtractDomainFromClaims(conf, params)
		if err == nil {
			oidcLog.Infof("OIDC sharing lookup by domain: domain=%s", domain)
			memberInst, err = lifecycle.GetInstance(domain)
			if err != nil {
				oidcLog.Infof(
					"OIDC sharing lookup by domain failed: domain=%s error=%s",
					domain, err,
				)
			}
		} else if errors.Is(err, ErrAuthenticationFailed) && conf.AllowCustomInstance {
			oidcLog.Infof(
				"OIDC sharing lookup domain extraction failed, retrying oidc_id lookup: oidc_id=%q context=%s error=%s",
				sub, state.OIDCContext, err,
			)
			memberInst, err = oidcprovider.FindInstanceBySub(sub, state.OIDCContext)
			if err != nil {
				oidcLog.Infof(
					"OIDC sharing lookup retry by oidc_id failed: oidc_id=%q context=%s error=%s",
					sub, state.OIDCContext, err,
				)
			}
		} else {
			oidcLog.Infof("OIDC sharing lookup domain extraction failed: error=%s", err)
		}
	}
	if err != nil || memberInst == nil {
		oidcLog.Infof(
			"OIDC sharing lookup failed: owner=%s sharing_id=%s oidc_context=%s sub=%q error=%v",
			ownerInst.Domain, state.SharingID, state.OIDCContext, sub, err,
		)
		return renderError(c, nil, http.StatusNotFound, "Sorry, the twake was not found.")
	}

	s, err := sharing.FindSharing(ownerInst, state.SharingID)
	if err != nil {
		return renderError(c, ownerInst, http.StatusNotFound, "Sorry, the sharing was not found.")
	}
	member, err := s.FindMemberByState(state.SharingState)
	if err != nil {
		if errors.Is(err, sharing.ErrMemberNotFound) || errors.Is(err, sharing.ErrInvalidSharing) {
			return renderError(c, ownerInst, http.StatusNotFound, "Sorry, the sharing was not found.")
		}
		return renderError(c, ownerInst, http.StatusInternalServerError, "Sorry, the sharing invitation could not be processed.")
	}
	if memberInst.Domain == ownerInst.Domain {
		return renderError(c, ownerInst, http.StatusBadRequest, "Sorry, you cannot accept your own sharing invitation.")
	}
	memberURL := memberInst.PageURL("/", nil)
	if err = s.RegisterCozyURLInteractive(ownerInst, member, memberURL); err != nil {
		return renderError(c, ownerInst, http.StatusInternalServerError, "Sorry, the sharing invitation could not be processed.")
	}
	sharing.PersistInstanceURL(ownerInst, member.Email, member.Instance)
	redirectURL, err := member.GenerateOAuthURL(s, "")
	if err != nil {
		return renderError(c, ownerInst, http.StatusInternalServerError, "Sorry, the sharing invitation could not be processed.")
	}
	return c.Redirect(http.StatusFound, redirectURL)
}

func TwoFactor(c echo.Context) error {
	accessToken := c.FormValue("access-token")
	redirect := c.FormValue("redirect")
	confirm := c.FormValue("confirm")
	trustedDeviceToken := []byte(c.FormValue("trusted-device-token"))

	inst := middlewares.GetInstance(c)
	conf, err := oidcprovider.LoadConfig(
		inst.ContextName,
		oidcprovider.RequireUserInfoURL,
		oidcprovider.RequireUserInfoMapping,
	)
	if err != nil {
		return renderError(c, inst, http.StatusBadRequest, "No OpenID Connect is configured.")
	}
	if err := checkDomainFromUserInfo(conf, inst, accessToken); err != nil {
		extras := map[string]interface{}{}
		errMsg := err.Error()
		if dmErr, ok := err.(*DomainMismatchError); ok {
			extras["ErrorArgs"] = dmErr.TranslationArgs()
			errMsg = dmErr.TranslationKey()
			if logoutURL := getOIDCLogoutURL(inst.ContextName); logoutURL != "" {
				extras["Button"] = "Disconnect"
				extras["ButtonURL"] = logoutURL
			}
		}
		return renderError(c, inst, http.StatusBadRequest, errMsg, extras)
	}

	if inst.ValidateTwoFactorTrustedDeviceSecret(c.Request(), trustedDeviceToken) {
		return createSessionAndRedirect(c, inst, redirect, confirm, "")
	}

	twoFactorToken, err := lifecycle.SendTwoFactorPasscode(inst)
	if err != nil {
		return err
	}
	v := url.Values{}
	v.Add("two_factor_token", string(twoFactorToken))
	if redirect != "" {
		v.Add("redirect", redirect)
	}
	if confirm != "" {
		v.Add("confirm", "true")
		v.Add("state", confirm)
	}
	return c.Redirect(http.StatusSeeOther, inst.PageURL("/auth/twofactor", v))
}

func createSessionAndRedirect(c echo.Context, inst *instance.Instance, redirect, confirm, sid string) error {
	// The OIDC danse has been made to confirm the identity of the user, not
	// for creating a new session.
	if confirm != "" {
		return auth.ConfirmSuccess(c, inst, confirm)
	}

	sessionID, err := auth.SetCookieForNewSession(c, session.NormalRun, sid)
	if err != nil {
		return err
	}
	if err = session.StoreNewLoginEntry(inst, sessionID, "", c.Request(), "OIDC"); err != nil {
		inst.Logger().Errorf("Could not store session history %q: %s", sessionID, err)
	}
	if redirect == "" {
		redirect = inst.DefaultRedirection().String()
	}
	return c.Redirect(http.StatusSeeOther, redirect)
}

type logoutContext struct {
	ContextName string
	Conf        *Config
	Claims      jwt.MapClaims
}

// Logout is the handler for the OpenID back-channel logout endpoint.
func Logout(c echo.Context) error {
	contextHint := c.Param("context")
	logoutToken := c.FormValue("logout_token")
	if logoutToken == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{"error": "missing logout_token"})
	}

	rawClaims, err := parseLogoutTokenUnverified(logoutToken)
	if err != nil {
		logger.WithNamespace("oidc").Errorf("Error on ParseUnverified for logout token: %s", err)
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error":             "error on parsing the token",
			"error_description": err.Error(),
		})
	}
	sid, _ := rawClaims["sid"].(string)
	sub, _ := rawClaims["sub"].(string)
	iss, _ := rawClaims.GetIssuer()
	aud, _ := rawClaims.GetAudience()
	logger.WithNamespace("oidc").Debugf(
		"Received backchannel logout request: context_hint=%s iss=%s aud=%v has_sid=%t has_sub=%t",
		contextHint, iss, aud, sid != "", sub != "",
	)

	contextName, err := resolveLogoutContext(contextHint, rawClaims)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrAmbiguousLogout) {
			status = http.StatusConflict
		}
		logger.WithNamespace("oidc").Errorf("Error on resolveLogoutContext: %s", err)
		return c.JSON(status, echo.Map{
			"error":             "invalid logout token",
			"error_description": err.Error(),
		})
	}
	logger.WithNamespace("oidc").Debugf("Resolved logout context %s for backchannel logout request", contextName)

	verified, err := verifyLogoutTokenForContext(logoutToken, contextName)
	if err != nil {
		logger.WithNamespace("oidc").Errorf("Error on verifyLogoutTokenForContext for %s: %s", contextName, err)
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error":             "invalid logout token",
			"error_description": err.Error(),
		})
	}

	if err := applyLogoutForContext(*verified); err != nil {
		logger.WithNamespace("oidc").Errorf("Error on applyLogoutForContext for %s: %s", verified.ContextName, err)
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error":             "internal server error",
			"error_description": err.Error(),
		})
	}

	c.Response().Header().Set("Cache-Control", "no-store")
	logger.WithNamespace("oidc").Debugf("Completed backchannel logout for context %s", verified.ContextName)
	return c.NoContent(http.StatusOK)
}

func parseLogoutTokenUnverified(logoutToken string) (jwt.MapClaims, error) {
	claims := jwt.MapClaims{}
	_, _, err := jwt.NewParser().ParseUnverified(logoutToken, claims)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func resolveLogoutContext(contextHint string, claims jwt.MapClaims) (string, error) {
	if contextHint != "" {
		logger.WithNamespace("oidc").Debugf("Using logout context hint %s", contextHint)
		return contextHint, nil
	}

	sid, _ := claims["sid"].(string)
	iss, _ := claims.GetIssuer()
	aud, _ := claims.GetAudience()
	issuerAudienceContexts := findOIDCContextsByIssuerAudience(iss, aud)

	var sidContexts []string
	var err error
	if sid != "" {
		sidContexts, err = oidcbinding.FindProviderKeys(sid)
		if err != nil {
			return "", err
		}
	}
	logger.WithNamespace("oidc").Debugf(
		"Resolved logout context candidates: sid=%s iss=%s aud=%v sid_contexts=%v issuer_audience_contexts=%v",
		sid, iss, aud, sidContexts, issuerAudienceContexts,
	)

	// if there is only one session in redis, return it as is
	switch len(sidContexts) {
	// if there is no session, try to resolve it by OIDC configuration in logout token
	case 0:
		{
			if len(issuerAudienceContexts) == 1 {
				logger.WithNamespace("oidc").Debugf(
					"Resolved logout context %s from issuer/audience only",
					issuerAudienceContexts[0],
				)
				return issuerAudienceContexts[0], nil
			}
			if len(issuerAudienceContexts) == 0 {
				logger.WithNamespace("oidc").Warnf(
					"Cannot resolve logout context from issuer/audience: iss=%s aud=%v",
					iss, aud,
				)
			} else {
				logger.WithNamespace("oidc").Warnf(
					"Ambiguous logout context from issuer/audience: iss=%s aud=%v candidates=%v",
					iss, aud, issuerAudienceContexts,
				)
			}
			return "", ErrAmbiguousLogout
		}
	// if there is only one session in redis, return it as is
	case 1:
		{
			logger.WithNamespace("oidc").Debugf(
				"Resolved logout context %s from sid binding only",
				sidContexts[0],
			)
			return sidContexts[0], nil
		}
	default:
		{
			candidates := combineLogoutContextCandidates(sidContexts, issuerAudienceContexts)
			if len(candidates) == 1 {
				logger.WithNamespace("oidc").Debugf(
					"Resolved logout context %s from sid and issuer/audience candidates",
					candidates[0],
				)
				return candidates[0], nil
			}
			if len(candidates) == 0 {
				logger.WithNamespace("oidc").Warnf(
					"Logout context candidates do not intersect: sid=%s sid_contexts=%v iss=%s aud=%v issuer_audience_contexts=%v",
					sid, sidContexts, iss, aud, issuerAudienceContexts,
				)
			} else {
				logger.WithNamespace("oidc").Warnf(
					"Ambiguous logout context after combining sid and issuer/audience candidates: sid=%s candidates=%v iss=%s aud=%v",
					sid, candidates, iss, aud,
				)
			}
			return "", ErrAmbiguousLogout
		}
	}
}

func combineLogoutContextCandidates(sidContexts, issuerAudienceContexts []string) []string {
	if len(sidContexts) == 0 {
		return issuerAudienceContexts
	}
	if len(issuerAudienceContexts) == 0 {
		return sidContexts
	}
	allowed := make(map[string]struct{}, len(issuerAudienceContexts))
	for _, contextName := range issuerAudienceContexts {
		allowed[contextName] = struct{}{}
	}
	combined := make([]string, 0, len(sidContexts))
	for _, contextName := range sidContexts {
		if _, ok := allowed[contextName]; ok {
			combined = append(combined, contextName)
		}
	}
	return combined
}

func findOIDCContextsByIssuerAudience(issuer string, audience []string) []string {
	if issuer == "" || len(audience) == 0 {
		logger.WithNamespace("oidc").Debugf(
			"Cannot resolve OIDC contexts by issuer/audience: iss=%s aud=%v",
			issuer, audience,
		)
		return nil
	}

	contexts := make([]string, 0)
	for contextName := range config.GetConfig().Authentication {
		conf, err := oidcprovider.LoadConfig(contextName, oidcprovider.RequireClientID)
		if err != nil {
			logger.WithNamespace("oidc").Debugf(
				"Skipping OIDC context %s during logout resolution: invalid config: %s",
				contextName, err,
			)
			continue
		}
		if conf.IDTokenKeyURL == "" {
			logger.WithNamespace("oidc").Debugf(
				"Skipping OIDC context %s during logout resolution: missing id_token_jwk_url",
				contextName,
			)
			continue
		}
		if !audienceContains(audience, conf.ClientID) {
			logger.WithNamespace("oidc").Debugf(
				"Skipping OIDC context %s during logout resolution: client_id %s not found in aud=%v",
				contextName, conf.ClientID, audience,
			)
			continue
		}
		expectedIssuer, err := oidcprovider.GetIssuer(contextName, conf)
		if err != nil {
			logger.WithNamespace("oidc").Debugf(
				"Skipping OIDC context %s during logout resolution: cannot resolve issuer: %s",
				contextName, err,
			)
			continue
		}
		if expectedIssuer != issuer {
			logger.WithNamespace("oidc").Debugf(
				"Skipping OIDC context %s during logout resolution: issuer mismatch expected=%s got=%s",
				contextName, expectedIssuer, issuer,
			)
			continue
		}
		logger.WithNamespace("oidc").Debugf(
			"Matched OIDC context %s for logout resolution: issuer=%s client_id=%s",
			contextName, issuer, conf.ClientID,
		)
		contexts = append(contexts, contextName)
	}
	sort.Strings(contexts)
	return contexts
}

func verifyLogoutTokenForContext(logoutToken, contextName string) (*logoutContext, error) {
	logger.WithNamespace("oidc").Debugf("Verifying logout token for context %s", contextName)
	conf, err := oidcprovider.LoadConfig(
		contextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireIDTokenKeyURL,
		oidcprovider.RequireIssuerOrTokenURL,
	)
	if err != nil {
		return nil, err
	}
	claims, err := oidcprovider.VerifyLogoutToken(logoutToken, contextName, conf)
	if err != nil {
		return nil, err
	}
	issuer, _ := claims.GetIssuer()
	aud, _ := claims.GetAudience()
	sid, _ := claims["sid"].(string)
	sub, _ := claims["sub"].(string)
	logger.WithNamespace("oidc").Debugf(
		"Verified logout token for context %s: iss=%s aud=%v sid=%s sub=%s",
		contextName, issuer, aud, sid, sub,
	)
	return &logoutContext{
		ContextName: contextName,
		Conf:        conf,
		Claims:      claims,
	}, nil
}

func applyLogoutForContext(verified logoutContext) error {
	sid, _ := verified.Claims["sid"].(string)
	logger.WithNamespace("oidc").Debugf(
		"Applying backchannel logout for context %s: sid_present=%t allow_custom_instance=%t",
		verified.ContextName, sid != "", verified.Conf.AllowCustomInstance,
	)
	if sid != "" {
		logger.WithNamespace("oidc").Debugf(
			"Trying bound OIDC logout fanout for context %s and sid %s",
			verified.ContextName, sid,
		)
		deletedSessions, err := session.DeleteByOIDCSession(verified.ContextName, sid)
		if err != nil {
			return err
		}
		deletedClients, err := oauth.DeleteByOIDCSession(verified.ContextName, sid)
		if err != nil {
			return err
		}
		if deletedSessions > 0 || deletedClients > 0 {
			return nil
		}
		logger.WithNamespace("oidc").Debugf(
			"No bound OIDC state found for context %s and sid %s, falling back to instance-based logout",
			verified.ContextName, sid,
		)
	}

	inst, err := resolveLogoutInstance(verified.ContextName, verified.Conf, verified.Claims)
	if err != nil {
		return err
	}
	logger.WithNamespace("oidc").Debugf(
		"Resolved fallback logout instance for context %s: domain=%s",
		verified.ContextName, inst.Domain,
	)
	if sid != "" {
		logger.WithNamespace("oidc").Debugf(
			"Deleting local sessions by sid for context %s on instance %s",
			verified.ContextName, inst.Domain,
		)
		return session.DeleteBySID(inst, sid)
	}
	logger.WithNamespace("oidc").Debugf(
		"Deleting all local sessions for context %s on instance %s because logout token does not contain sid",
		verified.ContextName, inst.Domain,
	)
	return session.DeleteOthers(inst, "all")
}

func resolveLogoutInstance(contextName string, conf *Config, claims jwt.MapClaims) (*instance.Instance, error) {
	logger.WithNamespace("oidc").Debugf(
		"Resolving logout instance for context %s using OIDC instance mapping",
		contextName,
	)
	return oidcprovider.ResolveInstanceFromClaims(contextName, conf, claims)
}

func audienceContains(audiences []string, clientID string) bool {
	for _, audience := range audiences {
		if audience == clientID {
			return true
		}
	}
	return false
}

// validateDelegatedCode validates a delegated code and returns the associated session ID.
// Returns an error if the code is invalid or doesn't match the instance.
func validateDelegatedCode(inst *instance.Instance, code string) (string, error) {
	codeData := getStorage().GetCodeData(code)
	if codeData == nil || codeData.Sub == "" {
		return "", errors.New("invalid code")
	}

	sub := codeData.Sub
	if sub != inst.OIDCID && sub != inst.FranceConnectID && sub != inst.Domain {
		inst.Logger().WithNamespace("oidc").Infof("AccessToken invalid code: %s (%s - %s - %s)",
			sub, inst.OIDCID, inst.FranceConnectID, inst.Domain)
		return "", errors.New("invalid code")
	}

	return codeData.SessionID, nil
}

// validateOIDCToken validates an OIDC token (id_token or access_token) for the instance.
func validateOIDCToken(inst *instance.Instance, idToken, oidcToken string) error {
	var err error
	if idToken != "" {
		conf, err := oidcprovider.LoadConfig(inst.ContextName, oidcprovider.RequireIDTokenKeyURL)
		if err != nil || !conf.AllowOAuthToken {
			return errors.New("this endpoint is not enabled")
		}
		err = checkIDToken(conf, inst, idToken)
	} else {
		conf, err := oidcprovider.LoadConfig(
			inst.ContextName,
			oidcprovider.RequireUserInfoURL,
			oidcprovider.RequireUserInfoMapping,
		)
		if err != nil || !conf.AllowOAuthToken {
			return errors.New("this endpoint is not enabled")
		}
		err = checkDomainFromUserInfo(conf, inst, oidcToken)
	}
	return err
}

// AccessToken delivers an access_token and a refresh_token if the client gives
// a valid token for OIDC.
func AccessToken(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	var reqBody struct {
		ClientID       string `json:"client_id"`
		ClientSecret   string `json:"client_secret"`
		Scope          string `json:"scope"`
		OIDCToken      string `json:"oidc_token"`
		IDToken        string `json:"id_token"`
		Code           string `json:"code"`
		TwoFactorToken string `json:"two_factor_token"`
		TwoFactorCode  string `json:"two_factor_passcode"`
	}
	if err := c.Bind(&reqBody); err != nil {
		return err
	}

	// Validate the delegated code or OIDC token
	var codeSessionID string
	if reqBody.Code != "" {
		sessionID, err := validateDelegatedCode(inst, reqBody.Code)
		if err != nil {
			return c.JSON(http.StatusBadRequest, echo.Map{
				"error": "invalid code",
			})
		}
		codeSessionID = sessionID
	} else {
		if err := validateOIDCToken(inst, reqBody.IDToken, reqBody.OIDCToken); err != nil {
			return c.JSON(http.StatusBadRequest, echo.Map{
				"error": err.Error(),
			})
		}
	}

	// Load the OAuth client
	client, err := oauth.FindClient(inst, reqBody.ClientID)
	if err != nil {
		if couchErr, isCouchErr := couchdb.IsCouchError(err); isCouchErr && couchErr.StatusCode >= 500 {
			return err
		}
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "the client must be registered",
		})
	}
	if subtle.ConstantTimeCompare([]byte(reqBody.ClientSecret), []byte(client.ClientSecret)) == 0 {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid client_secret",
		})
	}

	if inst.HasAuthMode(instance.TwoFactorMail) {
		token := []byte(reqBody.TwoFactorToken)
		if len(token) == 0 {
			twoFactorToken, err := lifecycle.SendTwoFactorPasscode(inst)
			if err != nil {
				return err
			}
			return c.JSON(http.StatusUnauthorized, echo.Map{
				"error":            "two factor needed",
				"two_factor_token": string(twoFactorToken),
			})
		}
		if ok := inst.ValidateTwoFactorPasscode(token, reqBody.TwoFactorCode); !ok {
			return c.JSON(http.StatusForbidden, echo.Map{
				"error": inst.Translate(auth.TwoFactorErrorKey),
			})
		}
	}

	// Prepare the scope
	out := auth.AccessTokenReponse{
		Type:  "bearer",
		Scope: reqBody.Scope,
	}
	if !client.Flagship {
		if slug := oauth.GetLinkedAppSlug(client.SoftwareID); slug != "" {
			if err := auth.CheckLinkedAppInstalled(inst, slug); err != nil {
				return err
			}
			out.Scope = oauth.BuildLinkedAppScope(slug)
		}
	}
	if out.Scope == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid scope",
		})
	}
	if out.Scope == "*" {
		if !client.Flagship {
			return auth.ReturnSessionCode(c, http.StatusAccepted, inst)
		}
	}

	// Store the session ID in the client for logout purposes (priority: delegated code > id_token)
	oldSessionID := client.OIDCSessionID
	sessionID := codeSessionID
	if sessionID == "" {
		sessionID = extractSessionID(reqBody.IDToken)
	}
	if sessionID != "" {
		client.OIDCSessionID = sessionID
		logger.WithNamespace("oidc").Debugf("Using session ID for OIDC logout: %s", sessionID)
	}

	// Update the OAuth client if needed (pending flag or new session ID)
	needsUpdate := client.Pending || sessionID != ""
	if needsUpdate {
		client.Pending = false
		client.ClientID = ""
		if err := couchdb.UpdateDoc(inst, client); err != nil {
			inst.Logger().WithNamespace("oidc").Warnf("Cannot update OAuth client %s: %s", client.CouchID, err)
		} else if sessionID != "" {
			if oldSessionID != "" && oldSessionID != sessionID {
				inst.Logger().WithNamespace("oidc").Debugf(
					"Replacing OIDC session binding for OAuth client %s: old_sid=%s new_sid=%s",
					client.CouchID, oldSessionID, sessionID,
				)
				if err := oidcbinding.UnbindOAuthClient(inst.ContextName, inst.Domain, oldSessionID, client.CouchID); err != nil {
					inst.Logger().WithNamespace("oidc").Warnf("Cannot unbind OIDC session %s from OAuth client %s: %s", oldSessionID, client.CouchID, err)
				}
			}
			if err := oidcbinding.BindOAuthClient(inst.ContextName, inst.Domain, sessionID, client.CouchID); err != nil {
				inst.Logger().WithNamespace("oidc").Warnf("Cannot bind OIDC session %s to OAuth client %s: %s", sessionID, client.CouchID, err)
			}
		}
		client.ClientID = client.CouchID
	}

	// Generate the access/refresh tokens
	accessToken, err := client.CreateJWT(inst, consts.AccessTokenAudience, out.Scope)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate access token",
		})
	}
	out.Access = accessToken
	refreshToken, err := client.CreateJWT(inst, consts.RefreshTokenAudience, out.Scope)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate refresh token",
		})
	}
	out.Refresh = refreshToken

	return c.JSON(http.StatusOK, out)
}

// Config is the config to log in a user with an OpenID Connect identity provider.
type Config = oidcprovider.Config

func getPublicOIDCConfig() (*Config, string, error) {
	// Get public OIDC context name from default context
	contextName := config.GetPublicOIDCContext(config.DefaultInstanceContext)
	if contextName == "" {
		return nil, "", errors.New("No public OIDC context is configured")
	}
	conf, err := oidcprovider.LoadConfig(
		contextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireScope,
		oidcprovider.RequireRedirectURI,
		oidcprovider.RequireAuthorizeURL,
	)
	if err != nil {
		return nil, "", err
	}
	return conf, contextName, nil
}

func getLoginHint(inst *instance.Instance) string {
	if inst == nil {
		return ""
	}
	if inst.OrgDomain != "" {
		if email, err := inst.SettingsEMail(); err == nil && email != "" {
			return email
		}
	}
	if inst.OldDomain != "" {
		return inst.OldDomain
	}
	if inst.OIDCID != "" {
		return inst.OIDCID
	}
	return ""
}

func makeStartURL(inst *instance.Instance, domain, redirect, confirm, oidcContext string, conf *Config) (string, error) {
	u, err := url.Parse(conf.AuthorizeURL)
	if err != nil {
		return "", err
	}
	state := newStateHolder(domain, redirect, confirm, oidcContext, conf.Provider)
	if err = getStorage().Add(state); err != nil {
		return "", err
	}
	vv := u.Query()
	vv.Add("response_type", "code")
	vv.Add("scope", conf.Scope)
	vv.Add("client_id", conf.ClientID)
	vv.Add("redirect_uri", conf.RedirectURI)
	vv.Add("state", state.id)
	vv.Add("nonce", state.Nonce)
	loginHint := getLoginHint(inst)
	if loginHint != "" {
		vv.Add("login_hint", loginHint)
	}
	if conf.Provider == oidcprovider.FranceConnectProvider {
		vv.Add("acr_values", "eidas1")
	}
	u.RawQuery = vv.Encode()
	return u.String(), nil
}

func makeSharingStartURL(inst *instance.Instance, sharingID, sharingState string, conf *Config, contextName string) (string, error) {
	return makeSharingStartURLWithLoginHint(inst, sharingID, sharingState, conf, contextName, getLoginHint(inst))
}

func makeSharingStartURLWithLoginHint(inst *instance.Instance, sharingID, sharingState string, conf *Config, contextName, loginHint string) (string, error) {
	u, err := url.Parse(conf.AuthorizeURL)
	if err != nil {
		return "", err
	}
	state := newSharingStateHolder(inst.Domain, sharingID, sharingState, conf.Provider, contextName)
	if err = getStorage().Add(state); err != nil {
		return "", err
	}
	vv := u.Query()
	vv.Add("response_type", "code")
	vv.Add("scope", conf.Scope)
	vv.Add("client_id", conf.ClientID)
	vv.Add("redirect_uri", conf.RedirectURI)
	vv.Add("state", state.id)
	vv.Add("nonce", state.Nonce)
	if loginHint != "" {
		vv.Add("login_hint", loginHint)
	}
	if conf.Provider == oidcprovider.FranceConnectProvider {
		vv.Add("acr_values", "eidas1")
	}
	u.RawQuery = vv.Encode()
	return u.String(), nil
}

var oidcClient = &http.Client{
	Timeout: 15 * time.Second,
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
}

func getToken(conf *Config, code string) (*tokenResponse, error) {
	data := url.Values{
		"grant_type":   []string{"authorization_code"},
		"code":         []string{code},
		"redirect_uri": []string{conf.RedirectURI},
	}

	// FranceConnect expects the client_id+client_secret in the body, not in a
	// Authentication header like normal OIDC.
	if conf.Provider == oidcprovider.FranceConnectProvider {
		data.Add("client_id", conf.ClientID)
		data.Add("client_secret", conf.ClientSecret)
	}

	body := strings.NewReader(data.Encode())
	req, err := http.NewRequest("POST", conf.TokenURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Add(echo.HeaderContentType, echo.MIMEApplicationForm)
	req.Header.Add(echo.HeaderAccept, echo.MIMEApplicationJSON)

	if conf.Provider == oidcprovider.GenericProvider {
		auth := []byte(conf.ClientID + ":" + conf.ClientSecret)
		req.Header.Add(echo.HeaderAuthorization, "Basic "+base64.StdEncoding.EncodeToString(auth))
	}

	res, err := oidcClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		// Read the error response body to log detailed error information
		errBody, _ := io.ReadAll(res.Body)
		logger.WithNamespace("oidc").
			Errorf("Token request failed: status=%d url=%s redirect_uri=%s response=%s",
				res.StatusCode, conf.TokenURL, conf.RedirectURI, string(errBody))
		return nil, fmt.Errorf("OIDC service responded with %d", res.StatusCode)
	}
	resBody, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}

	var out tokenResponse
	err = json.Unmarshal(resBody, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func getDomainFromUserInfo(conf *Config, token string) (string, error) {
	if conf.AllowCustomInstance {
		return "", ErrInvalidConfiguration
	}
	params, err := getUserInfo(conf, token)
	if err != nil {
		return "", err
	}
	return oidcprovider.ExtractDomainFromClaims(conf, params)
}

func checkDomainFromUserInfo(conf *Config, inst *instance.Instance, token string) error {
	params, err := getUserInfo(conf, token)
	if err != nil {
		return err
	}

	err = oidcprovider.CheckClaimsForInstance(conf, inst, params)
	if err != nil && conf.Provider == oidcprovider.FranceConnectProvider {
		if errors.Is(err, ErrAuthenticationFailed) {
			return ErrFranceConnectFailed
		}
		var mismatch *oidcprovider.InstanceMismatchError
		if errors.As(err, &mismatch) {
			return ErrFranceConnectFailed
		}
	}
	return err
}

func getUserInfo(conf *Config, token string) (map[string]interface{}, error) {
	req, err := http.NewRequest("GET", conf.UserInfoURL, nil)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+token)
	res, err := oidcClient.Do(req)
	if err != nil {
		logger.WithNamespace("oidc").Errorf("Error on getDomainFromUserInfo: %s", err)
		return nil, ErrIdentityProvider
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		// Flush the body, so that the connecion can be reused by keep-alive
		_, _ = io.Copy(io.Discard, res.Body)
		logger.WithNamespace("oidc").
			Infof("Invalid status code %d for %s", res.StatusCode, conf.UserInfoURL)
		return nil, fmt.Errorf("OIDC service responded with %d", res.StatusCode)
	}

	var params map[string]interface{}
	err = json.NewDecoder(res.Body).Decode(&params)
	if err != nil {
		logger.WithNamespace("oidc").Errorf("Error on getDomainFromUserInfo: %s", err)
		return nil, ErrIdentityProvider
	}
	return params, nil
}

func checkIDToken(conf *Config, inst *instance.Instance, idToken string) error {
	claims, err := oidcprovider.VerifyIDToken(idToken, conf)
	if err != nil {
		logger.WithNamespace("oidc").Errorf("Error on VerifyIDToken: %s", err)
		return ErrInvalidToken
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		inst.Logger().WithNamespace("oidc").Errorf("Invalid sub: %v != %s", claims["sub"], inst.OIDCID)
		return ErrAuthenticationFailed
	}
	if sub != inst.OIDCID {
		inst.Logger().WithNamespace("oidc").Errorf("Invalid sub: %s != %s", sub, inst.OIDCID)
		return &DomainMismatchError{ExpectedDomain: inst.Domain, ActualDomain: sub}
	}

	return nil
}

func logOIDCSessionExpired(c echo.Context, route, reason string, inst *instance.Instance, stateID string) {
	log := logger.WithNamespace("oidc")
	if inst != nil {
		log = inst.Logger().WithNamespace("oidc")
	}
	sessionState := c.QueryParam("session_state")
	log.Warnf(
		"OIDC session expired error rendered: route=%s reason=%s state_present=%t state_len=%d state_storage=%s code_present=%t access_token_present=%t id_token_present=%t session_state_present=%t session_state_len=%d iss_present=%t redirect_present=%t confirm_present=%t",
		route,
		reason,
		stateID != "",
		len(stateID),
		oidcStateStorageKind(),
		c.QueryParam("code") != "",
		c.QueryParam("access_token") != "",
		c.QueryParam("id_token") != "",
		sessionState != "",
		len(sessionState),
		c.QueryParam("iss") != "",
		c.QueryParam("redirect") != "",
		c.QueryParam("confirm_state") != "",
	)
}

func oidcStateStorageKind() string {
	if config.GetConfig().OauthStateStorage == nil {
		return "memory"
	}
	return "redis"
}

func renderError(c echo.Context, inst *instance.Instance, code int, msg string, extras ...map[string]interface{}) error {
	if inst == nil {
		inst = &instance.Instance{
			Domain:      c.Request().Host,
			ContextName: config.DefaultInstanceContext,
			Locale:      consts.DefaultLocale,
		}
	}
	params := echo.Map{
		"Domain":         inst.ContextualDomain(),
		"ContextName":    inst.ContextName,
		"Locale":         inst.Locale,
		"Title":          inst.TemplateTitle(),
		"Favicon":        middlewares.Favicon(inst),
		"Illustration":   "/images/generic-error.svg",
		"Error":          msg,
		"SupportEmail":   inst.SupportEmailAddress(),
		"SupportPageURL": inst.SupportPageURL(),
	}
	// Merge any extra params (Button, ButtonURL, etc.)
	for _, extra := range extras {
		for k, v := range extra {
			params[k] = v
		}
	}
	return c.Render(code, "error.html", params)
}

func getOIDCLogoutURL(contextName string) string {
	a := config.GetConfig().Authentication
	delegated, _ := a[contextName].(map[string]interface{})
	oidc, _ := delegated["oidc"].(map[string]interface{})
	u, _ := oidc["logout_url"].(string)
	return u
}

// Routes setup routing for OpenID Connect routes.
// Careful, the normal middlewares NeedInstance and LoadSession are not applied
// to this group in web/routing
func Routes(router *echo.Group) {
	router.GET("/start", Start, middlewares.NeedInstance, middlewares.CheckOnboardingNotFinished)
	router.GET("/franceconnect", StartFranceConnect, middlewares.NeedInstance, middlewares.CheckOnboardingNotFinished)
	router.GET("/sharing", Sharing, middlewares.NeedInstance)
	router.GET("/sharing/public", SharingPublic, middlewares.NeedInstance)
	router.GET("/bitwarden/:context", BitwardenStart)
	router.POST("/bitwarden/:context", BitwardenExchange, middlewares.NeedInstance)
	router.GET("/redirect", Redirect)
	router.GET("/login", Login, middlewares.NeedInstance)
	router.POST("/twofactor", TwoFactor, middlewares.NeedInstance)
	router.POST("/logout", Logout)
	router.POST("/:context/logout", Logout)
	router.POST("/access_token", AccessToken, middlewares.NeedInstance)
}

// GetDelegatedCode is mostly a proxy for the userinfo request made by the
// cloudery to the OIDC provider. It adds a delegated code in the response
// associated to the sub. If an id_token is provided, the session ID (sid) is
// extracted and stored with the delegated code for later use in OIDC logout.
func GetDelegatedCode(c echo.Context) error {
	contextName := c.Param("context")
	provider := c.Param("provider")
	var conf *Config
	var err error
	if provider == "franceconnect" {
		conf, err = oidcprovider.LoadFranceConnectConfig(
			contextName,
			oidcprovider.RequireUserInfoURL,
			oidcprovider.RequireUserInfoMapping,
		)
	} else {
		conf, err = oidcprovider.LoadConfig(
			contextName,
			oidcprovider.RequireUserInfoURL,
			oidcprovider.RequireUserInfoMapping,
		)
	}
	if err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": err.Error(),
		})
	}

	var reqBody struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
	}
	if err := c.Bind(&reqBody); err != nil {
		return err
	}

	params, err := getUserInfo(conf, reqBody.AccessToken)
	if err != nil {
		return err
	}

	var s string
	if conf.AllowCustomInstance {
		sub, ok := params["sub"].(string)
		if !ok {
			logger.WithNamespace("oidc").Errorf("Missing sub")
			return ErrAuthenticationFailed
		}
		s = sub
	} else {
		domain, err := oidcprovider.ExtractDomainFromClaims(conf, params)
		if err != nil {
			logger.WithNamespace("oidc").Warnf("Cannot extract domain: %s", err)
			return err
		}
		s = domain
	}

	// Extract session ID (sid) from id_token if provided
	sessionID := extractSessionID(reqBody.IDToken)

	logger.WithNamespace("oidc").Infof("GetDelegatedCode for %s", s)
	params["delegated_code"] = getStorage().CreateCodeData(s, sessionID)
	return c.JSON(http.StatusOK, params)
}

// AdminRoutes setup the routing for OpenID Connect on the admin port. It is
// mostly used by the cloudery.
func AdminRoutes(router *echo.Group) {
	router.POST("/:context/:provider/code", GetDelegatedCode)
}

// LoginDomainHandler is the handler for the requests on the login domain. It
// shows a page with a login button (that can start the OIDC dance).
func LoginDomainHandler(c echo.Context, contextName string) error {
	r := c.Request()
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		rndr, err := statik.NewRenderer()
		if err != nil {
			return err
		}
		rndr.ServeHTTP(c.Response(), r)
		return nil
	}

	if r.Method != http.MethodPost {
		i := &instance.Instance{Locale: "fr", ContextName: contextName}
		title := i.Translate("Login Welcome")
		return c.Render(http.StatusOK, "oidc_login.html", echo.Map{
			"Domain":      i.ContextualDomain(),
			"ContextName": i.ContextName,
			"Locale":      i.Locale,
			"Title":       title,
			"Favicon":     middlewares.Favicon(i),
		})
	}

	conf, err := oidcprovider.LoadConfig(
		contextName,
		oidcprovider.RequireClientID,
		oidcprovider.RequireScope,
		oidcprovider.RequireRedirectURI,
		oidcprovider.RequireAuthorizeURL,
	)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the context was not found.")
	}
	u, err := makeStartURL(nil, r.Host, "", "", "", conf)
	if err != nil {
		return renderError(c, nil, http.StatusNotFound, "Sorry, the server is not configured for OpenID Connect.")
	}
	return c.Redirect(http.StatusSeeOther, u)
}

// FindLoginDomain returns the context name for which the login domain matches
// the host.
func FindLoginDomain(host string) (string, bool) {
	for ctx, auth := range config.GetConfig().Authentication {
		delegated, ok := auth.(map[string]interface{})
		if !ok {
			continue
		}
		oidc, ok := delegated["oidc"].(map[string]interface{})
		if !ok {
			continue
		}
		domain, ok := oidc["login_domain"].(string)
		if ok && domain == host {
			return ctx, true
		}
	}
	return "", false
}
