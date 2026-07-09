// Package settings regroups some API methods to facilitate the usage of the
// io.cozy settings documents.
package settings

import (
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

type apiPassphraseParameters struct {
	Salt       string `json:"salt"`
	Kdf        int    `json:"kdf"`
	Iterations int    `json:"iterations"`
}

func (p *apiPassphraseParameters) ID() string                             { return consts.PassphraseParametersID }
func (p *apiPassphraseParameters) Rev() string                            { return "" }
func (p *apiPassphraseParameters) DocType() string                        { return consts.Settings }
func (p *apiPassphraseParameters) Clone() couchdb.Doc                     { return p }
func (p *apiPassphraseParameters) SetID(_ string)                         {}
func (p *apiPassphraseParameters) SetRev(_ string)                        {}
func (p *apiPassphraseParameters) Relationships() jsonapi.RelationshipMap { return nil }
func (p *apiPassphraseParameters) Included() []jsonapi.Object             { return nil }
func (p *apiPassphraseParameters) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/settings/passphrase"}
}

func (h *HTTPHandler) getPassphraseParameters(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Settings); err != nil {
		return err
	}
	inst := middlewares.GetInstance(c)
	settings, err := settings.Get(inst)
	if err != nil {
		return err
	}
	params := apiPassphraseParameters{
		Salt:       string(inst.PassphraseSalt()),
		Kdf:        settings.PassphraseKdf,
		Iterations: settings.PassphraseKdfIterations,
	}
	return jsonapi.Data(c, http.StatusOK, &params, nil)
}

type passphraseRegistrationParameters struct {
	Redirection string `json:"redirection" form:"redirection"`
	Register    string `json:"register_token" form:"register_token"`
	Passphrase  string `json:"passphrase" form:"passphrase"`
	Hint        string `json:"hint" form:"hint"`
	Key         string `json:"key" form:"key"`
	PublicKey   string `json:"public_key" form:"public_key"`
	PrivateKey  string `json:"private_key" form:"private_key"`
	Iterations  int    `json:"iterations" form:"iterations"`

	// For flagship app
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

func (h *HTTPHandler) registerPassphrase(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	accept := c.Request().Header.Get(echo.HeaderAccept)
	acceptHTML := strings.Contains(accept, echo.MIMETextHTML)

	var args passphraseRegistrationParameters
	if err := c.Bind(&args); err != nil {
		return err
	}

	registerToken, err := hex.DecodeString(args.Register)
	if err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if args.Iterations < crypto.MinPBKDF2Iterations && args.Iterations != 0 {
		err := errors.New("The KdfIterations number is too low")
		return jsonapi.InvalidParameter("KdfIterations", err)
	}
	if args.Iterations > crypto.MaxPBKDF2Iterations {
		err := errors.New("The KdfIterations number is too high")
		return jsonapi.InvalidParameter("KdfIterations", err)
	}

	passphrase := []byte(args.Passphrase)
	err = lifecycle.RegisterPassphrase(inst, registerToken, lifecycle.PassParameters{
		Pass:       passphrase,
		Iterations: args.Iterations,
		Key:        args.Key,
		PublicKey:  args.PublicKey,
		PrivateKey: args.PrivateKey,
	})
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	if args.Hint != "" {
		setting, err := settings.Get(inst)
		if err != nil {
			return err
		}
		setting.PassphraseHint = args.Hint
		if err := setting.Save(inst); err != nil {
			return err
		}
	}

	sessionID, err := auth.SetCookieForNewSession(c, session.LongRun, "")
	if err != nil {
		return err
	}
	if err := session.StoreNewLoginEntry(inst, sessionID, "", c.Request(), "registration"); err != nil {
		inst.Logger().Errorf("Could not store session history %q: %s", sessionID, err)
	}

	return finishOnboarding(c, args.Redirection, acceptHTML)
}

func (h *HTTPHandler) registerPassphraseFlagship(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	var args passphraseRegistrationParameters
	if err := c.Bind(&args); err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	registerToken, err := hex.DecodeString(args.Register)
	if err != nil {
		return jsonapi.Errorf(http.StatusBadRequest, "%s", err)
	}

	if args.Iterations < crypto.MinPBKDF2Iterations {
		err := errors.New("The KdfIterations number is too low")
		return jsonapi.InvalidParameter("KdfIterations", err)
	}
	if args.Iterations > crypto.MaxPBKDF2Iterations {
		err := errors.New("The KdfIterations number is too high")
		return jsonapi.InvalidParameter("KdfIterations", err)
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

	passphrase := []byte(args.Passphrase)
	inst.OnboardingFinished = true
	err = lifecycle.RegisterPassphrase(inst, registerToken, lifecycle.PassParameters{
		Pass:       passphrase,
		Iterations: args.Iterations,
		Key:        args.Key,
		PublicKey:  args.PublicKey,
		PrivateKey: args.PrivateKey,
	})
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	if args.Hint != "" {
		setting, err := settings.Get(inst)
		if err != nil {
			return err
		}
		setting.PassphraseHint = args.Hint
		if err := setting.Save(inst); err != nil {
			return err
		}
	}

	if !client.Flagship {
		context := inst.ContextName
		if context == "" {
			context = config.DefaultInstanceContext
		}
		cfg := config.GetConfig().Flagship.Contexts[context]
		skipCertification := false
		if cfg, ok := cfg.(map[string]interface{}); ok {
			skipCertification = cfg["skip_certification"] == true
		}
		if !skipCertification {
			_ = client.SetCreatedAtOnboarding(inst)
			return auth.ReturnSessionCode(c, http.StatusAccepted, inst, "password")
		}
	}

	out := auth.AccessTokenReponse{
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

func (h *HTTPHandler) updatePassphrase(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	currentSession, hasSession := middlewares.GetSession(c)

	// Even if the current passphrase is needed for this request to work, we
	// enforce a valid permission to avoid having an unauthorized enpoint that
	// can be bruteforced.
	if err := middlewares.AllowWholeType(c, permission.PUT, consts.Settings); err != nil {
		return err
	}

	args := struct {
		Current           string `json:"current_passphrase"`
		Passphrase        string `json:"new_passphrase"`
		Iterations        int    `json:"iterations"`
		TwoFactorPasscode string `json:"two_factor_passcode"`
		TwoFactorToken    []byte `json:"two_factor_token"`
		Force             bool   `json:"force,omitempty"`
		Key               string `json:"key"`
		PublicKey         string `json:"publicKey"`
		PrivateKey        string `json:"privateKey"`
	}{}
	err := c.Bind(&args)
	if err != nil {
		return jsonapi.BadRequest(err)
	}
	newPassphrase := []byte(args.Passphrase)
	currentPassphrase := []byte(args.Current)

	// If we want to force the update
	if args.Force {
		canForce := false

		// CLI can force the passphrase
		if _, ok := middlewares.GetCLIPermission(c); ok {
			canForce = true
		}

		// On cozy with OIDC and empty vault, the password can be forced to
		// allow the setup of Cozy Pass. Same for magic links. But only to
		// set a password for the first time.
		if inst.PasswordDefined == nil {
			if inst.HasForcedOIDC() || inst.MagicLink {
				bitwarden, err := settings.Get(inst)
				if err == nil && !bitwarden.ExtensionInstalled {
					canForce = true
				}
			}
		} else if !*inst.PasswordDefined {
			canForce = true
		}

		if !canForce {
			err = fmt.Errorf("Bitwarden extension has already been installed on this Cozy, cannot force update the passphrase.")
			return jsonapi.BadRequest(err)
		}

		params := lifecycle.PassParameters{
			Pass:       []byte(args.Passphrase),
			Iterations: args.Iterations,
			PublicKey:  args.PublicKey,
			PrivateKey: args.PrivateKey,
			Key:        args.Key,
		}
		err = lifecycle.ForceUpdatePassphrase(inst, newPassphrase, params)
		if err != nil {
			return err
		}
		go func() {
			_ = sharing.SendPublicKey(inst, params.PublicKey)
		}()
		if hasSession {
			_, _ = auth.SetCookieForNewSession(c, currentSession.Duration(), "")
		}
		return c.NoContent(http.StatusNoContent)
	}

	// Else, we keep going on the standard checks (2FA, current passphrase, ...)
	if inst.HasAuthMode(instance.TwoFactorMail) && len(args.TwoFactorToken) == 0 {
		if instance.CheckPassphrase(inst, currentPassphrase) == nil {
			var twoFactorToken []byte
			twoFactorToken, err = lifecycle.SendTwoFactorPasscode(inst)
			if err != nil {
				return err
			}
			return c.JSON(http.StatusOK, echo.Map{
				"two_factor_token": twoFactorToken,
			})
		}
		return instance.ErrInvalidPassphrase
	}

	if args.Iterations < crypto.MinPBKDF2Iterations && args.Iterations != 0 {
		err := errors.New("The KdfIterations number is too low")
		return jsonapi.InvalidParameter("KdfIterations", err)
	}
	if args.Iterations > crypto.MaxPBKDF2Iterations {
		err := errors.New("The KdfIterations number is too high")
		return jsonapi.InvalidParameter("KdfIterations", err)
	}

	err = lifecycle.UpdatePassphrase(inst, currentPassphrase,
		args.TwoFactorPasscode, args.TwoFactorToken,
		lifecycle.PassParameters{
			Pass:       newPassphrase,
			Iterations: args.Iterations,
			Key:        args.Key,
		})
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	duration := session.LongRun
	if hasSession {
		duration = currentSession.Duration()
	}
	if _, err = auth.SetCookieForNewSession(c, duration, ""); err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) checkPassphrase(c echo.Context) error {
	// Even if the current passphrase is needed for this request to work, we
	// enforce a valid permission to avoid having an unauthorized enpoint that
	// can be bruteforced.
	if err := middlewares.AllowWholeType(c, permission.PUT, consts.Settings); err != nil {
		return err
	}

	inst := middlewares.GetInstance(c)
	args := struct {
		Passphrase string `json:"passphrase"`
	}{}
	err := c.Bind(&args)
	if err != nil {
		return jsonapi.BadRequest(err)
	}

	if instance.CheckPassphrase(inst, []byte(args.Passphrase)) != nil {
		return jsonapi.Forbidden(instance.ErrInvalidPassphrase)
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) getHint(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.GET, consts.Settings); err != nil {
		return err
	}

	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}

	if setting.PassphraseHint == "" {
		return jsonapi.NotFound(errors.New("No hint"))
	}

	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) updateHint(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.PUT, consts.Settings); err != nil {
		return err
	}

	args := struct {
		Hint string `json:"hint"`
	}{}
	if err := c.Bind(&args); err != nil {
		return jsonapi.BadRequest(err)
	}

	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	if err := lifecycle.CheckHint(inst, setting, args.Hint); err != nil {
		return jsonapi.InvalidParameter("hint", err)
	}

	setting.PassphraseHint = args.Hint
	if err := setting.Save(inst); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

func (h *HTTPHandler) createVault(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	if err := middlewares.AllowWholeType(c, permission.POST, consts.BitwardenCiphers); err != nil {
		return err
	}

	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}

	if !setting.ExtensionInstalled {
		if err := settings.MigrateAccountsToCiphers(inst); err != nil {
			return jsonapi.InternalServerError(err)
		}
	}
	return c.NoContent(http.StatusNoContent)
}
