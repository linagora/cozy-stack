// Package auth provides register and login handlers
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/session"
	csettings "github.com/cozy/cozy-stack/model/settings"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

const (
	// CredentialsErrorKey is the key for translating the message showed to the
	// user when he/she enters incorrect credentials
	CredentialsErrorKey = "Login Credentials error"
	// TwoFactorErrorKey is the key for translating the message showed to the
	// user when he/she enters incorrect two factor secret
	TwoFactorErrorKey = "Login Two factor error"
	// TwoFactorExceededErrorKey is the key for translating the message showed to the
	// user when there were too many attempts
	TwoFactorExceededErrorKey = "Login Two factor attempts error"
)

func wantsJSON(c echo.Context) bool {
	return c.Request().Header.Get(echo.HeaderAccept) == echo.MIMEApplicationJSON
}

func renderError(c echo.Context, code int, msg string) error {
	return renderErrorWithTitle(c, code, "", msg)
}

func renderErrorWithTitle(c echo.Context, code int, title, msg string) error {
	i := middlewares.GetInstance(c)
	data := echo.Map{
		"Domain":         i.ContextualDomain(),
		"ContextName":    i.ContextName,
		"Locale":         i.Locale,
		"Title":          i.TemplateTitle(),
		"Favicon":        middlewares.Favicon(i),
		"Illustration":   "/images/generic-error.svg",
		"ErrorTitle":     title,
		"Error":          msg,
		"SupportEmail":   i.SupportEmailAddress(),
		"SupportPageURL": i.SupportPageURL(),
	}
	return c.Render(code, "error.html", data)
}

// Home is the handler for /
// It redirects to the login page is the user is not yet authentified
// Else, it redirects to its home application (or onboarding)
func Home(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	if len(instance.RegisterToken) > 0 && !instance.OnboardingFinished {
		if !middlewares.CheckRegisterToken(c, instance) {
			return middlewares.RenderNeedOnboarding(c, instance)
		}
		return c.Redirect(http.StatusSeeOther, instance.PageURL("/auth/passphrase", c.QueryParams()))
	}

	if middlewares.IsLoggedIn(c) {
		redirect := instance.DefaultRedirection()
		return c.Redirect(http.StatusSeeOther, redirect.String())
	}

	// Onboarding to a specific app when authentication via OIDC is enabled
	redirection := c.QueryParam("redirection")
	if redirection != "" && instance.HasForcedOIDC() {
		splits := strings.SplitN(redirection, "#", 2)
		parts := strings.SplitN(splits[0], "/", 2)
		if _, err := app.GetWebappBySlug(instance, parts[0]); err == nil {
			u := instance.SubDomain(parts[0])
			if len(parts) == 2 {
				u.Path = parts[1]
			}
			if len(splits) == 2 {
				u.Fragment = splits[1]
			}
			q := url.Values{"redirect": {u.String()}}
			return c.Redirect(http.StatusSeeOther, instance.PageURL("/oidc/start", q))
		}
	}

	params := make(url.Values)
	if jwt := c.QueryParam("jwt"); jwt != "" {
		params.Add("jwt", jwt)
	}
	if code := c.QueryParam("email_verified_code"); code != "" {
		params.Add("email_verified_code", code)
	}
	return c.Redirect(http.StatusSeeOther, instance.PageURL("/auth/login", params))
}

// SetCookieForNewSession creates a new session and sets the cookie on echo context
func SetCookieForNewSession(c echo.Context, duration session.Duration, sid string) (string, error) {
	instance := middlewares.GetInstance(c)
	session, err := session.New(instance, duration, sid)
	if err != nil {
		return "", err
	}
	cookie, err := session.ToCookie()
	if err != nil {
		return "", err
	}
	c.SetCookie(cookie)
	return session.ID(), nil
}

// isTrustedDevice checks if a device of an instance is trusted
func isTrustedDevice(c echo.Context, inst *instance.Instance) bool {
	trustedDeviceToken := []byte(c.FormValue("trusted-device-token"))
	return inst.ValidateTwoFactorTrustedDeviceSecret(c.Request(), trustedDeviceToken)
}

// hasEmailVerified checks if the email has already been verified, and if it is
// the case, the stack can skip the 2FA by email.
func hasEmailVerified(c echo.Context, inst *instance.Instance) bool {
	code := c.FormValue("email_verified_code")
	return inst.CheckEmailVerifiedCode(code)
}

func getLogoutURL(context string) string {
	auth := config.GetConfig().Authentication
	delegated, _ := auth[context].(map[string]interface{})
	oidc, _ := delegated["oidc"].(map[string]interface{})
	u, _ := oidc["logout_url"].(string)
	return u
}

func redirectOIDC(c echo.Context, inst *instance.Instance) error {
	if u := getLogoutURL(inst.ContextName); u != "" {
		cookie, err := c.Cookie("logout")
		if err == nil && cookie.Value == "1" {
			c.SetCookie(&http.Cookie{
				Name:   "logout",
				Value:  "",
				MaxAge: -1,
				Domain: session.CookieDomain(inst),
			})
			return c.Redirect(http.StatusSeeOther, u)
		}
	}

	var q url.Values
	if redirect := c.QueryParam("redirect"); redirect != "" {
		q = url.Values{"redirect": {redirect}}
	}
	return c.Redirect(http.StatusSeeOther, inst.PageURL("/oidc/start", q))
}

func renderLoginForm(c echo.Context, i *instance.Instance, code int, credsErrors string, redirect *url.URL) error {
	if i.HasForcedOIDC() {
		return redirectOIDC(c, i)
	}
	hasFranceConnect := i.FranceConnectID != ""

	publicName, err := csettings.PublicName(i)
	if err != nil {
		publicName = ""
	}

	var redirectStr string
	var hasOAuth, hasSharing bool
	if redirect != nil {
		redirectStr = redirect.String()
		hasOAuth = hasRedirectToAuthorize(i, redirect)
		hasSharing = hasRedirectToAuthorizeSharing(i, redirect)
	}

	var title, help string
	if c.QueryParam("msg") == "passphrase-reset-requested" {
		title = i.Translate("Login Connect after reset requested title")
		help = i.Translate("Login Connect after reset requested help")
	} else if strings.Contains(redirectStr, "reconnect") {
		title = i.Translate("Login Reconnect title")
		help = i.Translate("Login Reconnect help")
	} else if hasSharing {
		title = i.Translate("Login Connect from sharing title", publicName)
		help = i.Translate("Login Connect from sharing help")
	} else {
		if publicName == "" {
			title = i.Translate("Login Welcome")
		} else {
			title = i.Translate("Login Welcome name", publicName)
		}
		help = i.Translate("Login Password help")
	}

	iterations := 0
	if settings, err := settings.Get(i); err == nil {
		iterations = settings.PassphraseKdfIterations
	}

	// When we have an email_verified_code, we need to ask the user their
	// password, not send them an email with a magic link
	emailVerifiedCode := c.QueryParam("email_verified_code")
	magicLink := i.MagicLink
	if emailVerifiedCode != "" {
		magicLink = false
	}

	dataProxyCleanURL := i.DataProxyCleanURL()
	csp := c.Response().Header().Get(echo.HeaderContentSecurityPolicy)
	csp = strings.Replace(csp, "frame-src 'none'", "frame-src "+dataProxyCleanURL+" ", 1)
	c.Response().Header().Set(echo.HeaderContentSecurityPolicy, csp)

	return c.Render(code, "login.html", echo.Map{
		"TemplateTitle":     i.TemplateTitle(),
		"Domain":            i.ContextualDomain(),
		"ContextName":       i.ContextName,
		"Locale":            i.Locale,
		"Favicon":           middlewares.Favicon(i),
		"CryptoPolyfill":    middlewares.CryptoPolyfill(c),
		"BottomNavBar":      middlewares.BottomNavigationBar(c),
		"Iterations":        iterations,
		"Salt":              string(i.PassphraseSalt()),
		"Title":             title,
		"PasswordHelp":      help,
		"CredentialsError":  credsErrors,
		"Redirect":          redirectStr,
		"CSRF":              c.Get("csrf"),
		"EmailVerifiedCode": emailVerifiedCode,
		"MagicLink":         magicLink,
		"OAuth":             hasOAuth,
		"FranceConnect":     hasFranceConnect,
		"DataProxyCleanURL": dataProxyCleanURL,
	})
}

func loginForm(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	redirect, err := checkRedirectParam(c, nil)
	if err != nil {
		return err
	}

	if middlewares.IsLoggedIn(c) {
		if redirect == nil {
			redirect = instance.DefaultRedirection()
		}
		return c.Redirect(http.StatusSeeOther, redirect.String())
	}
	// Delegated JWT
	if token := c.QueryParam("jwt"); token != "" {
		err := session.CheckDelegatedJWT(instance, token)
		if err != nil {
			instance.Logger().Warnf("Delegated token check failed: %s", err)
		} else {
			sessionID, err := SetCookieForNewSession(c, session.NormalRun, "")
			if err != nil {
				return err
			}
			if err = session.StoreNewLoginEntry(instance, sessionID, "", c.Request(), "JWT"); err != nil {
				instance.Logger().Errorf("Could not store session history %q: %s", sessionID, err)
			}
			if redirect == nil {
				redirect = instance.DefaultRedirection()
			}
			return c.Redirect(http.StatusSeeOther, redirect.String())
		}
	}
	return renderLoginForm(c, instance, http.StatusOK, "", redirect)
}

// newSession generates a new session, and puts a cookie for it
func newSession(c echo.Context, inst *instance.Instance, redirect *url.URL, duration session.Duration, logMessage string) error {
	var clientID string
	if hasRedirectToAuthorize(inst, redirect) {
		// NOTE: the login scope is used by external clients for authentication.
		// Typically, these clients are used for internal purposes, like
		// authenticating to an external system via the cozy. For these clients
		// we do not push a "client" notification, we only store a new login
		// history.
		clientID = redirect.Query().Get("client_id")
		duration = session.ShortRun
	}

	sessionID, err := SetCookieForNewSession(c, duration, "")
	if err != nil {
		return err
	}

	if err = session.StoreNewLoginEntry(inst, sessionID, clientID, c.Request(), logMessage); err != nil {
		inst.Logger().Errorf("Could not store session history %q: %s", sessionID, err)
	}

	return nil
}

func migrateToHashedPassphrase(inst *instance.Instance, settings *settings.Settings, passphrase []byte, iterations int) {
	salt := inst.PassphraseSalt()
	pass, masterKey := crypto.HashPassWithPBKDF2(passphrase, salt, iterations)
	hash, err := crypto.GenerateFromPassphrase(pass)
	if err != nil {
		inst.Logger().Errorf("Could not hash the passphrase: %s", err.Error())
		return
	}
	inst.PassphraseHash = hash
	settings.PassphraseKdfIterations = iterations
	settings.PassphraseKdf = instance.PBKDF2_SHA256
	settings.SecurityStamp = lifecycle.NewSecurityStamp()
	key, encKey, err := lifecycle.CreatePassphraseKey(masterKey)
	if err != nil {
		inst.Logger().Errorf("Could not create passphrase key: %s", err.Error())
		return
	}
	settings.Key = key
	pubKey, privKey, err := lifecycle.CreateKeyPair(encKey)
	if err != nil {
		inst.Logger().Errorf("Could not create key pair: %s", err.Error())
		return
	}
	settings.PublicKey = pubKey
	settings.PrivateKey = privKey
	if err := instance.Update(inst); err != nil {
		inst.Logger().Errorf("Could not update: %s", err.Error())
	}
	if err := settings.Save(inst); err != nil {
		inst.Logger().Errorf("Could not update: %s", err.Error())
	}
}

func login(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	redirect, err := checkRedirectParam(c, inst.DefaultRedirection())
	if err != nil {
		return err
	}

	passphrase := []byte(c.FormValue("passphrase"))
	longRunSession, _ := strconv.ParseBool(c.FormValue("long-run-session"))

	var sessionID string
	sess, ok := middlewares.GetSession(c)
	if ok { // The user was already logged-in
		sessionID = sess.ID()
	} else if instance.CheckPassphrase(inst, passphrase) == nil {
		iterations := crypto.DefaultPBKDF2Iterations
		settings, err := settings.Get(inst)
		// If the passphrase was not yet hashed on the client side, migrate it
		if err == nil && settings.PassphraseKdfIterations == 0 {
			migrateToHashedPassphrase(inst, settings, passphrase, iterations)
		}

		// In case the second factor authentication mode is "mail", we also
		// check that the mail has been confirmed. If not, 2FA is not
		// activated.
		// If device is trusted, skip the 2FA.
		// If the email has already been verified, skip the 2FA too.
		if inst.HasAuthMode(instance.TwoFactorMail) && !isTrustedDevice(c, inst) && !hasEmailVerified(c, inst) {
			twoFactorToken, err := lifecycle.SendTwoFactorPasscode(inst)
			if err != nil {
				return err
			}
			v := url.Values{}
			v.Add("two_factor_token", string(twoFactorToken))
			v.Add("long_run_session", strconv.FormatBool(longRunSession))
			if loc := c.FormValue("redirect"); loc != "" {
				v.Add("redirect", loc)
			}

			if wantsJSON(c) {
				return c.JSON(http.StatusOK, echo.Map{
					"redirect": inst.PageURL("/auth/twofactor", v),
				})
			}
			return c.Redirect(http.StatusSeeOther, inst.PageURL("/auth/twofactor", v))
		}
	} else { // Bad login passphrase
		errorMessage := inst.Translate(CredentialsErrorKey)
		err := config.GetRateLimiter().CheckRateLimit(inst, limits.AuthType)
		if limits.IsLimitReachedOrExceeded(err) {
			if err = LoginRateExceeded(inst); err != nil {
				inst.Logger().WithNamespace("auth").Warn(err.Error())
			}
		}
		if wantsJSON(c) {
			return c.JSON(http.StatusUnauthorized, echo.Map{
				"error": errorMessage,
			})
		}
		return renderLoginForm(c, inst, http.StatusUnauthorized, errorMessage, redirect)
	}

	// Successful authentication
	// User is now logged-in, generate a new session
	if sessionID == "" {
		duration := session.NormalRun
		if longRunSession {
			duration = session.LongRun
		}
		err := newSession(c, inst, redirect, duration, "password")
		if err != nil {
			return err
		}
	}
	if wantsJSON(c) {
		return c.JSON(http.StatusOK, echo.Map{
			"redirect": redirect.String(),
		})
	}

	return c.Redirect(http.StatusSeeOther, redirect.String())
}

// addLogoutCookie adds a cookie for logged-out users on instances in a context
// where OIDC is configured. It allows to redirects the user on the next request
// to a special page instead of sending them to the OIDC page (which can logs
// in the user again automatically).
func addLogoutCookie(c echo.Context, inst *instance.Instance) {
	if u := getLogoutURL(inst.ContextName); u == "" {
		return
	}
	c.SetCookie(&http.Cookie{
		Name:     "logout",
		Value:    "1",
		MaxAge:   10,
		Domain:   session.CookieDomain(inst),
		Secure:   !build.IsDevRelease(),
		HttpOnly: true,
	})
}

func logout(c echo.Context) error {
	res := c.Response()
	origin := c.Request().Header.Get(echo.HeaderOrigin)
	res.Header().Set(echo.HeaderAccessControlAllowOrigin, origin)
	res.Header().Set(echo.HeaderAccessControlAllowCredentials, "true")

	inst := middlewares.GetInstance(c)
	if !middlewares.AllowLogout(c) {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "The user can logout only from client-side apps",
		})
	}

	session, ok := middlewares.GetSession(c)
	if ok {
		c.SetCookie(session.Delete(inst))
	}

	addLogoutCookie(c, inst)

	return c.NoContent(http.StatusNoContent)
}

func logoutOthers(c echo.Context) error {
	res := c.Response()
	origin := c.Request().Header.Get(echo.HeaderOrigin)
	res.Header().Set(echo.HeaderAccessControlAllowOrigin, origin)
	res.Header().Set(echo.HeaderAccessControlAllowCredentials, "true")

	instance := middlewares.GetInstance(c)
	if !middlewares.AllowLogout(c) {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "The user can logout only from client-side apps",
		})
	}

	sess, ok := middlewares.GetSession(c)
	if !ok {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "Could not retrieve session",
		})
	}
	if err := session.DeleteOthers(instance, sess.ID()); err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

func corsPreflight(method string) echo.HandlerFunc {
	return func(c echo.Context) error {
		req := c.Request()
		res := c.Response()
		origin := req.Header.Get(echo.HeaderOrigin)

		res.Header().Add(echo.HeaderVary, echo.HeaderOrigin)
		res.Header().Add(echo.HeaderVary, echo.HeaderAccessControlRequestMethod)
		res.Header().Add(echo.HeaderVary, echo.HeaderAccessControlRequestHeaders)
		res.Header().Set(echo.HeaderAccessControlAllowOrigin, origin)
		res.Header().Set(echo.HeaderAccessControlAllowMethods, method)
		res.Header().Set(echo.HeaderAccessControlAllowCredentials, "true")
		res.Header().Set(echo.HeaderAccessControlMaxAge, middlewares.MaxAgeCORS)
		if h := req.Header.Get(echo.HeaderAccessControlRequestHeaders); h != "" {
			res.Header().Set(echo.HeaderAccessControlAllowHeaders, h)
		}

		return c.NoContent(http.StatusNoContent)
	}
}

func logoutPreflight(c echo.Context) error {
	return corsPreflight(echo.DELETE)(c)
}

func registerPreflight(c echo.Context) error {
	return corsPreflight(echo.POST)(c)
}

func tokenExchangeCORS(c echo.Context) bool {
	origin := c.Request().Header.Get(echo.HeaderOrigin)
	if origin == "" {
		return true
	}
	inst := middlewares.GetInstance(c)
	if !tokenExchangeOriginAllowed(origin, inst) {
		return false
	}

	res := c.Response()
	res.Header().Add(echo.HeaderVary, echo.HeaderOrigin)
	res.Header().Set(echo.HeaderAccessControlAllowOrigin, origin)
	res.Header().Set(echo.HeaderAccessControlAllowCredentials, "true")
	return true
}

// Allow CORS from *.org_domain, from Cozy app subdomains of the instance, from
// org-wide app domains resolved via client_url_flag, and from the owner's SaaS
// admin panel.
func tokenExchangeOriginAllowed(origin string, inst *instance.Instance) bool {
	if inst == nil {
		return false
	}
	originHost := utils.ExtractInstanceHost(origin)
	if originHost == "" {
		return false
	}

	if tokenExchangeOrgDomainOriginAllowed(originHost, inst) {
		return true
	}
	if tokenExchangeAppOriginAllowed(origin, originHost, inst) {
		return true
	}
	if tokenExchangeAppClientURLOriginAllowed(origin, inst) {
		return true
	}

	return tokenExchangeAdminPanelOriginAllowed(origin, originHost, inst)
}

func tokenExchangeOrgDomainOriginAllowed(originHost string, inst *instance.Instance) bool {
	orgDomain := utils.NormalizeDomain(inst.OrgDomain)
	if orgDomain == "" {
		return false
	}
	return originHost == orgDomain || strings.HasSuffix(originHost, "."+orgDomain)
}

func tokenExchangeAppOriginAllowed(origin, originHost string, inst *instance.Instance) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}

	instanceHost, appSlug, _ := config.SplitCozyHost(originHost)
	return appSlug != "" &&
		inst.HasDomain(instanceHost) &&
		tokenExchangeAppSlugAllowed(inst.ContextName, appSlug)
}

func tokenExchangeAppClientURLOriginAllowed(origin string, inst *instance.Instance) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}

	appExchangeConfig, err := config.GetOIDCAppTokenExchange(inst.ContextName)
	if err != nil || !appExchangeConfig.Enabled {
		return false
	}
	for _, appConfig := range appExchangeConfig.Apps {
		slug := oauth.GetLinkedAppSlug(appConfig.SoftwareID)
		if slug == "" {
			continue
		}
		clientURL := app.ResolveClientURL(inst, slug)
		if clientURL == "" {
			continue
		}
		parsed, err := url.Parse(clientURL)
		if err != nil || parsed.Host == "" {
			continue
		}
		if u.Scheme == parsed.Scheme && u.Host == parsed.Host {
			return true
		}
	}
	return false
}

func tokenExchangeAdminPanelOriginAllowed(origin, originHost string, inst *instance.Instance) bool {
	publicDomainSuffix := config.GetPublicDomainSuffix(inst.ContextName)
	if publicDomainSuffix == "" {
		return false
	}

	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return false
	}

	suffix := "." + publicDomainSuffix
	if !strings.HasSuffix(originHost, suffix) {
		return false
	}

	adminLabel := strings.TrimSuffix(originHost, suffix)
	if strings.Contains(adminLabel, ".") || !strings.HasSuffix(adminLabel, "-admin") {
		return false
	}

	ownerLabel := strings.TrimSuffix(adminLabel, "-admin")
	if ownerLabel == "" {
		return false
	}

	ownerInst, err := instance.Get(ownerLabel + suffix)
	if err != nil {
		return false
	}

	return tokenExchangeSameOrganization(inst, ownerInst)
}

func tokenExchangeSameOrganization(targetInst, originInst *instance.Instance) bool {
	if targetInst == nil || originInst == nil || targetInst.OrgID == "" || originInst.OrgID == "" {
		return false
	}
	if targetInst.OrgID != originInst.OrgID {
		return false
	}

	targetOrgDomain := utils.NormalizeDomain(targetInst.OrgDomain)
	originOrgDomain := utils.NormalizeDomain(originInst.OrgDomain)
	return targetOrgDomain == "" || originOrgDomain == "" || targetOrgDomain == originOrgDomain
}

func tokenExchangePreflight(c echo.Context) error {
	if !tokenExchangeCORS(c) {
		return c.NoContent(http.StatusForbidden)
	}
	req := c.Request()
	res := c.Response()
	res.Header().Add(echo.HeaderVary, echo.HeaderAccessControlRequestMethod)
	res.Header().Add(echo.HeaderVary, echo.HeaderAccessControlRequestHeaders)
	res.Header().Set(echo.HeaderAccessControlAllowMethods, echo.POST)
	res.Header().Set(echo.HeaderAccessControlMaxAge, middlewares.MaxAgeCORS)
	if h := req.Header.Get(echo.HeaderAccessControlRequestHeaders); h != "" {
		res.Header().Set(echo.HeaderAccessControlAllowHeaders, h)
	}
	return c.NoContent(http.StatusNoContent)
}

func tokenExchange(c echo.Context) error {
	if !tokenExchangeCORS(c) {
		return c.JSON(http.StatusForbidden, echo.Map{
			"error": "the origin of this application is not allowed",
		})
	}
	c.Response().Header().Set("Cache-Control", "no-store")
	c.Response().Header().Set("Pragma", "no-cache")

	inst := middlewares.GetInstance(c)
	var reqBody tokenExchangeRequest
	if err := c.Bind(&reqBody); err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid request body",
		})
	}
	reqBody.IDToken = strings.TrimSpace(reqBody.IDToken)
	reqBody.Scope = strings.TrimSpace(reqBody.Scope)
	reqBody.ExchangeType = strings.TrimSpace(reqBody.ExchangeType)
	if reqBody.IDToken == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "the id_token parameter is mandatory",
		})
	}
	exchangeType, err := tokenExchangeRequestExchangeType(reqBody.ExchangeType)
	if err != nil {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": err.Error(),
		})
	}
	reqBody.ExchangeType = exchangeType
	if exchangeType == tokenExchangeTypeAdmin && reqBody.Scope == "" {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "the scope parameter is mandatory",
		})
	}

	out, err := executeTokenExchange(c, inst, reqBody)
	if err != nil {
		var httpErr *echo.HTTPError
		if errors.As(err, &httpErr) {
			return c.JSON(httpErr.Code, echo.Map{
				"error": fmt.Sprint(httpErr.Message),
			})
		}
		return err
	}

	return c.JSON(http.StatusOK, out)
}

func registerFromWebApp(c echo.Context) error {
	res := c.Response()
	origin := c.Request().Header.Get(echo.HeaderOrigin)
	res.Header().Set(echo.HeaderAccessControlAllowOrigin, origin)
	res.Header().Set(echo.HeaderAccessControlAllowCredentials, "true")
	if err := middlewares.RequireSettingsApp(c); err != nil {
		return err
	}
	return registerClient(c)
}

// checkRedirectParam returns the optional redirect query parameter. If not
// empty, we check that the redirect is a subdomain of the cozy-instance.
func checkRedirectParam(c echo.Context, defaultRedirect *url.URL) (*url.URL, error) {
	instance := middlewares.GetInstance(c)
	redirect := c.FormValue("redirect")
	if redirect == "" {
		redirect = c.QueryParam("redirect")
	}

	// If the Cozy was moved from another address and the owner had a vault,
	// we will show them instructions about how to import their vault.
	settings, err := instance.SettingsDocument()
	if err == nil && settings.M["import_vault"] == true {
		u := url.URL{
			Scheme: instance.Scheme(),
			Host:   instance.ContextualDomain(),
			Path:   "/move/vault",
		}
		return &u, nil
	}

	if redirect == "" {
		if defaultRedirect == nil {
			return defaultRedirect, nil
		}
		redirect = defaultRedirect.String()
	}

	u, err := url.Parse(redirect)
	if err != nil || u.Scheme == "" {
		u, err = AppRedirection(instance, redirect)
	}
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			"bad url: could not parse")
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			"bad url: bad scheme")
	}

	if !instance.HasDomain(u.Host) {
		instanceHost, appSlug, _ := config.SplitCozyHost(u.Host)
		if !instance.HasDomain(instanceHost) || appSlug == "" {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				"bad url: should be subdomain")
		}
		return u, nil
	}

	// To protect against stealing authorization code with redirection, the
	// fragment is always overridden. Most browsers keep URI fragments upon
	// redirects, to make sure to override them, we put an empty one.
	//
	// see: oauthsecurity.com/#provider-in-the-middle
	// see: 7.4.2 OAuth2 in Action
	u.Fragment = "="
	return u, nil
}

func AppRedirection(inst *instance.Instance, redirect string) (*url.URL, error) {
	splits := strings.SplitN(redirect, "#", 2)
	parts := strings.SplitN(splits[0], "/", 2)
	if _, err := app.GetWebappBySlug(inst, parts[0]); err != nil {
		return nil, err
	}
	u := inst.SubDomain(parts[0])
	if len(parts) == 2 {
		u.Path = parts[1]
	}
	if len(splits) == 2 {
		u.Fragment = splits[1]
	}
	return u, nil
}

func resendActivationMail(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	rate := config.GetRateLimiter()
	if err := rate.CheckRateLimit(inst, limits.ResendOnboardingMailType); err == nil {
		client := instance.APIManagerClient(inst)
		if len(inst.RegisterToken) == 0 || inst.UUID == "" || client == nil {
			return errors.New("cannot resend activation link")
		}
		url := fmt.Sprintf("/api/v1/instances/%s/resend", url.PathEscape(inst.UUID))
		if err := client.Post(url, nil); err != nil {
			return errors.New("cannot resend activation link")
		}
	}
	return c.Render(http.StatusOK, "error.html", echo.Map{
		"Domain":         inst.ContextualDomain(),
		"ContextName":    inst.ContextName,
		"Locale":         inst.Locale,
		"Title":          inst.TemplateTitle(),
		"Favicon":        middlewares.Favicon(inst),
		"Inverted":       false,
		"Illustration":   "/images/mail-sent.svg",
		"ErrorTitle":     "Onboarding Resend activation Title",
		"Error":          "Onboarding Resend activation Body",
		"ErrorDetail":    "Onboarding Resend activation Detail",
		"SupportEmail":   inst.SupportEmailAddress(),
		"SupportPageURL": inst.SupportPageURL(),
	})
}

// Routes sets the routing for the status service
func Routes(router *echo.Group) {
	noCSRF := middlewares.CSRFWithConfig(middlewares.CSRFConfig{
		TokenLookup:    "form:csrf_token",
		CookieMaxAge:   3600, // 1 hour
		CookieHTTPOnly: true,
		CookieSecure:   !build.IsDevRelease(),
		CookieSameSite: http.SameSiteStrictMode,
		CookiePath:     "/auth",
	})

	// Login/logout
	router.GET("/login", loginForm, noCSRF, middlewares.CheckOnboardingNotFinished)
	router.POST("/login", login, noCSRF, middlewares.CheckOnboardingNotFinished)
	router.POST("/login/flagship", loginFlagship, middlewares.CheckOnboardingNotFinished)
	router.DELETE("/login/others", logoutOthers)
	router.OPTIONS("/login/others", logoutPreflight)
	router.DELETE("/login", logout)
	router.OPTIONS("/login", logoutPreflight)

	// Magic links
	router.POST("/magic_link", sendMagicLink, noCSRF)
	router.GET("/magic_link", loginWithMagicLink, noCSRF)
	router.POST("/magic_link/twofactor", loginWithMagicLinkAndPassword, noCSRF)
	router.POST("/magic_link/flagship", magicLinkFlagship)

	// Passphrase
	router.GET("/passphrase_reset", passphraseResetForm, noCSRF)
	router.POST("/passphrase_reset", passphraseReset, noCSRF)
	router.GET("/passphrase_renew", passphraseRenewForm, noCSRF)
	router.POST("/passphrase_renew", passphraseRenew, noCSRF)
	router.GET("/passphrase", passphraseForm, noCSRF)
	router.POST("/hint", sendHint)
	router.POST("/onboarding/resend", resendActivationMail)

	// Confirmation by typing
	router.GET("/confirm", confirmForm, noCSRF)
	router.POST("/confirm", confirmAuth, noCSRF)
	router.GET("/confirm/:code", confirmCode)

	// Register OAuth clients
	router.POST("/register", registerClient, middlewares.AcceptJSON, middlewares.ContentTypeJSON)
	router.POST("/registerFromWebApp", registerFromWebApp, middlewares.AcceptJSON, middlewares.ContentTypeJSON)
	router.OPTIONS("/registerFromWebApp", registerPreflight)
	router.GET("/register/:client-id", readClient, middlewares.AcceptJSON, checkRegistrationToken)
	router.PUT("/register/:client-id", updateClient, middlewares.AcceptJSON, middlewares.ContentTypeJSON)
	router.DELETE("/register/:client-id", deleteClient)
	router.POST("/clients/:client-id/challenge", postChallenge, checkRegistrationToken)
	router.POST("/clients/:client-id/attestation", postAttestation)
	router.POST("/clients/:client-id/flagship", confirmFlagship)

	// OAuth flow
	authHandler := NewAuthorizeHandler(config.GetConfig().DeprecatedApps)
	authHandler.Register(router.Group("/authorize", noCSRF))

	router.POST("/access_token", accessToken)
	router.POST("/token_exchange", tokenExchange, middlewares.AcceptJSON, middlewares.ContentTypeJSON)
	router.OPTIONS("/token_exchange", tokenExchangePreflight)

	// Flagship app
	router.POST("/session_code", CreateSessionCode)
	router.POST("/tokens/konnectors/:slug", buildKonnectorToken)

	// 2FA
	router.GET("/twofactor", twoFactorForm)
	router.POST("/twofactor", twoFactor)

	// Share by link protected by password
	router.POST("/share-by-link/password", checkPasswordForShareByLink)
}
