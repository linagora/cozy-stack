// Package bitwarden exposes an API compatible with the Bitwarden Open-Soure apps.
package bitwarden

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// GetConfig is the handler for GET /bitwarden/api/config.
func GetConfig(c echo.Context) error {
	inst := middlewares.GetInstance(c)

	// The "cipher key encryption" feature was introduced in 2024.2.0, and we
	// don't support it. So, we need a version number before that.
	version := "2024.0.0"

	return c.JSON(http.StatusOK, echo.Map{
		"version": version,
		"gitHash": nil,
		"server":  nil,
		"environment": map[string]interface{}{
			"cloudRegion":   "EU",
			"vault":         inst.PageURL("", nil),
			"api":           inst.PageURL("", nil),
			"identity":      inst.PageURL("", nil),
			"notifications": inst.PageURL("", nil),
		},
		"featureStates": map[string]interface{}{},
		"object":        "config",
	})
}

// Prelogin tells to the client how many KDF iterations it must apply when
// hashing the master password.
func Prelogin(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	oidc := inst.HasForcedOIDC()
	hasCiphers := true
	if resp, err := couchdb.NormalDocs(inst, consts.BitwardenCiphers, 0, 1, "", false); err == nil {
		hasCiphers = resp.Total > 0
	}
	flat := config.GetConfig().Subdomains == config.FlatSubdomains
	return c.JSON(http.StatusOK, echo.Map{
		"Kdf":            setting.PassphraseKdf,
		"KdfIterations":  setting.PassphraseKdfIterations,
		"OIDC":           oidc,
		"HasCiphers":     hasCiphers,
		"FlatSubdomains": flat,
		"Salt":           string(inst.PassphraseSalt()),
	})
}

// SendHint is the handler for sending the hint when the user has forgot their
// password.
func SendHint(c echo.Context) error {
	i := middlewares.GetInstance(c)
	return lifecycle.SendHint(i)
}

// GetProfile is the handler for the route to get profile information.
func GetProfile(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenProfiles); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	profile, err := newProfileResponse(inst, setting)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, profile)
}

// UpdateProfile is the handler for the route to update the profile. Currently,
// only the hint for the master password can be changed.
func UpdateProfile(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.PUT, consts.BitwardenProfiles); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	var data struct {
		Hint string `json:"masterPasswordHint"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&data); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid JSON payload",
		})
	}
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	setting.PassphraseHint = data.Hint
	if err := setting.Save(inst); err != nil {
		return err
	}
	profile, err := newProfileResponse(inst, setting)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, profile)
}

// SetKeyPair is the handler for setting the key pair: public and private keys.
func SetKeyPair(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	log := inst.Logger().WithNamespace("bitwarden")
	if err := middlewares.AllowWholeType(c, permission.POST, consts.BitwardenProfiles); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	var data struct {
		Private string `json:"encryptedPrivateKey"`
		Public  string `json:"publicKey"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&data); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid JSON payload",
		})
	}
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	if err := setting.SetKeyPair(inst, data.Public, data.Private); err != nil {
		log.Errorf("Cannot set key pair: %s", err)
		return err
	}
	profile, err := newProfileResponse(inst, setting)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, profile)
}

// ChangeSecurityStamp is used by the client to change the security stamp,
// which will deconnect all the clients.
func ChangeSecurityStamp(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	var data struct {
		Hashed string `json:"masterPasswordHash"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&data); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid JSON payload",
		})
	}

	if err := instance.CheckPassphrase(inst, []byte(data.Hashed)); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid masterPasswordHash",
		})
	}

	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	setting.SecurityStamp = lifecycle.NewSecurityStamp()
	if err := setting.Save(inst); err != nil {
		return err
	}
	return c.NoContent(http.StatusNoContent)
}

// GetRevisionDate returns the date of the last synchronization (as a number of
// milliseconds).
func GetRevisionDate(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenProfiles); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}

	at := setting.Metadata.UpdatedAt
	milliseconds := fmt.Sprintf("%d", at.UnixNano()/1000000)
	return c.Blob(http.StatusOK, "text/plain", []byte(milliseconds))
}

// GetToken is used by the clients to get an access token. There are two
// supported grant types: password and refresh_token. Password is used the
// first time to register the client, and gets the initial credentials, by
// sending a hash of the user password. Refresh token is used later to get
// a new access token by sending the refresh token.
func GetToken(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	copier := app.Copier(consts.WebappType, inst)
	_, err := app.GetWebappBySlugAndUpdate(inst, consts.PassSlug, copier, inst.Registries())
	if err != nil {
		installer, err := app.NewInstaller(inst, copier,
			&app.InstallerOptions{
				Operation:  app.Install,
				Type:       consts.WebappType,
				SourceURL:  "registry://" + consts.PassSlug,
				Slug:       consts.PassSlug,
				Registries: inst.Registries(),
			},
		)
		if err == nil {
			_, _ = installer.RunSync()
		}
	}

	switch c.FormValue("grant_type") {
	case "password":
		return getInitialCredentials(c)
	case "refresh_token":
		return refreshToken(c)
	case "":
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "the grant_type parameter is mandatory",
		})
	default:
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid grant type",
		})
	}
}

// AccessTokenReponse is the stuct used for serializing to JSON the response
// for an access token.
type AccessTokenReponse struct {
	ClientID   string      `json:"client_id,omitempty"`
	RegToken   string      `json:"registration_access_token,omitempty"`
	Type       string      `json:"token_type"`
	ExpiresIn  int         `json:"expires_in"`
	Access     string      `json:"access_token"`
	Refresh    string      `json:"refresh_token"`
	Key        string      `json:"Key"`
	PrivateKey interface{} `json:"PrivateKey"`
	Kdf        int         `json:"Kdf"`
	Iterations int         `json:"KdfIterations"`
}

func getInitialCredentials(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	pass := []byte(c.FormValue("password"))

	// Authentication
	if err := instance.CheckPassphrase(inst, pass); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid password",
		})
	}

	if inst.HasAuthMode(instance.TwoFactorMail) {
		if !checkTwoFactor(c, inst) {
			return nil
		}
	}

	return RegisterClientAndReturnTokens(c, inst)
}

func RegisterClientAndReturnTokens(c echo.Context, inst *instance.Instance) error {
	log := inst.Logger().WithNamespace("bitwarden")

	// Register the client
	kind := bitwarden.ParseBitwardenDeviceType(c.FormValue("deviceType"))
	clientName := c.FormValue("clientName")
	if clientName == "" {
		clientName = "Bitwarden " + c.FormValue("deviceName")
	}
	client := &oauth.Client{
		RedirectURIs: []string{"https://cozy.io/"},
		ClientName:   clientName,
		ClientKind:   kind,
		SoftwareID:   "registry://" + consts.PassSlug,
	}
	if err := client.Create(inst, oauth.NotPending); err != nil {
		return c.JSON(err.Code, err)
	}
	client.CouchID = client.ClientID

	// Create the credentials
	access, err := bitwarden.CreateAccessJWT(inst, client)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate access token",
		})
	}
	refresh, err := bitwarden.CreateRefreshJWT(inst, client)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate refresh token",
		})
	}
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	key := setting.Key

	if _, err := setting.OrganizationKey(); errors.Is(err, settings.ErrMissingOrgKey) {
		// The organization key should exist at this moment as it is created at the
		// instance creation or at the login-hashed migration.
		log.Warnf("Organization key does not exist")
		err := setting.EnsureCozyOrganization(inst)
		if err != nil {
			return err
		}
		err = couchdb.UpdateDoc(inst, setting)
		if err != nil {
			return err
		}
	}
	if client.ClientKind != "web" && !setting.ExtensionInstalled {
		// This is the first time the bitwarden extension is installed: make sure
		// the user gets the existing accounts into the vault.
		// ClientKind is "web" for web apps, e.g. Settings
		if err := settings.MigrateAccountsToCiphers(inst); err != nil {
			log.Errorf("Cannot push job for ciphers migration: %s", err)
		}
	}

	var ip string
	if forwardedFor := c.Request().Header.Get(echo.HeaderXForwardedFor); forwardedFor != "" {
		ip = strings.TrimSpace(strings.SplitN(forwardedFor, ",", 2)[0])
	}
	if ip == "" {
		ip = strings.Split(c.Request().RemoteAddr, ":")[0]
	}
	inst.Logger().WithNamespace("loginaudit").
		Infof("New bitwarden client from %s at %s", ip, time.Now())

	// Send the response
	out := AccessTokenReponse{
		ClientID:   client.ClientID,
		RegToken:   client.RegistrationToken,
		Type:       "Bearer",
		ExpiresIn:  int(consts.AccessTokenValidityDuration.Seconds()),
		Access:     access,
		Refresh:    refresh,
		Key:        key,
		Kdf:        setting.PassphraseKdf,
		Iterations: setting.PassphraseKdfIterations,
	}
	if setting.PrivateKey != "" {
		out.PrivateKey = setting.PrivateKey
	}
	return c.JSON(http.StatusOK, out)
}

// checkTwoFactor returns true if the request has a valid 2FA code.
func checkTwoFactor(c echo.Context, inst *instance.Instance) bool {
	cache := config.GetConfig().CacheStorage
	key := "bw-2fa:" + inst.Domain

	if passcode := c.FormValue("twoFactorToken"); passcode != "" {
		if token, ok := cache.Get(key); ok {
			if inst.ValidateTwoFactorPasscode(token, passcode) {
				return true
			} else {
				_ = c.JSON(http.StatusBadRequest, echo.Map{
					"error":             "invalid_grant",
					"error_description": "invalid_username_or_password",
					"ErrorModel": map[string]string{
						"Message": "Two-step token is invalid. Try again.",
						"Object":  "error",
					},
				})
				return false
			}
		}
	}

	// Allow the settings webapp get a bitwarden token without the 2FA. It's OK
	// from a security point of view as we still have 2 factors: the password
	// and a valid session cookie.
	if _, ok := middlewares.GetSession(c); ok {
		return true
	}

	email, err := inst.SettingsEMail()
	if err != nil {
		_ = c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
		return false
	}
	var obscured string
	if parts := strings.SplitN(email, "@", 2); len(parts) == 2 {
		s := strings.Map(func(_ rune) rune { return '*' }, parts[0])
		obscured = s + "@" + parts[1]
	}

	token, err := lifecycle.SendTwoFactorPasscode(inst)
	if err != nil {
		_ = c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
		return false
	}
	cache.Set(key, token, 5*time.Minute)

	_ = c.JSON(http.StatusBadRequest, echo.Map{
		"error":             "invalid_grant",
		"error_description": "Two factor required.",
		// 1 means email
		// https://github.com/bitwarden/jslib/blob/master/common/src/enums/twoFactorProviderType.ts
		"TwoFactorProviders": []int{1},
		"TwoFactorProviders2": map[string]map[string]string{
			"1": {"Email": obscured},
		},
	})
	return false
}

func refreshToken(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	refresh := c.FormValue("refresh_token")

	// Check the refresh token
	claims, ok := oauth.ValidTokenWithSStamp(inst, consts.RefreshTokenAudience, refresh)
	if !ok {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid refresh token",
		})
	}

	// Find the OAuth client
	client, err := oauth.FindClient(inst, claims.Subject)
	if err != nil {
		if couchErr, isCouchErr := couchdb.IsCouchError(err); isCouchErr && couchErr.StatusCode >= 500 {
			return err
		}
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "the client must be registered",
		})
	}
	if !bitwarden.IsBitwardenClient(client, claims.Scope) {
		return c.JSON(http.StatusBadRequest, echo.Map{
			"error": "invalid refresh token",
		})
	}

	// Create the credentials
	access, err := bitwarden.CreateAccessJWT(inst, client)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": "Can't generate access token",
		})
	}
	setting, err := settings.Get(inst)
	if err != nil {
		return err
	}
	key := setting.Key

	// Send the response
	out := AccessTokenReponse{
		Type:       "Bearer",
		ExpiresIn:  int(consts.AccessTokenValidityDuration.Seconds()),
		Access:     access,
		Refresh:    refresh,
		Key:        key,
		Kdf:        setting.PassphraseKdf,
		Iterations: setting.PassphraseKdfIterations,
	}
	if setting.PrivateKey != "" {
		out.PrivateKey = setting.PrivateKey
	}
	return c.JSON(http.StatusOK, out)
}

// GetCozy returns the information about the cozy organization, including the
// organization key.
func GetCozy(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.BitwardenOrganizations); err != nil {
		return c.JSON(http.StatusUnauthorized, echo.Map{
			"error": "invalid token",
		})
	}

	setting, err := settings.Get(inst)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}
	orgKey, err := setting.OrganizationKey()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err.Error(),
		})
	}

	res := map[string]interface{}{
		"organizationId":  setting.OrganizationID,
		"collectionId":    setting.CollectionID,
		"organizationKey": orgKey,
	}
	return c.JSON(http.StatusOK, res)
}

// Routes sets the routing for the Bitwarden-like API
func Routes(router *echo.Group) {
	identity := router.Group("/identity")
	identity.POST("/connect/token", GetToken)
	identity.POST("/accounts/prelogin", Prelogin)

	api := router.Group("/api")
	api.GET("/config", GetConfig)
	api.GET("/sync", Sync)

	accounts := api.Group("/accounts")
	accounts.POST("/prelogin", Prelogin)
	accounts.POST("/password-hint", SendHint)
	accounts.GET("/profile", GetProfile)
	accounts.POST("/profile", UpdateProfile)
	accounts.PUT("/profile", UpdateProfile)
	accounts.POST("/keys", SetKeyPair)
	accounts.POST("/security-stamp", ChangeSecurityStamp)
	accounts.GET("/revision-date", GetRevisionDate)

	settings := api.Group("/settings")
	settings.GET("/domains", GetDomains)
	settings.PUT("/domains", UpdateDomains)
	settings.POST("/domains", UpdateDomains)

	ciphers := api.Group("/ciphers")
	ciphers.GET("", ListCiphers)
	ciphers.POST("", CreateCipher)
	ciphers.POST("/create", CreateSharedCipher)
	ciphers.GET("/:id", GetCipher)
	ciphers.GET("/:id/details", GetCipher)
	ciphers.POST("/:id", UpdateCipher)
	ciphers.PUT("/:id", UpdateCipher)
	ciphers.POST("/import", ImportCiphers)

	ciphers.DELETE("/:id", DeleteCipher)
	ciphers.POST("/:id/delete", DeleteCipher)
	ciphers.PUT("/:id/delete", SoftDeleteCipher)
	ciphers.PUT("/:id/restore", RestoreCipher)
	ciphers.DELETE("", BulkDeleteCiphers)
	ciphers.POST("/delete", BulkDeleteCiphers)
	ciphers.PUT("/delete", BulkSoftDeleteCiphers)
	ciphers.PUT("/restore", BulkRestoreCiphers)

	ciphers.POST("/:id/share", ShareCipher)
	ciphers.PUT("/:id/share", ShareCipher)

	folders := api.Group("/folders")
	folders.GET("", ListFolders)
	folders.POST("", CreateFolder)
	folders.GET("/:id", GetFolder)
	folders.POST("/:id", RenameFolder)
	folders.PUT("/:id", RenameFolder)
	folders.DELETE("/:id", DeleteFolder)
	folders.POST("/:id/delete", DeleteFolder)

	orgs := api.Group("/organizations")
	orgs.POST("", CreateOrganization)
	orgs.GET("/:id", GetOrganization)
	orgs.GET("/:id/collections", GetCollections)
	orgs.DELETE("/:id", DeleteOrganization)
	orgs.GET("/:id/users", ListOrganizationUser)
	orgs.POST("/:id/users/:user-id/confirm", ConfirmUser)

	router.GET("/organizations/cozy", GetCozy)
	router.DELETE("/contacts/:id", RefuseContact)

	api.GET("/users/:id/public-key", GetPublicKey)

	hub := router.Group("/notifications/hub")
	hub.GET("", WebsocketHub)
	hub.POST("/negotiate", NegotiateHub)

	icons := router.Group("/icons")
	cacheControl := middlewares.CacheControl(middlewares.CacheOptions{
		MaxAge: 24 * time.Hour,
	})
	icons.GET("/:domain/icon.png", GetIcon, cacheControl)
}
