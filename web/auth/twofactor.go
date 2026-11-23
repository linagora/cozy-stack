package auth

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func renderTwoFactorForm(c echo.Context, i *instance.Instance, code int, credsError string, twoFactorToken []byte) error {
	title := i.Translate("Login Two factor title")

	longRunSession, err := getTwoFactorLongRunSession(c)
	if err != nil {
		return err
	}
	redirect, err := getTwoFactorRedirect(c)
	if err != nil {
		return err
	}

	trustedDeviceCheckBox := true
	trustedDeviceCheckBoxParam := c.QueryParam("trusted_device_checkbox")
	if trustedDeviceCheckBoxParam != "" {
		if b, err := strconv.ParseBool(trustedDeviceCheckBoxParam); err == nil {
			trustedDeviceCheckBox = b
		}
	}

	oauth := hasRedirectToAuthorize(i, redirect)
	trustedCheckbox := !oauth && trustedDeviceCheckBox

	return c.Render(code, "twofactor.html", echo.Map{
		"Domain":                i.ContextualDomain(),
		"ContextName":           i.ContextName,
		"Locale":                i.Locale,
		"Title":                 title,
		"Favicon":               middlewares.Favicon(i),
		"CredentialsError":      credsError,
		"Redirect":              redirect.String(),
		"Confirm":               c.FormValue("confirm"),
		"State":                 c.FormValue("state"),
		"ClientID":              c.FormValue("client_id"),
		"LongRunSession":        longRunSession,
		"TwoFactorToken":        string(twoFactorToken),
		"TrustedDeviceCheckBox": trustedCheckbox,
	})
}

func getTwoFactorLongRunSession(c echo.Context) (bool, error) {
	if longRunParam := c.QueryParam("long-run-session"); longRunParam != "" {
		longRun, err := strconv.ParseBool(longRunParam)
		if err != nil {
			return false, err
		}
		return longRun, nil
	}
	return false, nil
}

func getTwoFactorRedirect(c echo.Context) (*url.URL, error) {
	inst := middlewares.GetInstance(c)
	if c.FormValue("client_id") != "" {
		return url.Parse(c.FormValue("redirect"))
	}
	return checkRedirectParam(c, inst.DefaultRedirection())
}

// twoFactorForm handles the twoFactor from GET request
func twoFactorForm(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	twoFactorTokenParam := c.QueryParam("two_factor_token")
	if twoFactorTokenParam == "" {
		return c.JSON(http.StatusBadRequest, "Missing twoFactorToken")
	}
	twoFactorToken := []byte(twoFactorTokenParam)

	return renderTwoFactorForm(c, inst, http.StatusOK, "", twoFactorToken)
}

// twoFactor handles a the twoFactor POST request
func twoFactor(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if !inst.HasAuthMode(instance.TwoFactorMail) {
		errorMessage := inst.Translate(TwoFactorErrorKey)
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": errorMessage,
		})
	}

	// Retreiving data from request
	token := []byte(c.FormValue("two-factor-token"))
	passcode := c.FormValue("two-factor-passcode")
	generateTrustedDeviceToken, _ := strconv.ParseBool(c.FormValue("two-factor-generate-trusted-device-token"))

	// Handle 2FA failed
	correctPasscode := inst.ValidateTwoFactorPasscode(token, passcode)
	if !correctPasscode {
		return twoFactorFailed(c, inst, token)
	}

	// Special case when the 2FA validation is for confirming authentication,
	// not creating a new session.
	if c.FormValue("confirm") == "true" {
		return ConfirmSuccess(c, inst, c.FormValue("state"))
	}

	// Special case when the 2FA validation if for moving a Cozy to this
	// instance.
	if c.FormValue("client_id") != "" {
		u, err := moveSuccessURI(c)
		if err != nil {
			return err
		}
		return c.JSON(http.StatusOK, echo.Map{
			"redirect": u,
		})
	}

	// Generate a new session
	longRunSession, err := getTwoFactorLongRunSession(c)
	if err != nil {
		return err
	}
	redirect, err := getTwoFactorRedirect(c)
	if err != nil {
		return err
	}
	duration := session.NormalRun
	if longRunSession {
		duration = session.LongRun
	} else if hasRedirectToAuthorize(inst, redirect) {
		duration = session.ShortRun
	}
	if err := newSession(c, inst, redirect, duration, "2FA"); err != nil {
		return err
	}

	// Check if the user trusts its device
	var generatedTrustedDeviceToken []byte
	if generateTrustedDeviceToken {
		generatedTrustedDeviceToken, _ = inst.GenerateTwoFactorTrustedDeviceSecret(c.Request())
	}
	if wantsJSON(c) {
		result := echo.Map{"redirect": redirect.String()}
		if len(generatedTrustedDeviceToken) > 0 {
			result["two_factor_trusted_device_token"] = string(generatedTrustedDeviceToken)
		}
		return c.JSON(http.StatusOK, result)
	}

	return c.Redirect(http.StatusSeeOther, redirect.String())
}

// twoFactorFailed returns the 2FA form with an error message
func twoFactorFailed(c echo.Context, inst *instance.Instance, token []byte) error {
	errorMessage := inst.Translate(TwoFactorErrorKey)
	if err := CountFailedTwoFactor(inst); err != nil {
		errorMessage = inst.Translate(TwoFactorExceededErrorKey)
	}

	// Render either the passcode page or a JSON message
	if wantsJSON(c) {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": errorMessage,
		})
	}
	return renderTwoFactorForm(c, inst, http.StatusUnauthorized, errorMessage, token)
}
