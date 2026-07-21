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
	"net/url"
	"os"
	"path/filepath"
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
	"github.com/cozy/cozy-stack/pkg/filetype"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web"
	webApps "github.com/cozy/cozy-stack/web/apps"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/gavv/httpexpect/v2"
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
		t.Cleanup(func() { _ = couchdb.DeleteDoc(testInstance, clientURLApp) })

		testInstance.FeatureFlags = map[string]interface{}{
			"clienturl_csp_flag": "https://external.example.com",
		}
		require.NoError(t, instance.Update(testInstance))
		t.Cleanup(func() {
			testInstance.FeatureFlags = nil
			_ = instance.Update(testInstance)
		})

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

		// Create the session code
		code := e.POST("/auth/session_code").
			WithHost(testInstance.Domain).
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(201).
			JSON().Object().
			Value("session_code").String().NotEmpty().Raw()

		// Load a non-public page
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
