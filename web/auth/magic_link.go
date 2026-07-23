package auth

import (
	"crypto/subtle"
	"net/http"

	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func sendMagicLink(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	err := config.GetRateLimiter().CheckRateLimit(inst, limits.MagicLinkType)
	if limits.IsLimitReachedOrExceeded(err) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "Too many requests")
	}

	redirect := c.FormValue("redirect")
	if err := lifecycle.SendMagicLink(inst, redirect); err != nil {
		return err
	}
	return c.Render(http.StatusOK, "error.html", echo.Map{
		"Domain":         inst.ContextualDomain(),
		"ContextName":    inst.ContextName,
		"Locale":         inst.Locale,
		"Title":          inst.TemplateTitle(),
		"Favicon":        middlewares.Favicon(inst),
		"Inverted":       false,
		"Illustration":   "/images/mail-sent.svg",
		"ErrorTitle":     "Magic link has been sent Title",
		"Error":          "Magic link has been sent Body",
		"ErrorDetail":    "Magic link has been sent Detail",
		"SupportEmail":   inst.SupportEmailAddress(),
		"SupportPageURL": inst.SupportPageURL(),
	})
}

func loginWithMagicLink(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	redirect, err := checkRedirectParam(c, inst.DefaultRedirection())
	if err != nil {
		return err
	}

	if _, ok := middlewares.GetSession(c); ok {
		return c.Redirect(http.StatusSeeOther, redirect.String())
	}

	code := c.QueryParam("code") // Login
	if code == "" {
		code = c.QueryParam("magic_code") // Onboarding from the cloudery
	}
	if err := lifecycle.CheckMagicLink(inst, code); err != nil {
		err := config.GetRateLimiter().CheckRateLimit(inst, limits.MagicLinkType)
		if limits.IsLimitReachedOrExceeded(err) {
			return echo.NewHTTPError(http.StatusTooManyRequests, "Too many requests")
		}
		return renderError(c, http.StatusBadRequest, "Error Invalid magic link")
	}

	if inst.HasAuthMode(instance.TwoFactorMail) {
		iterations := 0
		if settings, err := settings.Get(inst); err == nil {
			iterations = settings.PassphraseKdfIterations
		}
		return c.Render(http.StatusOK, "magic_link_twofactor.html", echo.Map{
			"TemplateTitle":  inst.TemplateTitle(),
			"Domain":         inst.ContextualDomain(),
			"ContextName":    inst.ContextName,
			"Locale":         inst.Locale,
			"Iterations":     iterations,
			"Salt":           string(inst.PassphraseSalt()),
			"CSRF":           c.Get("csrf"),
			"Favicon":        middlewares.Favicon(inst),
			"BottomNavBar":   middlewares.BottomNavigationBar(c),
			"CryptoPolyfill": middlewares.CryptoPolyfill(c),
			"MagicCode":      code,
			"Redirect":       redirect,
		})
	}

	err = newSession(c, inst, redirect, session.NormalRun, "magic_link")
	if err != nil {
		return err
	}
	return c.Redirect(http.StatusSeeOther, redirect.String())
}

func loginWithMagicLinkAndPassword(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	code := c.FormValue("magic_code")

	// Check magic code
	if err := lifecycle.CheckMagicLink(inst, code); err != nil {
		err := config.GetRateLimiter().CheckRateLimit(inst, limits.MagicLinkType)
		if limits.IsLimitReachedOrExceeded(err) {
			return echo.NewHTTPError(http.StatusTooManyRequests, "Too many requests")
		}
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": inst.Translate("Error Invalid magic link"),
		})
	}

	// Check passphrase
	passphrase := []byte(c.FormValue("passphrase"))
	if instance.CheckPassphrase(inst, passphrase) != nil {
		errorMessage := inst.Translate(CredentialsErrorKey)
		err := config.GetRateLimiter().CheckRateLimit(inst, limits.AuthType)
		if limits.IsLimitReachedOrExceeded(err) {
			if err = LoginRateExceeded(inst); err != nil {
				inst.Logger().WithNamespace("auth").Warn(err.Error())
			}
		}
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": errorMessage,
		})
	}

	redirect, err := checkRedirectParam(c, inst.DefaultRedirection())
	if err != nil {
		return err
	}
	err = newSession(c, inst, redirect, session.NormalRun, "magic_link")
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, echo.Map{
		"redirect": redirect.String(),
	})
}

type magicLinkFlagshipParameters struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	Code         string `json:"magic_code"`
	Passphrase   string `json:"passphrase"`
}

func magicLinkFlagship(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	var args magicLinkFlagshipParameters
	if err := c.Bind(&args); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if err := lifecycle.CheckMagicLink(inst, args.Code); err != nil {
		err := config.GetRateLimiter().CheckRateLimit(inst, limits.MagicLinkType)
		if limits.IsLimitReachedOrExceeded(err) {
			return echo.NewHTTPError(http.StatusTooManyRequests, "Too many requests")
		}
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid magic code",
		})
	}

	if inst.HasAuthMode(instance.TwoFactorMail) {
		if instance.CheckPassphrase(inst, []byte(args.Passphrase)) != nil {
			err := config.GetRateLimiter().CheckRateLimit(inst, limits.AuthType)
			if limits.IsLimitReachedOrExceeded(err) {
				if err = LoginRateExceeded(inst); err != nil {
					inst.Logger().WithNamespace("auth").Warn(err.Error())
				}
			}
			return c.JSON(http.StatusUnauthorized, echo.Map{
				"error": "passphrase is required as second authentication factor",
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
