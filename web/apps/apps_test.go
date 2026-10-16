// spec package is introduced to avoid circular dependencies since this
// particular test requires to depend on routing directly to expose the API and
// the APP server.
package apps_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sync/atomic"
	"testing"
	"time"

	apps "github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/intent"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/model/stack"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	"github.com/cozy/cozy-stack/pkg/assets/model"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/filetype"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	webApps "github.com/cozy/cozy-stack/web/apps"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/gavv/httpexpect/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const domain = "cozywithapps.example.net"

func TestApps(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	config.GetConfig().Assets = "../../assets"
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	setup.SetupSwiftTest()

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")
	tempdir := t.TempDir()

	cfg := config.GetConfig()
	cfg.Fs.URL = &url.URL{
		Scheme: "file",
		Host:   "localhost",
		Path:   tempdir,
	}
	was := cfg.Subdomains
	cfg.Subdomains = config.NestedSubdomains
	defer func() { cfg.Subdomains = was }()

	pass := "aephe2Ei"
	testInstance := setup.GetTestInstance(&lifecycle.Options{Domain: domain})
	params := lifecycle.PassParameters{
		Key:        "fake-encrypt-key",
		Iterations: 0,
	}
	_ = lifecycle.ForceUpdatePassphrase(testInstance, []byte(pass), params)
	testInstance.RegisterToken = nil
	testInstance.OnboardingFinished = true
	_ = instance.Update(testInstance)

	slug, err := setup.InstallMiniApp()
	require.NoError(t, err, "Could not install mini app")

	konnectorSlug, err := setup.InstallMiniKonnector()
	require.NoError(t, err, "Could not install mini konnector")

	clientSideSlug, err := setup.InstallMiniClientSideKonnector()
	require.NoError(t, err, "Could not install miniClientSideKonnector konnector")

	ts := setup.GetTestServer("/apps", webApps.WebappsRoutes, func(r *echo.Echo) *echo.Echo {
		r.POST("/login", func(c echo.Context) error {
			sess, _ := session.New(testInstance, session.LongRun, "")
			cookie, _ := sess.ToCookie()
			c.SetCookie(cookie)
			return c.HTML(http.StatusOK, "OK")
		})
		r.POST("/auth/session_code", auth.CreateSessionCode)
		router, err := web.CreateSubdomainProxy(r, &stack.Services{}, webApps.Serve)
		require.NoError(t, err, "Cant start subdoman proxy")
		return router
	})
	t.Cleanup(ts.Close)

	// Login
	cozysessID := testutils.CreateTestClient(t, ts.URL).POST("/login").
		WithHost(testInstance.Domain).
		Expect().Status(200).
		Cookie("cozysessid").Value().Raw()

	_, token := setup.GetTestClient(consts.Apps + " " + consts.Konnectors)

	t.Run("Serve", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		eAnon := testutils.CreateTestClient(t, ts.URL)

		assertNotPublic(e, slug, testInstance.Domain, "/foo", 302, "https://cozywithapps.example.net/auth/login?redirect=https%3A%2F%2Fmini.cozywithapps.example.net%2Ffoo")
		assertNotPublic(e, slug, testInstance.Domain, "/foo/hello.tml", 401, "")

		e = e.Builder(func(r *httpexpect.Request) {
			r.WithCookie("cozysessid", cozysessID)
		})

		assertAuthGet(e, slug, testInstance.Domain, "/foo/", "text/html", "utf-8", `this is index.html. <a lang="en" href="https://cozywithapps.example.net/status/">Status</a>`)
		assertAuthGet(e, slug, testInstance.Domain, "/foo/hello.html", "text/html", "utf-8", "world {{.Token}}")
		assertAuthGet(e, slug, testInstance.Domain, "/public", "text/html", "utf-8", "this is a file in public/")
		assertAuthGet(e, slug, testInstance.Domain, "/public/index.html", "text/html", "utf-8", "this is a file in public/")
		assertAnonGet(eAnon, slug, testInstance.Domain, "/public", "text/html", "utf-8", "this is a file in public/")
		assertAnonGet(eAnon, slug, testInstance.Domain, "/public/index.html", "text/html", "utf-8", "this is a file in public/")
		assertBodyContainsUnescaped(
			t,
			e.GET("/public").
				WithHost(slug+"."+testInstance.Domain).
				Expect().Status(200).
				Body().Raw(),
			`"isLoggedIn":true`,
		)
		assertBodyContainsUnescaped(
			t,
			eAnon.GET("/public").
				WithHost(slug+"."+testInstance.Domain).
				Expect().Status(200).
				Body().Raw(),
			`"isLoggedIn":false`,
		)
		assertNotFound(e, slug, testInstance.Domain, "/404")
		assertNotFound(e, slug, testInstance.Domain, "/")
		assertNotFound(e, slug, testInstance.Domain, "/index.html")
		assertNotFound(e, slug, testInstance.Domain, "/public/hello.html")
		assertInternalServerError(e, slug, testInstance.Domain, "/invalid")
	})

	t.Run("ServeWithClientsLimitExceeded", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the OAuth client for the flagship app
		flagship := oauth.Client{
			RedirectURIs: []string{"cozy://flagship"},
			ClientName:   "flagship-app",
			ClientKind:   "mobile",
			SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
			Flagship:     true,
		}
		require.Nil(t, flagship.Create(testInstance, oauth.NotPending))

		testutils.WithFlag(t, testInstance, "cozy.oauthclients.max", float64(0))

		e = e.Builder(func(r *httpexpect.Request) {
			r.WithCookie("cozysessid", cozysessID)
		})

		assertAuthGet(e, slug, testInstance.Domain, "/public", "text/html", "utf-8", "this is a file in public/")
		assertAnonGet(e, slug, testInstance.Domain, "/public", "text/html", "utf-8", "this is a file in public/")

		redirect := testInstance.SubDomain(slug)
		redirect.Path = "/foo"
		location := testInstance.PageURL("/settings/clients/limit-exceeded", url.Values{"redirect": {redirect.String()}})
		assertRedirect(e, slug, testInstance.Domain, "/foo", 303, location)

		assertAuthGet(e, slug, testInstance.Domain, "/foo/hello.html", "text/html", "utf-8", "world {{.Token}}")

		testInstance.FeatureFlags = map[string]interface{}{}
		require.NoError(t, instance.Update(testInstance))
	})

	t.Run("CozyBar", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/bar/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains(`<link rel="stylesheet" type="text/css" href="//cozywithapps.example.net/assets/css/cozy-bar`).
			Contains(`<script src="//cozywithapps.example.net/assets/js/cozy-bar`)
	})

	t.Run("Warnings", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Moved instance warning

		testInstance.Moved = true
		require.NoError(t, instance.Update(testInstance))

		e.GET("/foo/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains(`<meta name="user-action-required" data-title="Cozy has been moved" data-code="moved" data-detail="The Cozy has been moved to a new address"`)

		testInstance.Moved = false
		require.NoError(t, instance.Update(testInstance))

		// TOS not signed warning

		testutils.WithManager(t, testInstance, testutils.ManagerConfig{URL: "http://manager.example.org", WithPremiumLinks: true})

		tosSigned := testInstance.TOSSigned
		tosLatest := testInstance.TOSLatest
		tomorrow := time.Now().Add(24 * time.Hour)
		testInstance.TOSSigned = "1.0.0-20170901"
		testInstance.TOSLatest = "2.0.0-" + tomorrow.Format("20060102")
		require.NoError(t, instance.Update(testInstance))

		notSigned, deadline := testInstance.CheckTOSNotSignedAndDeadline()
		require.True(t, notSigned)
		require.Equal(t, deadline, instance.TOSWarning)

		tosLink, err := testInstance.ManagerURL(instance.ManagerTOSURL)
		require.NoError(t, err)
		require.NotEmpty(t, tosLink)

		e.GET("/foo/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			HasContentType("text/html", "utf-8").
			Body().
			Contains(`<meta name="user-action-required" data-title="TOS Updated" data-code="tos-updated" data-detail="Terms of services have been updated" data-links="` + tosLink + `"`)

		testInstance.TOSSigned = tosSigned
		testInstance.TOSLatest = tosLatest
		require.NoError(t, instance.Update(testInstance))
	})

	t.Run("ServeWithAnIntents", func(t *testing.T) {
		require.NoError(t, permission.ForceWebapp(testInstance, "test-app", permission.Set{}))
		e := testutils.CreateTestClient(t, ts.URL)

		intent := &intent.Intent{
			Action: "PICK",
			Type:   "io.cozy.foos",
			Client: "io.cozy.apps/test-app",
		}
		err := intent.Save(testInstance)
		require.NoError(t, err)
		err = intent.FillServices(testInstance)
		require.NoError(t, err)
		require.Len(t, intent.Services, 1)
		err = intent.Save(testInstance)
		require.NoError(t, err)

		u, err := url.Parse(intent.Services[0].Href)
		require.NoError(t, err)

		resp := e.GET(u.Path).
			WithHost(slug+"."+testInstance.Domain).
			WithQueryString(u.RawQuery).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200)

		csp := resp.Header(echo.HeaderContentSecurityPolicy)
		csp.Contains("script-src")
		csp.Contains("'wasm-unsafe-eval'")
		csp.Contains("connect-src blob:")
		csp.Contains("worker-src 'self' blob:;")
		csp.Contains("frame-ancestors 'self' https://test-app.cozywithapps.example.net;")

		body := resp.Body()
		body.Contains(`<script type="application/json" id="cozy-intent">`)
		body.Contains(intent.ID())
		body.Contains("PICK")
		body.Contains("io.cozy.foos")
		body.Contains("https://test-app.cozywithapps.example.net")
	})

	t.Run("ServeWithoutIntentDoesNotInline", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		body := e.GET("/foo").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Body()

		body.NotContains(`id="cozy-intent"`)
	})

	t.Run("ServeWithAnIntentsClientURL", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		clientURLApp := &couchdb.JSONDoc{
			Type: consts.Apps,
			M: map[string]interface{}{
				"_id":             consts.Apps + "/clienturl-app",
				"name":            "ClientURLApp",
				"slug":            "clienturl-app",
				"source":          "git://github.com/cozy/clienturl.git",
				"state":           apps.Ready,
				"client_url_flag": "clienturl_csp_flag",
				"routes": apps.Routes{
					"/foo": apps.Route{
						Folder: "/",
						Index:  "index.html",
						Public: false,
					},
				},
				"permissions": permission.Set{},
				"version":     "1.0.0",
			},
		}
		require.NoError(t, couchdb.CreateNamedDoc(testInstance, clientURLApp))
		require.NoError(t, permission.ForceWebapp(testInstance, "clienturl-app", permission.Set{}))
		t.Cleanup(func() { _ = couchdb.DeleteDoc(testInstance, clientURLApp) })

		testutils.WithFlag(t, testInstance, "clienturl_csp_flag", "https://external.example.com")

		intent := &intent.Intent{
			Action: "PICK",
			Type:   "io.cozy.foos",
			Client: "io.cozy.apps/clienturl-app",
		}
		err := intent.Save(testInstance)
		require.NoError(t, err)
		err = intent.FillServices(testInstance)
		require.NoError(t, err)
		require.Len(t, intent.Services, 1)
		err = intent.Save(testInstance)
		require.NoError(t, err)

		u, err := url.Parse(intent.Services[0].Href)
		require.NoError(t, err)

		csp := e.GET(u.Path).
			WithHost(slug+"."+testInstance.Domain).
			WithQueryString(u.RawQuery).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Header(echo.HeaderContentSecurityPolicy)
		csp.Contains("frame-ancestors 'self' https://external.example.com https://clienturl-app.cozywithapps.example.net;")
	})

	t.Run("ServeDataProxyWithIntentAllowsContainerAppsFrame", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		require.NoError(t, setup.InstallMiniDataProxy(t))
		containerApp := &couchdb.JSONDoc{
			Type: consts.Apps,
			M: map[string]interface{}{
				"_id":             consts.Apps + "/container-app",
				"name":            "ContainerApp",
				"slug":            "container-app",
				"source":          "git://github.com/cozy/containerapp.git",
				"state":           apps.Ready,
				"client_url_flag": "container_csp_flag",
				"routes":          apps.Routes{},
				"permissions":     permission.Set{},
				"version":         "1.0.0",
			},
		}
		require.NoError(t, couchdb.CreateNamedDoc(testInstance, containerApp))
		t.Cleanup(func() { _ = couchdb.DeleteDoc(testInstance, containerApp) })
		require.NoError(t, permission.ForceWebapp(testInstance, "container-app", permission.Set{}))
		t.Cleanup(func() { _ = permission.DestroyWebapp(testInstance, "container-app") })

		testutils.WithFlag(t, testInstance, "container_csp_flag", "https://external.example.com")

		intent := &intent.Intent{
			Action:   "PICK",
			Type:     "io.cozy.foos",
			Client:   "io.cozy.apps/container-app",
			Services: []intent.Service{{Slug: consts.DataProxySlug, Href: "/"}},
		}
		require.NoError(t, intent.Save(testInstance))

		csp := e.GET("/").
			WithHost(consts.DataProxySlug + "." + testInstance.Domain).
			WithQueryString("intent=" + intent.ID()).
			Expect().Status(200).
			Header(echo.HeaderContentSecurityPolicy)

		csp.Contains("frame-ancestors 'self'")
		// Cozy root (login page rule)
		csp.Contains("https://cozywithapps.example.net/")
		// All container apps and their external clients
		csp.Contains("https://external.example.com")
		csp.Contains("https://container-app.cozywithapps.example.net")
		// Apps without a client_url_flag are not added
		csp.NotContains("https://mini.cozywithapps.example.net")

		t.Run("LoggedIn", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)
			response := e.GET("/").WithHost(consts.DataProxySlug+"."+testInstance.Domain).
				WithQuery("intent", intent.ID()).WithCookie("cozysessid", cozysessID).
				Expect().Status(http.StatusOK)
			response.Header(echo.HeaderContentSecurityPolicy).Contains("https://external.example.com")
			match := regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(response.Body().Raw()))
			require.Len(t, match, 2)
			var claims permission.Claims
			require.NoError(t, crypto.ParseJWT(match[1], func(*jwt.Token) (interface{}, error) { return testInstance.PickKey(consts.AppAudience) }, &claims))
			require.Equal(t, "container-app", claims.Subject)
			require.Equal(t, consts.DataProxySlug, claims.Actor.Subject)
			require.Equal(t, intent.ID(), claims.IntentID)
			e.GET("/intents/"+intent.ID()).WithHost(testInstance.Domain).
				WithCookie("cozysessid", cozysessID).WithHeader("Authorization", "Bearer "+match[1]).
				Expect().Status(http.StatusOK)
		})
	})

	t.Run("ServeAddsNoExternalIntentServiceWithoutServiceURLFlag", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// The flag has a valid value, but no app declares it as its
		// service_url_flag: nothing is added to frame-src
		testutils.WithFlag(t, testInstance, "extservice_url_flag", "https://calendar.external.example.com/app/")
		csp := e.GET("/foo/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Header(echo.HeaderContentSecurityPolicy)
		csp.NotContains("calendar.external.example.com")
	})

	t.Run("ServeAddsExternalIntentServicesToFrameSrc", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		externalServiceApp := &couchdb.JSONDoc{
			Type: consts.Apps,
			M: map[string]interface{}{
				"_id":              consts.Apps + "/extservice-app",
				"name":             "ExtServiceApp",
				"slug":             "extservice-app",
				"source":           "git://github.com/cozy/extservice.git",
				"state":            apps.Ready,
				"service_url_flag": "extservice_url_flag",
				"routes":           apps.Routes{},
				"permissions":      permission.Set{},
				"version":          "1.0.0",
			},
		}
		require.NoError(t, couchdb.CreateNamedDoc(testInstance, externalServiceApp))
		t.Cleanup(func() { _ = couchdb.DeleteDoc(testInstance, externalServiceApp) })

		// Flag absent: nothing is added
		csp := e.GET("/foo/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Header(echo.HeaderContentSecurityPolicy)
		csp.NotContains("calendar.external.example.com")
		csp.NotContains("https://extservice-app.cozywithapps.example.net")

		// Flag set: its origin (not its path) is added to frame-src
		testutils.WithFlag(t, testInstance, "extservice_url_flag", "https://calendar.external.example.com/app/")
		csp = e.GET("/foo/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Header(echo.HeaderContentSecurityPolicy)
		csp.Match(`frame-src [^;]*https://calendar\.external\.example\.com[ ;]`)
		csp.NotContains("https://calendar.external.example.com/app/")

		// Flag with a CSP separator in its host: nothing is added
		testutils.WithFlag(t, testInstance, "extservice_url_flag", "https://x.example;sandbox")
		csp = e.GET("/foo/").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Header(echo.HeaderContentSecurityPolicy)
		csp.NotContains("x.example")
		csp.NotContains("sandbox")
	})

	t.Run("NoScriptTagBreakoutIntentData", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		intent := &intent.Intent{
			Action: "PICK",
			Type:   "io.cozy.foos",
			Client: "io.cozy.apps/" + slug,
		}
		err := intent.Save(testInstance)
		require.NoError(t, err)
		err = intent.FillServices(testInstance)
		require.NoError(t, err)
		require.Len(t, intent.Services, 1)
		intent.Action = "PICK</script><script>alert('XSS')</script>"
		err = intent.Save(testInstance)
		require.NoError(t, err)

		u, err := url.Parse(intent.Services[0].Href)
		require.NoError(t, err)

		body := e.GET(u.Path).
			WithHost(slug+"."+testInstance.Domain).
			WithQueryString(u.RawQuery).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Body().Raw()

		// IntentData returns template.HTML (raw, unescaped by html/template).
		// json.Marshal escapes < to \u003c to prevent </script> breakout
		// inside the JSON embedded in a <script> block.
		assert.Contains(t, body, `PICK\u003c/script\u003e`)
	})

	t.Run("FaviconWithContext", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		context := "foo"

		asset, ok := assets.Get("/favicon.ico", context)
		if ok {
			_ = assets.Remove(asset.Name, asset.Context)
		}
		// Create and insert an asset in foo context
		tmpdir := t.TempDir()
		_, err := os.OpenFile(filepath.Join(tmpdir, "custom_favicon.png"), os.O_RDWR|os.O_CREATE, 0600)
		assert.NoError(t, err)

		assetsOptions := []model.AssetOption{{
			URL:     fmt.Sprintf("file://%s", filepath.Join(tmpdir, "custom_favicon.png")),
			Name:    "/favicon.ico",
			Context: context,
		}}
		err = dynamic.RegisterCustomExternals(assetsOptions, 1)
		assert.NoError(t, err)

		// Test the theme
		assert.NoError(t, lifecycle.Patch(testInstance, &lifecycle.Options{
			ContextName: context,
		}))
		assert.NoError(t, err)

		e.GET("/foo").
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(200).
			Body().
			Contains(`this is index.html. <a lang="en" href="https://cozywithapps.example.net/status/">Status</a>`).
			Contains(fmt.Sprintf("/assets/ext/%s/favicon.ico", context)).
			NotContains("/assets/favicon.ico")
	})

	t.Run("SessionCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Create the OAuth client for the flagship app
		flagship := oauth.Client{
			RedirectURIs: []string{"cozy://flagship"},
			ClientName:   "flagship-app",
			SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
			Flagship:     true,
		}
		assert.Nil(t, flagship.Create(testInstance, oauth.NotPending))

		// Create a maximal permission for it
		token, err := testInstance.MakeJWT(consts.AccessTokenAudience,
			flagship.ClientID, "*", "", time.Now())
		assert.NoError(t, err)

		createSessionCode := func() string {
			return e.POST("/auth/session_code").
				WithHost(testInstance.Domain).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201).
				JSON().Object().
				Value("session_code").String().NotEmpty().Raw()
		}

		// Load a non-public page
		code := createSessionCode()
		e.GET("/foo/").
			WithQuery("session_code", code).
			WithHost(slug+"."+testInstance.Domain).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			Body().Contains("this is index.html")

		// Try again and check that the session code cannot be reused
		e.GET("/foo/").
			WithQuery("session_code", code).
			WithHost(slug + "." + testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(302).
			Header("Location").Contains("/auth/login")

		redirectCode := createSessionCode()
		redirect := testutils.CreateTestClient(t, ts.URL).GET("/foo/").
			WithQuery("intent", "intent-session-code-test").
			WithQuery("session_code", redirectCode).
			WithQuery("extra", "drop-me").
			WithHost(slug + "." + testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(http.StatusSeeOther)

		redirect.Cookie("cozysessid").Value().NotEmpty()
		redirect.Header("Location").Contains("intent=intent-session-code-test")
		redirect.Header("Location").NotContains("session_code")
		redirect.Header("Location").NotContains("extra")
	})

	t.Run("ServeAppsWithJWTNotLogged", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		config.GetConfig().Subdomains = config.FlatSubdomains
		appHost := "cozywithapps-mini.example.net"

		rawURL := e.GET("/foo").
			WithQuery("jwt", "abc").
			WithHost(appHost).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(302).
			Header("Location").Raw()

		location, err := url.Parse(rawURL)
		require.NoError(t, err)

		assert.Equal(t, "/auth/login", location.Path)
		assert.Equal(t, testInstance.Domain, location.Host)
		assert.NotEmpty(t, location.Query().Get("redirect"))
		assert.Equal(t, "abc", location.Query().Get("jwt"))
	})

	t.Run("OauthAppCantInstallApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/apps/mini-bis").
			WithHost(testInstance.Domain).
			WithQuery("Source", "git://github.com/nono/cozy-mini.git").
			WithHeader("Authorization", "Bearer "+token).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(403)
	})

	t.Run("OauthAppCantUpdateApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.PUT("/apps/mini").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(403)
	})

	t.Run("ListApps", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/apps/").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithCookie("cozysessid", cozysessID).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Array()
		data.Length().IsEqual(1)

		elem := data.Value(0).Object()
		elem.Value("id").String().NotEmpty()
		elem.HasValue("type", "io.cozy.apps")

		attrs := elem.Value("attributes").Object()
		attrs.HasValue("name", "Mini")
		attrs.HasValue("slug", "mini")

		links := elem.Value("links").Object()
		links.HasValue("self", "/apps/mini")
		links.HasValue("related", "https://cozywithapps-mini.example.net/")
		links.HasValue("icon", "/apps/mini/icon/1.0.0")
	})

	t.Run("IconForApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/apps/mini/icon").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Body().IsEqual("<svg>...</svg>")
	})

	t.Run("DownloadApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		res := e.GET("/apps/mini/download").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Raw()

		mimeType, reader := filetype.FromReader(res.Body)
		require.Equal(t, "application/gzip", mimeType)
		gr, err := gzip.NewReader(reader)
		require.NoError(t, err)
		tr := tar.NewReader(gr)
		indexFound := false
		for {
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			if header.Name == "/index.html" {
				indexFound = true
			}
		}
		require.True(t, indexFound)
	})

	t.Run("DownloadKonnectorVersion", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		res := e.GET("/konnectors/mini/download/1.0.0").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			Raw()

		mimeType, reader := filetype.FromReader(res.Body)
		require.Equal(t, "application/gzip", mimeType)
		gr, err := gzip.NewReader(reader)
		require.NoError(t, err)
		tr := tar.NewReader(gr)
		iconFound := false
		for {
			header, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			if header.Name == "/icon.svg" {
				iconFound = true
			}
		}
		require.True(t, iconFound)
	})

	t.Run("OpenWebapp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Expected flags since they can be modified by other tests
		flags, err := feature.GetFlags(testInstance)
		require.NoError(t, err)
		flagsStr, err := json.Marshal(flags)
		require.NoError(t, err)

		// Create the OAuth client for the flagship app
		flagship := oauth.Client{
			RedirectURIs: []string{"cozy://flagship"},
			ClientName:   "flagship-app",
			SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
			Flagship:     true,
		}
		require.Nil(t, flagship.Create(testInstance, oauth.NotPending))

		// Create a maximal permission for it
		token, err := testInstance.MakeJWT(consts.AccessTokenAudience,
			flagship.ClientID, "*", "", time.Now())
		require.NoError(t, err)

		obj := e.GET("/apps/mini/open").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		data.Value("id").String().NotEmpty()
		data.HasValue("type", "io.cozy.apps.open")

		attrs := data.Value("attributes").Object()
		attrs.HasValue("AppName", "Mini")
		attrs.HasValue("AppSlug", "mini")
		attrs.HasValue("IconPath", "icon.svg")
		attrs.HasValue("Tracking", "false")
		attrs.HasValue("SubDomain", "flat")
		attrs.Value("Cookie").String().Contains("HttpOnly")
		attrs.Value("Token").String().NotEmpty()
		attrs.HasValue("Flags", string(flagsStr))
		attrs.ContainsKey("Warnings")

		links := data.Value("links").Object()
		links.HasValue("self", "/apps/mini/open")
	})

	t.Run("UninstallAppWithLinkedClient", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Install drive app
		installer, err := apps.NewInstaller(testInstance, apps.Copier(consts.WebappType, testInstance),
			&apps.InstallerOptions{
				Operation:  apps.Install,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		require.NoError(t, err)

		_, err = installer.RunSync()
		require.NoError(t, err)

		// Link an OAuthClient to drive
		oauthClient := &oauth.Client{
			ClientName:   "test-linked",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "registry://drive",
		}

		oauthClient.Create(testInstance)
		// Forcing the oauthClient to get a couchID for the purpose of later deletion
		oauthClient, err = oauth.FindClient(testInstance, oauthClient.ClientID)
		require.NoError(t, err)

		scope := "io.cozy.apps:ALL"
		token, err := testInstance.MakeJWT("cli", "drive", scope, "", time.Now())
		require.NoError(t, err)

		// Trying to remove this app
		e.DELETE("/apps/drive").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(400).
			Body().Contains("linked OAuth client exists")

		// Cleaning
		uninstaller, err := apps.NewInstaller(testInstance, apps.Copier(consts.WebappType, testInstance),
			&apps.InstallerOptions{
				Operation:  apps.Delete,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		assert.NoError(t, err)
		_, err = uninstaller.RunSync()
		assert.NoError(t, err)
		errc := oauthClient.Delete(testInstance)
		assert.Nil(t, errc)
	})

	t.Run("UninstallAppWithoutLinkedClient", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Install drive app
		installer, err := apps.NewInstaller(testInstance, apps.Copier(consts.WebappType, testInstance),
			&apps.InstallerOptions{
				Operation:  apps.Install,
				Type:       consts.WebappType,
				Slug:       "drive",
				SourceURL:  "registry://drive",
				Registries: testInstance.Registries(),
			},
		)
		require.NoError(t, err)

		_, err = installer.RunSync()
		require.NoError(t, err)

		// Create an OAuth client not linked to drive
		oauthClient := &oauth.Client{
			ClientName:   "test-linked",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "foobarclient",
		}
		oauthClient.Create(testInstance)
		// Forcing the oauthClient to get a couchID for the purpose of later deletion
		oauthClient, err = oauth.FindClient(testInstance, oauthClient.ClientID)
		require.NoError(t, err)

		scope := "io.cozy.apps:ALL"
		token, err := testInstance.MakeJWT("cli", "drive", scope, "", time.Now())
		assert.NoError(t, err)

		// Trying to remove this app
		e.DELETE("/apps/drive").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200)

		// Cleaning
		errc := oauthClient.Delete(testInstance)
		assert.Nil(t, errc)
	})

	t.Run("SendKonnectorLogsFromFlagshipApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		initialOutput := logrus.New().Out
		defer logrus.SetOutput(initialOutput)

		testOutput := new(bytes.Buffer)
		logrus.SetOutput(testOutput)

		// Create the OAuth client for the flagship app
		flagship := oauth.Client{
			RedirectURIs: []string{"cozy://flagship"},
			ClientName:   "flagship-app",
			SoftwareID:   "github.com/cozy/cozy-stack/testing/flagship",
			Flagship:     true,
		}
		require.Nil(t, flagship.Create(testInstance, oauth.NotPending))

		// Give it the maximal permission
		token, err := testInstance.MakeJWT(consts.AccessTokenAudience, flagship.ClientID, "*", "", time.Now())
		require.NoError(t, err)

		// Send logs for a client side konnector
		e.POST("/konnectors/"+clientSideSlug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(204)

		assert.Equal(t, `time="2022-10-27T17:13:38.382Z" level=error msg="This is an error message" domain=`+domain+" job_id= nspace=jobs slug="+clientSideSlug+" worker_id=client\n", testOutput.String())

		// Send logs for a konnector
		testOutput.Reset()
		e.POST("/konnectors/"+konnectorSlug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(204)

		assert.Equal(t, `time="2022-10-27T17:13:38.382Z" level=error msg="This is an error message" domain=`+domain+" job_id= nspace=jobs slug="+konnectorSlug+"\n", testOutput.String())

		// Send logs for a webapp
		testOutput.Reset()
		e.POST("/apps/"+slug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(204)

		assert.Equal(t, `time="2022-10-27T17:13:38.382Z" level=error msg="This is an error message" domain=`+domain+" job_id= nspace=jobs slug="+slug+"\n", testOutput.String())
	})

	t.Run("SendKonnectorLogsFromKonnector", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		initialOutput := logrus.New().Out
		defer logrus.SetOutput(initialOutput)

		testOutput := new(bytes.Buffer)
		logrus.SetOutput(testOutput)

		token := testInstance.BuildKonnectorToken(slug)

		e.POST("/konnectors/"+slug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(204)

		assert.Equal(t, `time="2022-10-27T17:13:38.382Z" level=error msg="This is an error message" domain=`+domain+" job_id= nspace=jobs slug="+slug+"\n", testOutput.String())

		// Sending logs for a webapp should fail
		e.POST("/apps/"+slug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(403)
	})

	t.Run("SendAppLogsFromWebApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		initialOutput := logrus.New().Out
		defer logrus.SetOutput(initialOutput)

		testOutput := new(bytes.Buffer)
		logrus.SetOutput(testOutput)

		token := testInstance.BuildAppToken(slug, "")

		e.POST("/apps/"+slug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(204)

		assert.Equal(t, `time="2022-10-27T17:13:38.382Z" level=error msg="This is an error message" domain=`+domain+" job_id= nspace=jobs slug="+slug+"\n", testOutput.String())

		// Sending logs for a konnector should fail
		e.POST("/konnectors/"+slug+"/logs").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			WithBytes([]byte(`[ { "timestamp": "2022-10-27T17:13:38.382Z", "level": "error", "msg": "This is an error message" } ]`)).
			Expect().Status(403)
	})
}

func assertAuthGet(e *httpexpect.Expect, slug, domain, path, contentType, charset, content string) {
	e.GET(path).
		WithHost(slug+"."+domain).
		Expect().Status(200).
		HasContentType(contentType, charset).
		Body().Contains(content)
}

func assertAnonGet(e *httpexpect.Expect, slug, domain, path, contentType, charset, content string) {
	e.GET(path).
		WithHost(slug+"."+domain).
		Expect().Status(200).
		HasContentType(contentType, charset).
		Body().Contains(content)
}

func assertNotPublic(e *httpexpect.Expect, slug, domain, path string, code int, location string) {
	e.GET(path).
		WithHost(slug + "." + domain).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(code).
		Header("Location").IsEqual(location)
}

func assertNotFound(e *httpexpect.Expect, slug, domain, path string) {
	e.GET(path).
		WithHost(slug + "." + domain).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(404)
}

func assertInternalServerError(e *httpexpect.Expect, slug, domain, path string) {
	e.GET(path).
		WithHost(slug + "." + domain).
		Expect().Status(500)
}

func assertBodyContainsUnescaped(t *testing.T, body, content string) {
	t.Helper()
	assert.Contains(t, html.UnescapeString(body), content)
}

func assertRedirect(e *httpexpect.Expect, slug, domain, path string, code int, location string) {
	e.GET(path).
		WithHost(slug + "." + domain).
		WithRedirectPolicy(httpexpect.DontFollowRedirects).
		Expect().Status(code).
		Header("Location").IsEqual(location)
}

func TestIntentPermissions(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	cfg := config.GetConfig()
	oldAssets, oldURL := cfg.Assets, cfg.Fs.URL
	t.Cleanup(func() { cfg.Assets, cfg.Fs.URL = oldAssets, oldURL })
	cfg.Assets = "../../assets"
	cfg.Fs.URL = &url.URL{Scheme: "file", Path: t.TempDir()}
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	inst.OnboardingFinished = true
	inst.RegisterToken = nil
	require.NoError(t, instance.Update(inst))
	service, err := setup.InstallMiniApp()
	require.NoError(t, err)
	manifest := &couchdb.JSONDoc{Type: consts.Apps}
	require.NoError(t, couchdb.GetDoc(inst, consts.Apps, consts.Apps+"/"+service, manifest))
	manifest.M["routes"].(map[string]interface{})["/"] = apps.Route{Folder: "/public", Index: "index.html", Public: true}
	manifest.M["intents"] = []apps.Intent{
		{Action: "UPLOAD", Types: []string{consts.Files}, Href: "/public"},
		{Action: "PICK", Types: []string{consts.Files}, Href: "/public"},
	}
	require.NoError(t, couchdb.UpdateDoc(inst, manifest))
	servicePermissions := permission.Set{{Type: consts.Files, Verbs: permission.ALL}}
	require.NoError(t, permission.ForceWebapp(inst, service, servicePermissions))
	mailPermissions := permission.Set{{Type: consts.Files, Verbs: permission.Verbs(permission.GET, permission.POST), Selector: "referenced_by", Values: []string{consts.Apps + "/mail"}}}
	require.NoError(t, permission.ForceWebapp(inst, "mail", mailPermissions))
	mailDir, err := inst.ResolveDirID(consts.Apps + "/mail")
	require.NoError(t, err)
	notesDir, err := inst.ResolveDirID(consts.Apps + "/notes")
	require.NoError(t, err)
	sess, err := session.New(inst, session.NormalRun, "")
	require.NoError(t, err)
	cookie, err := sess.ToCookie()
	require.NoError(t, err)
	ts := setup.GetTestServer("/apps", webApps.WebappsRoutes, func(r *echo.Echo) *echo.Echo {
		proxy, err := web.CreateSubdomainProxy(r, &stack.Services{}, webApps.Serve)
		require.NoError(t, err)
		return proxy
	})
	newClient := func(t *testing.T) *httpexpect.Expect {
		t.Helper()
		return testutils.CreateTestClient(t, ts.URL).Builder(func(r *httpexpect.Request) {
			r.WithHost(inst.Domain).WithCookie(cookie.Name, cookie.Value)
		})
	}
	var downloads atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		_, _ = io.WriteString(w, "attachment")
	}))
	t.Cleanup(source.Close)
	require.NoError(t, safehttp.SetTrustedPrivateNetworks([]string{"127.0.0.1/32"}))
	t.Cleanup(func() { _ = safehttp.SetTrustedPrivateNetworks(config.GetConfig().SafeHTTPTrustedNetworks) })

	createIntent := func(t *testing.T, callerToken, action string) string {
		t.Helper()
		obj := newClient(t).POST("/intents").WithHeader("Authorization", "Bearer "+callerToken).
			WithJSON(map[string]interface{}{"data": map[string]interface{}{
				"type": consts.Intents,
				"attributes": map[string]interface{}{
					"action": action, "type": consts.Files,
					"client": consts.Apps + "/" + service,
				},
			}}).Expect().Status(http.StatusOK).JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).Object()
		return obj.Path("$.data.id").String().Raw()
	}
	serveToken := func(t *testing.T, id string) string {
		t.Helper()
		body := newClient(t).GET("/public").WithHost(service+"."+inst.Domain).
			WithQuery("intent", id).Expect().Status(http.StatusOK).Body().Raw()
		match := regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(body))
		require.Len(t, match, 2)
		return match[1]
	}
	upload := func(t *testing.T, token, folder, name string, status int) {
		t.Helper()
		before := downloads.Load()
		newClient(t).POST("/files/"+folder).WithHeader("Authorization", "Bearer "+token).
			WithQuery("Type", "file").WithQuery("Name", name).
			WithQuery("SourceURL", source.URL).Expect().Status(status)
		if status == http.StatusCreated {
			require.Equal(t, before+1, downloads.Load())
		} else {
			require.Equal(t, before, downloads.Load())
			dir, err := inst.VFS().DirByID(folder)
			require.NoError(t, err)
			_, err = inst.VFS().FileByPath(path.Join(dir.Fullpath, name))
			require.Error(t, err)
		}
	}
	parseToken := func(t *testing.T, token string) permission.Claims {
		t.Helper()
		var claims permission.Claims
		require.NoError(t, crypto.ParseJWT(token, func(*jwt.Token) (interface{}, error) { return inst.PickKey(consts.AppAudience) }, &claims))
		return claims
	}
	mailToken := inst.BuildAppToken("mail", sess.ID())

	t.Run("RejectUnsupportedCallersBeforeParsingBody", func(t *testing.T) {
		require.NoError(t, permission.ForceKonnector(inst, "mail", mailPermissions))
		for _, audience := range []string{consts.KonnectorAudience, consts.CLIAudience} {
			t.Run(audience, func(t *testing.T) {
				callerToken, err := inst.MakeJWT(audience, "mail", consts.Files+":ALL", "", time.Now())
				require.NoError(t, err)
				e := newClient(t)
				e.GET("/files/"+mailDir).WithHeader("Authorization", "Bearer "+callerToken).Expect().Status(http.StatusOK)
				e.POST("/intents").WithHeader("Authorization", "Bearer "+callerToken).
					WithText("invalid JSON").Expect().Status(http.StatusForbidden)
			})
		}
	})

	t.Run("UploadUsesCallerPermissionsAndAttribution", func(t *testing.T) {
		id := createIntent(t, mailToken, "UPLOAD")
		token := serveToken(t, id)
		claims := parseToken(t, token)
		require.Equal(t, "mail", claims.Subject)
		require.Equal(t, service, claims.Actor.Subject)
		require.Equal(t, consts.AppAudience, claims.AudienceString())
		require.Equal(t, id, claims.IntentID)
		require.WithinDuration(t, claims.IssuedAt.Add(consts.IntentTokenValidityDuration), claims.ExpiresAt.Time, time.Second)
		newClient(t).GET("/intents/"+id).WithHeader("Authorization", "Bearer "+token).Expect().Status(http.StatusOK)
		var stored intent.Intent
		require.NoError(t, couchdb.GetDoc(inst, consts.Intents, id, &stored))
		require.Equal(t, consts.Apps+"/mail", stored.Client)

		upload(t, token, mailDir, "allowed.txt", http.StatusCreated)
		mailFolder, err := inst.VFS().DirByID(mailDir)
		require.NoError(t, err)
		created, err := inst.VFS().FileByPath(path.Join(mailFolder.Fullpath, "allowed.txt"))
		require.NoError(t, err)
		require.Equal(t, "mail", created.CozyMetadata.CreatedByApp)
		require.Equal(t, "mail", created.CozyMetadata.UpdatedByApps[0].Slug)
		upload(t, token, consts.RootDirID, "denied-root.txt", http.StatusForbidden)
		upload(t, token, notesDir, "denied-notes.txt", http.StatusForbidden)
	})

	t.Run("RejectNestedIntentsBeforeParsingBody", func(t *testing.T) {
		token := serveToken(t, createIntent(t, mailToken, "UPLOAD"))
		newClient(t).POST("/intents").WithHeader("Authorization", "Bearer "+token).
			WithText("invalid JSON").Expect().Status(http.StatusForbidden)
	})

	t.Run("PickUsesCallerPermissionsAndCannotReadAnotherIntent", func(t *testing.T) {
		token := serveToken(t, createIntent(t, mailToken, "PICK"))
		upload(t, token, consts.RootDirID, "denied-picker.txt", http.StatusForbidden)
		otherID := createIntent(t, mailToken, "PICK")
		newClient(t).GET("/intents/"+otherID).WithHeader("Authorization", "Bearer "+token).Expect().Status(http.StatusForbidden)
	})

	t.Run("PermissionChangesApplyToExistingAndNewTokens", func(t *testing.T) {
		token := serveToken(t, createIntent(t, mailToken, "UPLOAD"))
		t.Cleanup(func() { require.NoError(t, permission.ForceWebapp(inst, "mail", mailPermissions)) })
		require.NoError(t, permission.ForceWebapp(inst, "mail", permission.Set{}))
		upload(t, token, mailDir, "revoked.txt", http.StatusForbidden)
		noWriteToken := serveToken(t, createIntent(t, mailToken, "UPLOAD"))
		upload(t, noWriteToken, mailDir, "no-write.txt", http.StatusForbidden)
	})

	t.Run("LinkedOAuthIntentUsesAppPermissionsAfterClientDeletion", func(t *testing.T) {
		client := &oauth.Client{ClientName: "Mail", SoftwareID: "registry://mail/stable", RedirectURIs: []string{"https://mail.example.test/callback"}}
		require.Nil(t, client.Create(inst, oauth.SoftwareIDPrevalidated))
		oauthToken, err := inst.MakeJWT(consts.AccessTokenAudience, client.ClientID, consts.Files+":GET", "", time.Now())
		require.NoError(t, err)
		id := createIntent(t, oauthToken, "UPLOAD")
		client, err = oauth.FindClient(inst, client.ClientID)
		require.NoError(t, err)
		require.NoError(t, couchdb.DeleteDoc(inst, client))
		token := serveToken(t, id)
		upload(t, token, mailDir, "oauth-app-upload.txt", http.StatusCreated)
		upload(t, token, notesDir, "denied-oauth-app.txt", http.StatusForbidden)
	})

	t.Run("UnlinkedOAuthIntentCannotReceiveServiceToken", func(t *testing.T) {
		client := &oauth.Client{ClientName: "Unlinked", SoftwareID: "github.com/example/unlinked", RedirectURIs: []string{"https://client.example.test/callback"}}
		require.Nil(t, client.Create(inst))
		oauthToken, err := inst.MakeJWT(consts.AccessTokenAudience, client.ClientID, consts.Files+":ALL", "", time.Now())
		require.NoError(t, err)
		id := createIntent(t, oauthToken, "UPLOAD")
		newClient(t).GET("/public").WithHost(service+"."+inst.Domain).
			WithQuery("intent", id).Expect().Status(http.StatusForbidden)
	})

	t.Run("CallerCanHaveBroaderPermissionsThanService", func(t *testing.T) {
		require.NoError(t, permission.ForceWebapp(inst, "broad-caller", servicePermissions))
		t.Cleanup(func() { require.NoError(t, permission.ForceWebapp(inst, service, servicePermissions)) })
		require.NoError(t, permission.ForceWebapp(inst, service, permission.Set{}))
		id := createIntent(t, inst.BuildAppToken("broad-caller", sess.ID()), "UPLOAD")
		token := serveToken(t, id)
		upload(t, token, consts.RootDirID, "broad-caller.txt", http.StatusCreated)
		newClient(t).GET("/files/"+consts.RootDirID).
			WithHeader("Authorization", "Bearer "+inst.BuildAppToken(service, sess.ID())).Expect().Status(http.StatusForbidden)
	})

	t.Run("RejectExpiredAndForgedTokens", func(t *testing.T) {
		id := createIntent(t, mailToken, "UPLOAD")
		claims := parseToken(t, serveToken(t, id))
		claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
		expired, err := crypto.NewJWT(inst.SessionSecret(), claims)
		require.NoError(t, err)
		newClient(t).GET("/intents/"+id).WithHeader("Authorization", "Bearer "+expired).
			Expect().Status(http.StatusForbidden).Header(echo.HeaderWWWAuthenticate).
			Equal(`Bearer error="invalid_token" error_description="The access token expired"`)
		forged, err := crypto.NewJWT([]byte("wrong-key"), claims)
		require.NoError(t, err)
		newClient(t).GET("/intents/"+id).WithHeader("Authorization", "Bearer "+forged).
			Expect().Status(http.StatusForbidden).Header(echo.HeaderWWWAuthenticate).
			Equal(`Bearer error="invalid_token"`)
	})

	t.Run("MissingIntentCannotReceiveToken", func(t *testing.T) {
		newClient(t).GET("/public").WithHost(service+"."+inst.Domain).
			WithQuery("intent", "missing").Expect().Status(http.StatusForbidden)
	})

	t.Run("IssuedTokensSurviveIntentDeletionButRespectAppRevocation", func(t *testing.T) {
		id := createIntent(t, mailToken, "UPLOAD")
		token := serveToken(t, id)
		var stored intent.Intent
		require.NoError(t, couchdb.GetDoc(inst, consts.Intents, id, &stored))
		require.NoError(t, couchdb.DeleteDoc(inst, &stored))
		upload(t, token, mailDir, "after-intent-deletion.txt", http.StatusCreated)
		upload(t, token, consts.RootDirID, "denied-after-intent-deletion.txt", http.StatusForbidden)
		t.Cleanup(func() { require.NoError(t, permission.ForceWebapp(inst, "mail", mailPermissions)) })
		require.NoError(t, permission.ForceWebapp(inst, "mail", permission.Set{}))
		upload(t, token, mailDir, "revoked-after-intent-deletion.txt", http.StatusForbidden)
	})

	t.Run("NormalAppPagesKeepServiceToken", func(t *testing.T) {
		body := newClient(t).GET("/public").WithHost(service + "." + inst.Domain).Expect().Status(http.StatusOK).Body().Raw()
		match := regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(body))
		require.Len(t, match, 2)
		claims := parseToken(t, match[1])
		require.Equal(t, service, claims.Subject)
		require.Nil(t, claims.Actor)
	})

	t.Run("RefreshWithIntentKeepsCallerPermissions", func(t *testing.T) {
		id := createIntent(t, mailToken, "UPLOAD")
		body := newClient(t).GET("/").WithHost(service+"."+inst.Domain).
			WithQuery("refreshToken", "").WithQuery("intent", id).Expect().Status(http.StatusOK).Body().Raw()
		match := regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(body))
		require.Len(t, match, 2)
		claims := parseToken(t, match[1])
		require.Equal(t, "mail", claims.Subject)
		require.Equal(t, service, claims.Actor.Subject)
		require.Equal(t, id, claims.IntentID)
		upload(t, match[1], mailDir, "refreshed.txt", http.StatusCreated)
		upload(t, match[1], consts.RootDirID, "denied-refreshed.txt", http.StatusForbidden)
		t.Cleanup(func() { require.NoError(t, permission.ForceWebapp(inst, "mail", mailPermissions)) })
		require.NoError(t, permission.ForceWebapp(inst, "mail", permission.Set{}))
		body = newClient(t).GET("/").WithHost(service+"."+inst.Domain).
			WithQuery("refreshToken", "").WithQuery("intent", id).Expect().Status(http.StatusOK).Body().Raw()
		match = regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(body))
		require.Len(t, match, 2)
		upload(t, match[1], mailDir, "denied-refreshed-revoked.txt", http.StatusForbidden)
	})

	t.Run("RefreshWithoutIntentReturnsOrdinaryServiceToken", func(t *testing.T) {
		// shortcut: cozy-client drops intent on refresh; preserve it there before relying on delegation across refreshes.
		body := newClient(t).GET("/").WithHost(service+"."+inst.Domain).
			WithQuery("refreshToken", "").Expect().Status(http.StatusOK).Body().Raw()
		match := regexp.MustCompile(`"token":"([^"]+)"`).FindStringSubmatch(html.UnescapeString(body))
		require.Len(t, match, 2)
		claims := parseToken(t, match[1])
		require.Equal(t, service, claims.Subject)
		require.Nil(t, claims.Actor)
	})

	t.Run("SessionIsRequiredAndRevocable", func(t *testing.T) {
		ownSession, err := session.New(inst, session.NormalRun, "")
		require.NoError(t, err)
		id := createIntent(t, mailToken, "UPLOAD")
		var stored intent.Intent
		require.NoError(t, couchdb.GetDoc(inst, consts.Intents, id, &stored))
		token, err := stored.ServiceToken(inst, service, ownSession.ID())
		require.NoError(t, err)
		ownCookie, err := ownSession.ToCookie()
		require.NoError(t, err)
		e := testutils.CreateTestClient(t, ts.URL).Builder(func(r *httpexpect.Request) {
			r.WithHost(inst.Domain).WithHeader("Authorization", "Bearer "+token)
		})
		e.GET("/intents/" + id).Expect().Status(http.StatusForbidden)
		e.GET("/intents/"+id).WithCookie(cookie.Name, cookie.Value).Expect().Status(http.StatusForbidden)
		e.GET("/intents/"+id).WithCookie(ownCookie.Name, ownCookie.Value).Expect().Status(http.StatusOK)
		require.NoError(t, couchdb.DeleteDoc(inst, ownSession))
		e.GET("/intents/"+id).WithCookie(ownCookie.Name, ownCookie.Value).Expect().Status(http.StatusForbidden)
	})
}
