package settings_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/session"
	csettings "github.com/cozy/cozy-stack/model/settings"
	cscommon "github.com/cozy/cozy-stack/model/settings/common"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	websettings "github.com/cozy/cozy-stack/web/settings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/cozy/cozy-stack/worker/mails"
)

func setupRouter(t *testing.T, inst *instance.Instance, svc csettings.Service) *httptest.Server {
	t.Helper()

	handler := echo.New()
	handler.HTTPErrorHandler = errors.ErrorHandler
	group := handler.Group("/settings", func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(context echo.Context) error {
			context.Set("instance", inst)

			cookie, err := context.Request().Cookie(session.CookieName(inst))
			if err != http.ErrNoCookie {
				require.NoError(t, err, "Could not get session cookie")
				if cookie.Value == "connected" {
					sess, _ := session.New(inst, session.LongRun, "")
					context.Set("session", sess)
				}
			}

			return next(context)
		}
	})

	websettings.NewHTTPHandler(svc, new(rabbitmq.NoopService)).Register(group)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)

	return ts
}

func TestSettings(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	var instanceRev string
	var oauthClientID string

	config.UseTestFile(t)
	conf := config.GetConfig()
	conf.Assets = "../../assets"
	conf.Contexts[config.DefaultInstanceContext] = map[string]interface{}{
		"logos": map[string]interface{}{
			"home": map[string]interface{}{
				"light": []interface{}{
					map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud"},
				},
			},
		},
	}
	was := conf.Subdomains
	conf.Subdomains = config.NestedSubdomains
	defer func() { conf.Subdomains = was }()

	_ = web.LoadSupportedLocales()
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	testInstance := setup.GetTestInstance(&lifecycle.Options{
		Locale:      "en",
		Timezone:    "Europe/Berlin",
		Email:       "alice@example.com",
		ContextName: "test-context",
		OrgDomain:   "test-org.example.com",
		OrgID:       "test-org-id-456",
	})
	scope := consts.Settings + " " + consts.OAuthClients
	_, token := setup.GetTestClient(scope)
	sessCookie := session.CookieName(testInstance)

	svc := csettings.NewServiceMock(t)
	ts := setupRouter(t, testInstance, svc)
	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	tsURL := ts.URL

	t.Run("GetContext", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		testutils.WithManager(t, testInstance, testutils.ManagerConfig{URL: "http://manager.example.org"})

		obj := e.GET("/settings/context").
			WithCookie(sessCookie, "connected").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.context")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("manager_url", "http://manager.example.org")
		attrs.HasValue("logos", map[string]interface{}{
			"home": map[string]interface{}{
				"light": []interface{}{
					map[string]interface{}{"src": "/logos/main_cozy.png", "alt": "Cozy Cloud"},
				},
			},
		})
	})

	t.Run("GetContextWithAntivirusDisabled", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		// Antivirus is disabled by default
		obj := e.GET("/settings/context").
			WithCookie(sessCookie, "connected").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		av := attrs.Value("antivirus").Object()
		av.HasValue("enabled", false)
	})

	t.Run("GetContextWithAntivirusDefaultActions", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		// Enable antivirus without specifying actions (uses defaults)
		conf.Contexts["test-context"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"address": "localhost:3310",
				"notifications": map[string]interface{}{
					"email_on_infected": true,
				},
			},
		}
		defer func() {
			delete(conf.Contexts, "test-context")
		}()

		obj := e.GET("/settings/context").
			WithCookie(sessCookie, "connected").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		av := attrs.Value("antivirus").Object()
		av.HasValue("enabled", true)

		// Check default actions matrix
		actions := av.Value("actions").Object()
		actions.Value("pending").Array().Contains("download", "share", "preview", "delete")
		actions.Value("clean").Array().Contains("download", "share", "preview", "delete")
		actions.Value("infected").Array().Contains("delete")
		actions.Value("infected").Array().NotContains("download", "share", "preview")
	})

	t.Run("GetContextWithAntivirusCustomActions", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		// Enable antivirus with custom restricted actions (blocking mode)
		conf.Contexts["test-context"] = map[string]interface{}{
			"antivirus": map[string]interface{}{
				"enabled": true,
				"address": "localhost:3310",
				"actions": map[string]interface{}{
					"pending":  []string{"delete"},
					"clean":    []string{"download", "share", "preview", "delete"},
					"infected": []string{"delete"},
					"error":    []string{"delete"},
					"skipped":  []string{"delete"},
				},
			},
		}
		defer func() {
			delete(conf.Contexts, "test-context")
		}()

		obj := e.GET("/settings/context").
			WithCookie(sessCookie, "connected").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		av := attrs.Value("antivirus").Object()
		av.HasValue("enabled", true)

		// Check custom actions matrix - pending files are blocked
		actions := av.Value("actions").Object()
		actions.Value("pending").Array().ContainsAll("delete")
		actions.Value("pending").Array().NotContainsAll("download", "share", "preview")
		actions.Value("clean").Array().ContainsAll("download", "share", "preview", "delete")
		actions.Value("infected").Array().ContainsAll("delete")
	})

	t.Run("PatchWithGoodRev", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		doc1, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		// We are going to patch an instance with newer values, and give the good rev
		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
      "data": {
        "type": "io.cozy.settings",
        "id": "io.cozy.settings.instance",
        "meta": {
          "rev": "%s"
        },
        "attributes": {
          "tz": "Europe/London",
          "email": "alice@example.org",
          "locale": "fr"
        }
      }
    }`, doc1.Rev()))).
			Expect().Status(200)
	})

	t.Run("PatchWithBadRev", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		// We are going to patch an instance with newer values, but with a totally
		// random rev
		rev := "6-2d9b7ef014d10549c2b4e206672d3e44"

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
        "data": {
          "type": "io.cozy.settings",
          "id": "io.cozy.settings.instance",
          "meta": {
            "rev": "%s"
          },
          "attributes": {
            "tz": "Europe/Berlin",
            "email": "alice@example.com",
            "locale": "en"
          }
        }
      }`, rev))).
			Expect().Status(409)
	})

	t.Run("PatchWithBadRevNoChanges", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		// We are defining a random rev, but make no changes in the instance values
		rev := "6-2d9b7ef014d10549c2b4e206672d3e44"

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
        "data": {
          "type": "io.cozy.settings",
          "id": "io.cozy.settings.instance",
          "meta": {
            "rev": "%s"
          },
          "attributes": {
            "tz": "Europe/London",
            "email": "alice@example.org",
            "locale": "fr"
          }
        }
      }`, rev))).
			Expect().Status(200)
	})

	t.Run("PatchWithBadRevAndChanges", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		// We are defining a random rev, but make changes in the instance values.
		// email is guarded, so the change has to be on an editable settings-doc
		// field (tz) to still trigger the conflict.
		rev := "6-2d9b7ef014d10549c2b4e206672d3e44"

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
        "data": {
          "type": "io.cozy.settings",
          "id": "io.cozy.settings.instance",
          "meta": {
            "rev": "%s"
          },
          "attributes": {
            "tz": "Asia/Tokyo",
            "email": "alice@example.com",
            "locale": "en"
          }
        }
      }`, rev))).
			Expect().Status(409)
	})

	t.Run("DiskUsage", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		obj := e.GET("/settings/disk-usage").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		e.GET("/settings/disk-usage").
			WithCookie(sessCookie, "connected").
			Expect().Status(401)

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.disk-usage")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("used", "0")
		attrs.HasValue("files", "0")
		attrs.HasValue("versions", "0")
	})

	t.Run("RegisterPassphraseWrongToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.POST("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "passphrase":     "MyFirstPassphrase",
        "iterations":     50000,
        "register_token": "BADBEEF",
      }`)).
			Expect().Status(400)

		e.POST("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "passphrase":     "MyFirstPassphrase",
        "iterations":     50000,
        "register_token": "XYZ",
      }`)).
			Expect().Status(400)
	})

	t.Run("RegisterPassphraseCorrectToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		res := e.POST("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithJSON(map[string]interface{}{
				"passphrase":     "MyFirstPassphrase",
				"iterations":     50000,
				"register_token": hex.EncodeToString(testInstance.RegisterToken),
				"key":            "xxx",
			}).
			Expect().Status(200)

		res.Cookies().Length().IsEqual(1)
		res.Cookie("cozysessid").Value().NotEmpty()
	})

	t.Run("UpdatePassphraseWithWrongPassphrase", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.PUT("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "new_passphrase":     "MyPassphrase",
        "current_passphrase": "BADBEEF",
        "iterations":         50000
      }`)).
			Expect().Status(400)
	})

	t.Run("UpdatePassphraseSuccess", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		res := e.PUT("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "new_passphrase":     "MyUpdatedPassphrase",
        "current_passphrase": "MyFirstPassphrase",
        "iterations":         50000
      }`)).
			Expect().Status(204)

		res.Cookies().Length().IsEqual(1)
		res.Cookie("cozysessid").Value().NotEmpty()
	})

	t.Run("UpdatePassphraseWithForce", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.PUT("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "new_passphrase": "MyPassphrase",
        "iterations":     50000,
        "force":          true
      }`)).
			Expect().Status(400)

		passwordDefined := false
		testInstance.PasswordDefined = &passwordDefined

		e.PUT("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithQuery("Force", true).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "new_passphrase": "MyPassphrase",
        "iterations":     50000,
        "force":          true
      }`)).
			Expect().Status(204)
	})

	t.Run("CheckPassphrase", func(t *testing.T) {
		t.Run("invalid", func(t *testing.T) {
			e := testutils.CreateTestClient(t, tsURL)

			e.POST("/settings/passphrase/check").
				WithCookie(sessCookie, "connected").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "passphrase": "Invalid Passphrase"
      }`)).
				Expect().Status(403)
		})

		t.Run("valid", func(t *testing.T) {
			e := testutils.CreateTestClient(t, tsURL)

			e.POST("/settings/passphrase/check").
				WithCookie(sessCookie, "connected").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "passphrase": "MyPassphrase"
      }`)).
				Expect().Status(204)
		})
	})

	t.Run("GetHint", func(t *testing.T) {
		t.Run("WithNoHint", func(t *testing.T) {
			e := testutils.CreateTestClient(t, tsURL)

			e.GET("/settings/hint").
				WithCookie(sessCookie, "connected").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(404)
		})

		t.Run("WithHint", func(t *testing.T) {
			e := testutils.CreateTestClient(t, tsURL)

			setting, err := settings.Get(testInstance)
			assert.NoError(t, err)
			setting.PassphraseHint = "my hint"
			err = couchdb.UpdateDoc(testInstance, setting)
			assert.NoError(t, err)

			e.GET("/settings/hint").
				WithCookie(sessCookie, "connected").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})
	})

	t.Run("UpdateHint", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.PUT("/settings/hint").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "hint": "my updated hint"
      }`)).
			Expect().Status(204)

		setting, err := settings.Get(testInstance)
		assert.NoError(t, err)
		assert.Equal(t, "my updated hint", setting.PassphraseHint)
	})

	t.Run("GetPassphraseParameters", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		obj := e.GET("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.passphrase")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("salt", "me@"+testInstance.Domain)
		attrs.HasValue("kdf", 0.0)
		attrs.HasValue("iterations", 50000)
	})

	t.Run("GetCapabilities", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)
		svc.On("GetLegalNoticeUrl", testInstance).Return("", nil).Once()

		e.GET("/settings/instance").
			WithCookie(sessCookie, "connected").
			Expect().Status(401)

		obj := e.GET("/settings/capabilities").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.capabilities")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("file_versioning", true)
		attrs.HasValue("can_auth_with_password", true)
		attrs.HasValue("can_auth_with_magic_links", false)
		attrs.HasValue("can_auth_with_oidc", false)
	})

	t.Run("GetInstance", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.GET("/settings/instance").
			WithCookie(sessCookie, "connected").
			Expect().Status(401)

		testInstance.RegisterToken = []byte("test")

		e.GET("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithQuery("registerToken", "74657374").
			Expect().Status(200)

		testInstance.RegisterToken = []byte{}
		svc.On("GetLegalNoticeUrl", testInstance).Return("https://testmanager.cozycloud.cc/tos/12345.pdf", nil).Once()

		obj := e.GET("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.instance")

		meta := data.Value("meta").Object()
		instanceRev = meta.Value("rev").String().NotEmpty().Raw()

		attrs := data.Value("attributes").Object()
		attrs.HasValue("email", "alice@example.com") // email is guarded: not changeable via PUT
		attrs.HasValue("tz", "Europe/London")
		attrs.HasValue("locale", "en")
		attrs.HasValue("password_defined", true)
		attrs.HasValue("legal_notice_url", "https://testmanager.cozycloud.cc/tos/12345.pdf")
		attrs.HasValue("org_domain", "test-org.example.com")
		attrs.HasValue("org_id", "test-org-id-456")
	})

	t.Run("GetInstanceDegradesWhenLegalNoticeFails", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)
		svc.On("GetLegalNoticeUrl", testInstance).
			Return("", fmt.Errorf("cloudery is down")).Once()

		obj := e.GET("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		attrs.NotContainsKey("legal_notice_url")
	})

	t.Run("UpdateInstance", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		obj := e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
        "data": {
          "type": "io.cozy.settings",
          "id": "io.cozy.settings.instance",
          "meta": {
            "rev": "%s"
          },
          "attributes": {
            "tz": "Europe/Paris",
            "email": "alice@example.net",
            "locale": "fr"
          }
        }
      }`, instanceRev))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.instance")

		meta := data.Value("meta").Object()
		instanceRev = meta.Value("rev").String().NotEmpty().NotEqual(instanceRev).Raw()

		attrs := data.Value("attributes").Object()
		attrs.HasValue("email", "alice@example.com") // email is guarded: not changeable via PUT
		attrs.HasValue("tz", "Europe/Paris")
		attrs.HasValue("locale", "fr")
	})

	t.Run("GetUpdatedInstance", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)
		svc.On("GetLegalNoticeUrl", testInstance).Return("", nil).Once()

		obj := e.GET("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Content-Type", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.instance")

		meta := data.Value("meta").Object()
		meta.HasValue("rev", instanceRev)

		attrs := data.Value("attributes").Object()
		attrs.HasValue("email", "alice@example.com") // email is guarded: not changeable via PUT
		attrs.HasValue("tz", "Europe/Paris")
		attrs.HasValue("locale", "fr")
	})

	t.Run("UpdatePassphraseWithTwoFactorAuth", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.PUT("/settings/instance/auth_mode").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/json").
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "auth_mode": "two_factor_mail"
      }`)).
			Expect().Status(204)

		mailPassCode, err := testInstance.GenerateMailConfirmationCode()
		require.NoError(t, err)

		e.PUT("/settings/instance/auth_mode").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/json").
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(fmt.Sprintf(`{
        "auth_mode": "two_factor_mail",
        "two_factor_activation_code": "%s"
      }`, mailPassCode))).
			Expect().Status(204)

		obj := e.PUT("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "current_passphrase": "MyPassphrase"
      }`)).
			Expect().Status(200).
			JSON().Object()

		obj.Value("two_factor_token").String().NotEmpty()

		twoFactorToken, twoFactorPasscode, err := testInstance.GenerateTwoFactorSecrets()
		require.NoError(t, err)

		e.PUT("/settings/passphrase").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithJSON(map[string]interface{}{
				"new_passphrase":      "MyLastPassphrase",
				"two_factor_token":    twoFactorToken,
				"two_factor_passcode": twoFactorPasscode,
			}).
			Expect().Status(204)
	})

	t.Run("ListClients", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.GET("/settings/clients").
			WithCookie(sessCookie, "connected").
			Expect().Status(401)

		client := &oauth.Client{
			RedirectURIs:    []string{"http:/localhost:4000/oauth/callback"},
			ClientName:      "Cozy-desktop on my-new-laptop",
			ClientKind:      "desktop",
			ClientURI:       "https://docs.cozy.io/en/mobile/desktop.html",
			LogoURI:         "https://docs.cozy.io/assets/images/cozy-logo-docs.svg",
			PolicyURI:       "https://cozy.io/policy",
			SoftwareID:      "/github.com/cozy-labs/cozy-desktop",
			SoftwareVersion: "0.16.0",
		}
		regErr := client.Create(testInstance)
		assert.Nil(t, regErr)
		oauthClientID = client.ClientID

		obj := e.GET("/settings/clients").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().IsEqual(2)

		el := data.Value(1).Object()
		el.HasValue("type", "io.cozy.oauth.clients")
		el.HasValue("id", client.ClientID)

		links := el.Value("links").Object()
		links.HasValue("self", "/settings/clients/"+client.ClientID)

		attrs := el.Value("attributes").Object()
		attrs.HasValue("client_name", client.ClientName)
		attrs.HasValue("client_kind", client.ClientKind)
		attrs.HasValue("client_uri", client.ClientURI)
		attrs.HasValue("logo_uri", client.LogoURI)
		attrs.HasValue("policy_uri", client.PolicyURI)
		attrs.HasValue("software_id", client.SoftwareID)
		attrs.HasValue("software_version", client.SoftwareVersion)
		attrs.NotContainsKey("client_secret")

		redirectURIs := attrs.Value("redirect_uris").Array()
		redirectURIs.Length().IsEqual(1)
		redirectURIs.Value(0).String().IsEqual(client.RedirectURIs[0])
	})

	t.Run("RevokeClient", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.DELETE("/settings/clients/"+oauthClientID).
			WithCookie(sessCookie, "connected").
			Expect().Status(401)

		e.DELETE("/settings/clients/"+oauthClientID).
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)

		obj := e.GET("/settings/clients").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().IsEqual(1)
	})

	t.Run("PatchInstanceSameParams", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		doc1, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(`{
          "data": {
            "type": "io.cozy.settings",
            "id": "io.cozy.settings.instance",
            "meta": {
              "rev": "%s"
            },
            "attributes": {
              "tz": "Europe/Paris",
              "email": "alice@example.net",
              "locale": "fr"
            }
          }
        }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().NotEmpty()

		doc2, err := testInstance.SettingsDocument()
		assert.NoError(t, err)

		// Assert no changes
		assert.Equal(t, doc1.Rev(), doc2.Rev())
	})

	t.Run("PatchInstanceChangeParams", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
          "data": {
            "type": "io.cozy.settings",
            "id": "io.cozy.settings.instance",
            "meta": {
              "rev": "%s"
            },
            "attributes": {
              "tz": "Antarctica/McMurdo",
              "email": "alice@expat.eu",
              "locale": "de"
            }
          }
        }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().NotEmpty()

		doc, err = testInstance.SettingsDocument()
		assert.NoError(t, err)

		assert.Equal(t, "Antarctica/McMurdo", doc.M["tz"].(string))
		// email is guarded: the value in the request body is ignored.
		assert.Equal(t, "alice@example.com", doc.M["email"].(string))
	})

	t.Run("PatchInstanceAddParam", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		doc1, err := testInstance.SettingsDocument()
		assert.NoError(t, err)

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
          "data": {
            "type": "io.cozy.settings",
            "id": "io.cozy.settings.instance",
            "meta": {
              "rev": "%s"
            },
            "attributes": {
              "tz": "Europe/Berlin",
              "email": "alice@example.com",
              "how_old_are_you": "42"
            }
          }
        }`, doc1.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().NotEmpty()

		doc2, err := testInstance.SettingsDocument()
		assert.NoError(t, err)
		assert.NotEqual(t, doc1.Rev(), doc2.Rev())
		assert.Equal(t, "42", doc2.M["how_old_are_you"].(string))
		assert.Equal(t, "Europe/Berlin", doc2.M["tz"].(string))
		assert.Equal(t, "alice@example.com", doc2.M["email"].(string))
	})

	t.Run("PatchInstanceRemoveParams", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		doc1, err := testInstance.SettingsDocument()
		assert.NoError(t, err)

		e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(fmt.Sprintf(`{
          "data": {
            "type": "io.cozy.settings",
            "id": "io.cozy.settings.instance",
            "meta": {
              "rev": "%s"
            },
            "attributes": {
              "tz": "Europe/Berlin"
            }
          }
        }`, doc1.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().NotEmpty()

		doc2, err := testInstance.SettingsDocument()
		assert.NoError(t, err)
		assert.NotEqual(t, doc1.Rev(), doc2.Rev())
		assert.Equal(t, "Europe/Berlin", doc2.M["tz"].(string))
		// email is guarded: it cannot be removed through a partial update.
		assert.Equal(t, "alice@example.com", doc2.M["email"].(string))
	})

	t.Run("UpdateInstanceGuardsEmailButNotPhoneWithoutSignup", func(t *testing.T) {
		// Without signup, email is still guarded (it must go through the verified
		// POST /settings/email flow), but phone stays editable.
		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)
		e := testutils.CreateTestClient(t, tsURL)
		obj := e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
        "data": {
          "type": "io.cozy.settings",
          "id": "io.cozy.settings.instance",
          "meta": {"rev": "%s"},
          "attributes": {
            "email": "attacker@evil.example",
            "phone": "+33123456789",
            "tz": "Indian/Mauritius"
          }
        }
      }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		attrs.HasValue("email", "alice@example.com") // guarded
		attrs.HasValue("phone", "+33123456789")      // editable without signup
		attrs.HasValue("tz", "Indian/Mauritius")
	})

	t.Run("UpdateInstanceGuardsPhoneAndRecoveryWhenSignup", func(t *testing.T) {
		// With signup, phone and recovery_email are also owned by the signup
		// flow, so they are locked too (phone keeps the value set just above).
		conf.Contexts["test-context"] = map[string]interface{}{"signup_url": "https://signup.example.org"}
		defer delete(conf.Contexts, "test-context")

		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)
		e := testutils.CreateTestClient(t, tsURL)
		obj := e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
        "data": {
          "type": "io.cozy.settings",
          "id": "io.cozy.settings.instance",
          "meta": {"rev": "%s"},
          "attributes": {
            "email": "attacker@evil.example",
            "phone": "+10000000000",
            "recovery_email": "attacker-recovery@evil.example",
            "tz": "Pacific/Auckland"
          }
        }
      }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Value("data").Object().Value("attributes").Object()
		attrs.HasValue("email", "alice@example.com") // guarded
		attrs.HasValue("phone", "+33123456789")      // guarded on signup: not changed
		attrs.NotContainsKey("recovery_email")       // guarded on signup: not set
		attrs.HasValue("tz", "Pacific/Auckland")
	})

	t.Run("PostEmailBlockedWhenSignupEnabled", func(t *testing.T) {
		// The dedicated email-change flow is also disabled on signup-managed
		// contexts.
		conf.Contexts["test-context"] = map[string]interface{}{"signup_url": "https://signup.example.org"}
		defer delete(conf.Contexts, "test-context")

		e := testutils.CreateTestClient(t, tsURL)
		e.POST("/settings/email").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{"passphrase": "MyPassphrase", "email": "new@example.com"}`)).
			Expect().Status(403)
	})

	t.Run("FeatureFlags", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		_ = couchdb.DeleteDB(prefixer.GlobalPrefixer, consts.Settings)
		t.Cleanup(func() { _ = couchdb.DeleteDB(prefixer.GlobalPrefixer, consts.Settings) })

		obj := e.GET("/settings/flags").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.flags")

		data.Value("attributes").Object().IsEmpty()

		testInstance.FeatureFlags = map[string]interface{}{
			"from_instance_flag":   true,
			"from_multiple_source": "instance_flag",
			"json_object":          map[string]interface{}{"foo": "bar"},
		}
		testInstance.FeatureSets = []string{"set1", "set2"}
		require.NoError(t, instance.Update(testInstance))

		cache := config.GetConfig().CacheStorage

		cacheKey := fmt.Sprintf("flags:%s:%v", testInstance.ContextName, testInstance.FeatureSets)
		buf, err := json.Marshal(map[string]interface{}{
			"from_feature_sets":    true,
			"from_multiple_source": "manager",
		})
		assert.NoError(t, err)
		cache.Set(cacheKey, buf, 5*time.Second)
		ctxFlags := couchdb.JSONDoc{Type: consts.Settings}
		ctxFlags.M = map[string]interface{}{
			"ratio_0": []map[string]interface{}{
				{"ratio": 0, "value": "context"},
			},
			"ratio_1": []map[string]interface{}{
				{"ratio": 1, "value": "context"},
			},
			"ratio_0.000001": []map[string]interface{}{
				{"ratio": 0.000001, "value": "context"},
			},
			"ratio_0.999999": []map[string]interface{}{
				{"ratio": 0.999999, "value": "context"},
			},
		}

		id := fmt.Sprintf("%s.%s", consts.ContextFlagsSettingsID, testInstance.ContextName)
		ctxFlags.SetID(id)
		err = couchdb.CreateNamedDocWithDB(prefixer.GlobalPrefixer, &ctxFlags)
		assert.NoError(t, err)
		defFlags := couchdb.JSONDoc{Type: consts.Settings}
		defFlags.M = map[string]interface{}{
			"ratio_0":              "defaults",
			"ratio_1":              "defaults",
			"ratio_0.000001":       "defaults",
			"ratio_0.999999":       "defaults",
			"from_multiple_source": "defaults",
			"from_defaults":        true,
		}
		defFlags.SetID(consts.DefaultFlagsSettingsID)
		err = couchdb.CreateNamedDocWithDB(prefixer.GlobalPrefixer, &defFlags)
		assert.NoError(t, err)

		obj = e.GET("/settings/flags").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data = obj.Value("data").Object()
		data.HasValue("type", "io.cozy.settings")
		data.HasValue("id", "io.cozy.settings.flags")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("from_instance_flag", true)
		attrs.HasValue("from_feature_sets", true)
		attrs.HasValue("from_defaults", true)
		attrs.HasValue("json_object", testInstance.FeatureFlags["json_object"])
		attrs.HasValue("from_multiple_source", "instance_flag")
		attrs.HasValue("ratio_0", "defaults")
		attrs.HasValue("ratio_0.000001", "defaults")
		attrs.HasValue("ratio_0.999999", "context")
		attrs.HasValue("ratio_1", "context")
	})

	// Verify common settings version bump on instance update
	t.Run("UpdateInstanceTriggersCommonSettings", func(t *testing.T) {
		// Stub common settings HTTP to succeed and verify version bump
		oldDo := cscommon.DoCommonHTTP
		cscommon.DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return nil }
		defer func() { cscommon.DoCommonHTTP = oldDo }()

		// Configure common_settings so the code path is enabled
		conf := config.GetConfig()
		conf.CommonSettings = map[string]config.CommonSettings{
			config.DefaultInstanceContext: {URL: "http://example.org", Token: "test-token"},
			"test-context":                {URL: "http://example.org", Token: "test-token"},
		}

		// Ensure starting version is 0 to hit CreateCommonSettings path
		prev := 0
		testInstance.CommonSettingsVersion = prev

		// Use the current settings revision to avoid conflict
		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		// create
		e := testutils.CreateTestClient(t, tsURL)
		obj := e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
                "data": {
                  "type": "io.cozy.settings",
                  "id": "io.cozy.settings.instance",
                  "meta": {"rev": "%s"},
                  "attributes": {"email": "bob@example.net", "public_name": "Bob Jones"}
                }
              }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		var doGet = cscommon.GetRemoteCommonSettings
		cscommon.GetRemoteCommonSettings = func(inst *instance.Instance) (*cscommon.UserSettingsRequest, error) {
			return &cscommon.UserSettingsRequest{
				Version: 1,
				Payload: cscommon.UserSettingsPayload{
					DisplayName: "Bob Jones",
					Email:       "bob@example.net",
				},
			}, nil
		}
		defer func() { cscommon.GetRemoteCommonSettings = doGet }()

		// update
		doc, err = testInstance.SettingsDocument()
		obj = e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
                "data": {
                  "type": "io.cozy.settings",
                  "id": "io.cozy.settings.instance",
                  "meta": {"rev": "%s"},
                  "attributes": {"email": "alice@example.net", "public_name": "Alice Jones"}
                }
              }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Check instance rev bumped in response and common settings version increased
		data := obj.Value("data").Object()
		meta := data.Value("meta").Object()
		instanceRev = meta.Value("rev").String().NotEmpty().Raw()
		assert.Greater(t, testInstance.CommonSettingsVersion, prev)

		// Confirm it's persisted by reloading the instance
		reloaded, err := instance.Get(testInstance.Domain)
		require.NoError(t, err)
		assert.Equal(t, testInstance.CommonSettingsVersion, reloaded.CommonSettingsVersion)
	})

	// Verify common settings version bump on instance update
	t.Run("UpdateInstanceTriggersCommonSettings_VersionMismatch", func(t *testing.T) {
		// Stub common settings HTTP to succeed and verify version bump
		oldDo := cscommon.DoCommonHTTP
		cscommon.DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return nil }
		defer func() { cscommon.DoCommonHTTP = oldDo }()

		// Configure common_settings so the code path is enabled
		conf := config.GetConfig()
		conf.CommonSettings = map[string]config.CommonSettings{
			config.DefaultInstanceContext: {URL: "http://example.org", Token: "test-token"},
			"test-context":                {URL: "http://example.org", Token: "test-token"},
		}

		// Ensure starting version is 0 to hit CreateCommonSettings path
		prev := 0
		testInstance.CommonSettingsVersion = prev

		// Use the current settings revision to avoid conflict
		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		// create
		e := testutils.CreateTestClient(t, tsURL)
		obj := e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
                "data": {
                  "type": "io.cozy.settings",
                  "id": "io.cozy.settings.instance",
                  "meta": {"rev": "%s"},
                  "attributes": {"email": "bob@example.net", "public_name": "Bob Jones"}
                }
              }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		var doGet = cscommon.GetRemoteCommonSettings
		cscommon.GetRemoteCommonSettings = func(inst *instance.Instance) (*cscommon.UserSettingsRequest, error) {
			return &cscommon.UserSettingsRequest{
				Version: 2,
				Payload: cscommon.UserSettingsPayload{
					DisplayName: "Bob Jones",
					Email:       "bob@example.net",
				},
			}, nil
		}
		defer func() { cscommon.GetRemoteCommonSettings = doGet }()

		// update
		doc, err = testInstance.SettingsDocument()
		obj = e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
                "data": {
                  "type": "io.cozy.settings",
                  "id": "io.cozy.settings.instance",
                  "meta": {"rev": "%s"},
                  "attributes": {"email": "alice@example.net", "public_name": "Alice Jones"}
                }
              }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Check instance rev bumped the same and common setting version is unchanged
		data := obj.Value("data").Object()
		meta := data.Value("meta").Object()
		instanceRev = meta.Value("rev").String().NotEmpty().Raw()
		assert.Equal(t, testInstance.CommonSettingsVersion, 1)

		// Confirm it's persisted by reloading the instance
		reloaded, err := instance.Get(testInstance.Domain)
		require.NoError(t, err)
		assert.Equal(t, testInstance.CommonSettingsVersion, reloaded.CommonSettingsVersion)
	})

	// Verify common settings version bump on instance update
	t.Run("UpdateInstanceTriggersCommonSettings_VersionMismatch_SettingsUnchanged", func(t *testing.T) {
		// Stub common settings HTTP to succeed and verify version bump
		oldDo := cscommon.DoCommonHTTP
		cscommon.DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return nil }
		defer func() { cscommon.DoCommonHTTP = oldDo }()

		// Configure common_settings so the code path is enabled
		conf := config.GetConfig()
		conf.CommonSettings = map[string]config.CommonSettings{
			config.DefaultInstanceContext: {URL: "http://example.org", Token: "test-token"},
			"test-context":                {URL: "http://example.org", Token: "test-token"},
		}

		// Ensure starting version is 0 to hit CreateCommonSettings path
		prev := 0
		testInstance.CommonSettingsVersion = prev

		// Use the current settings revision to avoid conflict
		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		// create
		e := testutils.CreateTestClient(t, tsURL)
		obj := e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
                "data": {
                  "type": "io.cozy.settings",
                  "id": "io.cozy.settings.instance",
                  "meta": {"rev": "%s"},
                  "attributes": {"email": "bob@example.net", "public_name": "Bob Jones"}
                }
              }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		var doGet = cscommon.GetRemoteCommonSettings
		cscommon.GetRemoteCommonSettings = func(inst *instance.Instance) (*cscommon.UserSettingsRequest, error) {
			return &cscommon.UserSettingsRequest{
				Version: 2,
				Payload: cscommon.UserSettingsPayload{
					DisplayName: "Bob Jones",
					Email:       "bob@example.net",
				},
			}, nil
		}
		defer func() { cscommon.GetRemoteCommonSettings = doGet }()

		// update
		doc, err = testInstance.SettingsDocument()
		obj = e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
                "data": {
                  "type": "io.cozy.settings",
                  "id": "io.cozy.settings.instance",
                  "meta": {"rev": "%s"},
                  "attributes": {"email": "bob@example.net", "public_name": "Bob Jones"}
                }
              }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Check instance rev bumped the same and common setting version is unchanged
		data := obj.Value("data").Object()
		meta := data.Value("meta").Object()
		instanceRev = meta.Value("rev").String().NotEmpty().Raw()
		assert.Equal(t, testInstance.CommonSettingsVersion, 1)

		// Confirm it's persisted by reloading the instance
		reloaded, err := instance.Get(testInstance.Domain)
		require.NoError(t, err)
		assert.Equal(t, testInstance.CommonSettingsVersion, reloaded.CommonSettingsVersion)
	})

	// Verify common settings NOT updated when only non-common fields change
	t.Run("NonCommonFieldsDoNotUpdateCommonSettings", func(t *testing.T) {
		// Stub common settings HTTP to succeed and verify version bump
		oldDo := cscommon.DoCommonHTTP
		cscommon.DoCommonHTTP = func(method, urlStr, token string, body []byte) error { return nil }
		defer func() { cscommon.DoCommonHTTP = oldDo }()

		// Ensure common_settings is enabled
		conf := config.GetConfig()
		conf.CommonSettings = map[string]config.CommonSettings{
			config.DefaultInstanceContext: {URL: "http://127.0.0.1:9", Token: "test-token"},
			"test-context":                {URL: "http://127.0.0.1:9", Token: "test-token"},
		}

		prev := testInstance.CommonSettingsVersion

		// Use current settings rev to avoid conflict
		doc, err := testInstance.SettingsDocument()
		require.NoError(t, err)

		e := testutils.CreateTestClient(t, tsURL)
		_ = e.PUT("/settings/instance").
			WithCookie(sessCookie, "connected").
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(fmt.Sprintf(`{
		        "data": {
		          "type": "io.cozy.settings",
		          "id": "io.cozy.settings.instance",
		          "meta": {"rev": "%s"},
		          "attributes": {"how_old_are_you": "99"}
		        }
		      }`, doc.Rev()))).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		// Confirm common settings version did not change
		assert.Equal(t, prev, testInstance.CommonSettingsVersion)
		reloaded, err := instance.Get(testInstance.Domain)
		require.NoError(t, err)
		assert.Equal(t, prev, reloaded.CommonSettingsVersion)
	})

	t.Run("ClientsLimitExceededWithoutSession", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.GET("/settings/clients/limit-exceeded").
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(401)
	})

	t.Run("ClientsLimitExceededWithoutLimit", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8").
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(302).
			Header("location").IsEqual(testInstance.DefaultRedirection().String())

		redirect := "cozy://my-app"
		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithQuery("redirect", redirect).
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8").
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(302).
			Header("location").IsEqual(redirect)
	})

	t.Run("ClientsLimitExceededWithLimitExceeded", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(0))

		// Create the OAuth client for the flagship app
		flagship := oauth.Client{
			RedirectURIs: []string{"cozy://flagship"},
			ClientName:   "flagship-app",
			ClientKind:   "mobile",
			SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
			Flagship:     true,
		}
		require.Nil(t, flagship.Create(testInstance, oauth.NotPending))
		defer flagship.Delete(testInstance)

		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains("Disconnect one of your devices or change your Twake offer to access your Twake from this device.").
			Contains("/#/connectedDevices").
			NotContains("http://manager.example.org")

		testutils.WithManager(t, testInstance, testutils.ManagerConfig{URL: "http://manager.example.org", WithPremiumLinks: true})

		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains("Disconnect one of your devices or change your Twake offer to access your Twake from this device.").
			Contains("/#/connectedDevices").
			Contains("http://manager.example.org")

		testutils.WithFlag(t, testInstance, "flagship.iap.enabled", true)

		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithQuery("isFlagship", true).
			WithHeader("Authorization", "Bearer "+token).
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains("Disconnect one of your devices or change your Twake offer to access your Twake from this device.").
			Contains("/#/connectedDevices").
			NotContains("http://manager.example.org")

		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithQuery("isFlagship", true).
			WithQuery("isIapAvailable", true).
			WithHeader("Authorization", "Bearer "+token).
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains("Disconnect one of your devices or change your Twake offer to access your Twake from this device.").
			Contains("/#/connectedDevices").
			Contains("http://manager.example.org")
	})

	t.Run("ClientsLimitExceededWithLimitReached", func(t *testing.T) {
		e := testutils.CreateTestClient(t, tsURL)

		clients, _, err := oauth.GetConnectedUserClients(testInstance, 100, "")
		require.NoError(t, err)

		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(len(clients)))

		e.GET("/settings/clients/limit-exceeded").
			WithCookie(sessCookie, "connected").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8").
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(302).
			Header("location").IsEqual(testInstance.DefaultRedirection().String())
	})

	t.Run("Avatar", func(t *testing.T) {
		t.Run("Put", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)
			sessCookie := session.CookieName(testInstance)

			// Create a sample avatar image
			avatarContent := "fake image content"

			e.PUT("/settings/avatar").
				WithCookie(sessCookie, "connected").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "image/png").
				WithBytes([]byte(avatarContent)).
				Expect().Status(204)

			// Verify common settings version persisted increased
			reloaded, err := instance.Get(testInstance.Domain)
			require.NoError(t, err)
			assert.Greater(t, reloaded.CommonSettingsVersion, 0)
		})

		t.Run("Delete", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)
			sessCookie := session.CookieName(testInstance)
			e.DELETE("/settings/avatar").
				WithCookie(sessCookie, "connected").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)

			// Verify common settings version persisted increased again
			reloaded, err := instance.Get(testInstance.Domain)
			require.NoError(t, err)
			assert.Greater(t, reloaded.CommonSettingsVersion, 0)
		})
	})
}

func TestRegisterPassphraseForFlagshipApp(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	oauthClient := &oauth.Client{
		RedirectURIs:    []string{"http:/localhost:4000/oauth/callback"},
		ClientName:      "Cozy-desktop on my-new-laptop",
		ClientKind:      "desktop",
		ClientURI:       "https://docs.cozy.io/en/mobile/desktop.html",
		LogoURI:         "https://docs.cozy.io/assets/images/cozy-logo-docs.svg",
		PolicyURI:       "https://cozy.io/policy",
		SoftwareID:      "/github.com/cozy-labs/cozy-desktop",
		SoftwareVersion: "0.16.0",
	}

	setupFlagship := testutils.NewSetup(t, t.Name())
	testInstance := setupFlagship.GetTestInstance(&lifecycle.Options{
		Locale:      "en",
		Timezone:    "Europe/Berlin",
		Email:       "alice2@example.com",
		ContextName: "test-context",
	})

	svc := csettings.NewServiceMock(t)
	tsURL := setupRouter(t, testInstance, svc).URL

	require.Nil(t, oauthClient.Create(testInstance))
	client, err := oauth.FindClient(testInstance, oauthClient.ClientID)
	require.NoError(t, err)
	require.NoError(t, client.SetFlagship(testInstance))

	e := httpexpect.Default(t, tsURL)
	obj := e.POST("/settings/passphrase/flagship").
		WithJSON(map[string]interface{}{
			"passphrase":     "MyFirstPassphrase",
			"iterations":     50000,
			"register_token": hex.EncodeToString(testInstance.RegisterToken),
			"key":            "xxx-key-xxx",
			"public_key":     "xxx-public-key-xxx",
			"private_key":    "xxx-private-key-xxx",
			"client_id":      client.CouchID,
			"client_secret":  client.ClientSecret,
		}).
		Expect().Status(200).
		JSON().Object()

	obj.Value("access_token").String().NotEmpty()
	obj.Value("refresh_token").String().NotEmpty()
	obj.HasValue("scope", "*")
	obj.HasValue("token_type", "bearer")
}
