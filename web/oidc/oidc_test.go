package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/oauth"
	oidcbinding "github.com/cozy/cozy-stack/model/oidc/binding"
	oidcprovider "github.com/cozy/cozy-stack/model/oidc/provider"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/pkg/assets/dynamic"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/gavv/httpexpect/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type delegatedCodeRedis struct {
	values map[string]string
}

func (r *delegatedCodeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	return redis.NewStringResult(r.values[key], nil)
}

func (r *delegatedCodeRedis) Set(_ context.Context, key string, value interface{}, _ time.Duration) *redis.StatusCmd {
	r.values[key] = string(value.([]byte))
	return redis.NewStatusResult("OK", nil)
}

func TestDelegatedCodeIsolation(t *testing.T) {
	config.UseTestFile(t)
	previousStorage := globalStorage
	t.Cleanup(func() { globalStorage = previousStorage })
	inst := &instance.Instance{
		Domain:          "alice.twake.linagora.com",
		ContextName:     "linagora_default",
		OIDCID:          "shared-sub",
		FranceConnectID: "fc-sub",
	}
	stores := map[string]stateStorage{
		"memory": &memStateStorage{codes: make(map[string]*DelegatedCodeData)},
		"redis": &redisStateStorage{
			cl:  &delegatedCodeRedis{values: make(map[string]string)},
			ctx: context.Background(),
		},
	}
	for name, store := range stores {
		t.Run(name, func(t *testing.T) {
			globalStorage = store
			for _, tc := range []struct {
				name     string
				sub      string
				context  string
				provider oidcprovider.Kind
				error    string
			}{
				{"oidc", inst.OIDCID, inst.ContextName, oidcprovider.GenericProvider, ""},
				{"domain", inst.Domain, inst.ContextName, oidcprovider.GenericProvider, ""},
				{"franceconnect", inst.FranceConnectID, inst.ContextName, oidcprovider.FranceConnectProvider, ""},
				{"same subject in another context", inst.OIDCID, "cozy_twake_default", oidcprovider.GenericProvider, ErrContextMismatch.Error()},
				{"same domain in another context", inst.Domain, "cozy_twake_default", oidcprovider.GenericProvider, ErrContextMismatch.Error()},
				{"unbound legacy code", inst.OIDCID, "", oidcprovider.GenericProvider, "invalid code"},
				{"different subject", "other-sub", inst.ContextName, oidcprovider.GenericProvider, "invalid code"},
				{"missing subject", "", inst.ContextName, oidcprovider.GenericProvider, "invalid code"},
				{"oidc code for franceconnect subject", inst.FranceConnectID, inst.ContextName, oidcprovider.GenericProvider, "invalid code"},
				{"franceconnect code for oidc subject", inst.OIDCID, inst.ContextName, oidcprovider.FranceConnectProvider, "invalid code"},
				{"franceconnect code for domain", inst.Domain, inst.ContextName, oidcprovider.FranceConnectProvider, "invalid code"},
				{"unknown provider", inst.OIDCID, inst.ContextName, oidcprovider.Kind(99), "invalid code"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					code := store.CreateCodeData(tc.sub, "session-id", tc.context, tc.provider)
					data, err := validateDelegatedCode(inst, code)
					if tc.error != "" {
						require.EqualError(t, err, tc.error)
						require.Nil(t, data)
						return
					}
					require.NoError(t, err)
					require.Equal(t, &DelegatedCodeData{
						Sub: tc.sub, SessionID: "session-id", ContextName: tc.context, Provider: tc.provider,
					}, data)
				})
			}
			_, err := validateDelegatedCode(inst, "unknown-code")
			require.EqualError(t, err, "invalid code")
		})
	}
	legacyRedis := &delegatedCodeRedis{values: map[string]string{
		"raw-code":  inst.OIDCID,
		"json-code": `{"sub":"shared-sub","session_id":"legacy-session"}`,
	}}
	globalStorage = &redisStateStorage{cl: legacyRedis, ctx: context.Background()}
	for _, code := range []string{"raw-code", "json-code"} {
		_, err := validateDelegatedCode(inst, code)
		require.EqualError(t, err, "invalid code")
	}
}

func TestDelegatedCodeContextMismatch(t *testing.T) {
	config.UseTestFile(t)
	previousStorage := globalStorage
	t.Cleanup(func() { globalStorage = previousStorage })
	globalStorage = &memStateStorage{codes: make(map[string]*DelegatedCodeData)}
	userinfo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer external-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{"sub":"shared-sub"}`))
		assert.NoError(t, err)
	}))
	t.Cleanup(userinfo.Close)
	config.GetConfig().Authentication = map[string]interface{}{
		"linagora_default": map[string]interface{}{
			"oidc": map[string]interface{}{
				"userinfo_url": userinfo.URL, "allow_custom_instance": true,
			},
		},
	}
	e := echo.New()
	mintRequest := httptest.NewRequest(http.MethodPost, "/admin-oidc/linagora_default/generic/code",
		strings.NewReader(`{"access_token":"external-token"}`))
	mintRequest.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	mintResponse := httptest.NewRecorder()
	c := e.NewContext(mintRequest, mintResponse)
	c.SetParamNames("context", "provider")
	c.SetParamValues("linagora_default", "generic")
	require.NoError(t, GetDelegatedCode(c))
	require.Equal(t, http.StatusOK, mintResponse.Code)
	var minted struct {
		Code string `json:"delegated_code"`
	}
	require.NoError(t, json.Unmarshal(mintResponse.Body.Bytes(), &minted))
	require.NotEmpty(t, minted.Code)
	_, err := validateDelegatedCode(&instance.Instance{
		Domain: "alice.twake.linagora.com", ContextName: "linagora_default", OIDCID: "shared-sub",
	}, minted.Code)
	require.NoError(t, err)

	for _, tc := range []struct {
		path        string
		contentType string
		body        string
		handler     echo.HandlerFunc
	}{
		{"/oidc/access_token", echo.MIMEApplicationJSON, `{"code":"` + minted.Code + `","scope":"*"}`, AccessToken},
		{"/oidc/bitwarden/cozy_twake_default", echo.MIMEApplicationForm, url.Values{"code": {minted.Code}}.Encode(), BitwardenExchange},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set(echo.HeaderContentType, tc.contentType)
			res := httptest.NewRecorder()
			c := e.NewContext(req, res)
			middlewares.SetInstance(c, &instance.Instance{
				Domain: "alice.twake.app", ContextName: "cozy_twake_default", OIDCID: "shared-sub",
			})
			require.NoError(t, tc.handler(c))
			require.Equal(t, http.StatusBadRequest, res.Code)
			require.JSONEq(t, `{"error":"OIDC context mismatch"}`, res.Body.String())
			require.Empty(t, res.Header().Values(echo.HeaderSetCookie))
		})
	}
}

func TestOidc(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	var redirectURL *url.URL

	config.UseTestFile(t)
	config.GetConfig().Assets = "../../assets"
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	// Declaring a dummy worker for the 2FA (sendmail)
	wl := &job.WorkerConfig{
		WorkerType:  "sendmail",
		Concurrency: 4,
		WorkerFunc: func(ctx *job.TaskContext) error {
			return nil
		},
	}
	job.AddWorker(wl)

	testInstance := setup.GetTestInstance(&lifecycle.Options{ContextName: "foocontext"})

	// Mocking API endpoint to validate token
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/oidc":       Routes,
		"/admin-oidc": AdminRoutes,
		"/token": func(g *echo.Group) {
			g.POST("/getToken", func(c echo.Context) error {
				return c.JSON(http.StatusOK, echo.Map{
					"access_token": "foobar",
					"id_token":     makeUnsignedJWT(map[string]interface{}{"sid": "login-session-456"}),
				})
			})
			g.GET("/:domain", func(c echo.Context) error {
				return c.JSON(http.StatusOK, echo.Map{"domain": c.Param("domain")})
			})
		},
		"/api": func(g *echo.Group) {
			g.GET("/v1/userinfo", func(c echo.Context) error {
				auth := c.Request().Header.Get(echo.HeaderAuthorization)
				if auth != "Bearer fc_token" {
					return c.NoContent(http.StatusBadRequest)
				}
				return c.JSON(http.StatusOK, echo.Map{
					"sub":   "fc_sub",
					"email": "jerome@example.org",
				})
			})
		},
	})

	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	// Creating a custom context with oidc and franceconnect authentication
	tokenURL := ts.URL + "/token/getToken"
	userInfoURL := ts.URL + "/token/" + testInstance.Domain
	authentication := map[string]interface{}{
		"oidc": map[string]interface{}{
			"redirect_uri":            "http://foobar.com/redirect",
			"client_id":               "foo",
			"client_secret":           "bar",
			"scope":                   "foo",
			"authorize_url":           "http://foobar.com/authorize",
			"token_url":               tokenURL,
			"userinfo_url":            userInfoURL,
			"userinfo_instance_field": "domain",
		},
		"franceconnect": map[string]interface{}{
			"redirect_uri":  "http://foobar.com/redirect",
			"client_id":     "fc_client_id",
			"client_secret": "fc_client_secret",
			"scope":         "openid profile",
			"authorize_url": "https://franceconnect.gouv.fr/api/v1/authorize",
			"token_url":     "https://franceconnect.gouv.fr/api/v1/token",
			"userinfo_url":  ts.URL + "/api/v1/userinfo",
		},
	}
	conf := config.GetConfig()
	conf.Authentication = map[string]interface{}{
		"foocontext": authentication,
	}

	require.NoError(t, dynamic.InitDynamicAssetFS(config.FsURL().String()), "Could not init dynamic FS")

	t.Run("StartWithOnboardingNotFinished", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Should get a 200 with body "activate your cozy"
		e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			Expect().Status(200).
			ContentType("text/html").
			Body().Contains("Onboarding Not activated")
	})

	t.Run("StartWithOnboardingFinished", func(t *testing.T) {
		var err error

		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{OnboardingFinished: &onboardingFinished})

		// Should return a 303 redirect
		u := e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err = url.Parse(u)
		require.NoError(t, err)

		assert.Equal(t, "foobar.com", redirectURL.Host)
		assert.Equal(t, "/authorize", redirectURL.Path)
		assert.NotNil(t, redirectURL.Query().Get("client_id"))
		assert.NotNil(t, redirectURL.Query().Get("nonce"))
		assert.NotNil(t, redirectURL.Query().Get("redirect_uri"))
		assert.NotNil(t, redirectURL.Query().Get("response_type"))
		assert.NotNil(t, redirectURL.Query().Get("state"))
		assert.NotNil(t, redirectURL.Query().Get("scope"))
		// Verify login_hint logic: OrgDomain+email > old_domain > OIDCID > nothing
		loginHint := redirectURL.Query().Get("login_hint")
		expectedLoginHint := ""
		if testInstance.OrgDomain != "" {
			if email, err := testInstance.SettingsEMail(); err == nil && email != "" {
				expectedLoginHint = email
			}
		}
		if expectedLoginHint == "" && testInstance.OldDomain != "" {
			expectedLoginHint = testInstance.OldDomain
		} else if expectedLoginHint == "" && testInstance.OIDCID != "" {
			expectedLoginHint = testInstance.OIDCID
		}
		if expectedLoginHint != "" {
			assert.NotEmpty(t, loginHint, "login_hint should be present in redirect URL")
			assert.Equal(t, expectedLoginHint, loginHint, "login_hint should match email, old_domain or OIDCID")
		} else {
			assert.Empty(t, loginHint, "login_hint should not be present when no hint source is available")
		}
	})

	// Get the login page, assert we have an error if state is missing
	t.Run("Success", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/oidc/login").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			WithQueryString(redirectURL.RawQuery). // Reuse the query for the request above.
			Expect().Status(303).
			Header("Location").Equal(testInstance.DefaultRedirection().String())
	})

	t.Run("WithoutState", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		queryWithoutState := redirectURL.Query()
		queryWithoutState.Del("state")

		e.GET("/oidc/login").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			WithQueryString(queryWithoutState.Encode()).
			Expect().Status(404)
	})

	t.Run("LoginWithCustomInstanceSubMismatch", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
			OIDCID:             "expected-sub",
		})

		originalAuth := conf.Authentication["foocontext"]
		conf.Authentication["foocontext"] = map[string]interface{}{
			"oidc": map[string]interface{}{
				"redirect_uri":          "http://foobar.com/redirect",
				"client_id":             "foo",
				"client_secret":         "bar",
				"scope":                 "openid profile",
				"authorize_url":         "http://foobar.com/authorize",
				"userinfo_url":          ts.URL + "/api/v1/userinfo",
				"allow_oauth_token":     true,
				"allow_custom_instance": true,
			},
		}
		t.Cleanup(func() {
			conf.Authentication["foocontext"] = originalAuth
		})

		body := e.GET("/oidc/login").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			WithQuery("access_token", "fc_token").
			Expect().Status(400).
			ContentType("text/html").
			Body()

		body.Contains(testInstance.Domain)
		body.Contains("fc_sub")
		body.NotContains("Error from the identity provider.")
	})

	t.Run("LoginWith2FA", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{OnboardingFinished: &onboardingFinished, AuthMode: "two_factor_mail"})

		u := e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		// Get the login page, assert we have the 2FA activated
		queryWithToken := redirectURL.Query()
		queryWithToken.Add("token", "foo")

		body := e.GET("/oidc/login").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			WithQueryString(queryWithToken.Encode()).
			Expect().Status(200).
			ContentType("text/html").
			Body()

		body.Contains(`<form id="oidc-twofactor-form"`)
		matches := body.Match(`name="access-token" value="(\w+)"`)
		matches.Length().Equal(2)
		accessToken := matches.Index(1).NotEmpty().Raw()

		// Check that the user is redirected to the 2FA page
		u = e.POST("/oidc/twofactor").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			WithFormField("access-token", accessToken).
			WithFormField("trusted-device-token", "").
			WithFormField("redirect", "").
			WithFormField("confirm", "").
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err = url.Parse(u)
		require.NoError(t, err)

		assert.Equal(t, "/auth/twofactor", redirectURL.Path)
		assert.NotNil(t, redirectURL.Query().Get("two_factor_token"))
	})

	t.Run("DelegatedCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		sub := "fc_sub"
		email := "jerome@example.org"

		onboardingFinished := true
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
			FranceConnectID:    sub,
			AuthMode:           "basic",
		})

		obj := e.POST("/admin-oidc/"+testInstance.ContextName+"/franceconnect/code").
			WithHeader("Content-Type", "application/json").
			WithJSON(map[string]string{
				"access_token": "fc_token",
				"id_token":     makeUnsignedJWT(map[string]interface{}{"sid": "cloudery-session-789"}),
			}).
			Expect().Status(200).
			JSON().
			Object()
		obj.Value("sub").String().Equal(sub)
		obj.Value("email").String().Equal(email)
		code := obj.Value("delegated_code").String().NotEmpty().Raw()

		oauthClient := &oauth.Client{
			RedirectURIs:    []string{"cozy://flagship"},
			ClientName:      "Cozy Flagship",
			ClientKind:      "mobile",
			SoftwareID:      "cozy-flagship",
			SoftwareVersion: "0.1.0",
		}
		require.Nil(t, oauthClient.Create(testInstance))
		client, err := oauth.FindClient(testInstance, oauthClient.ClientID)
		require.NoError(t, err)
		client.CertifiedFromStore = true
		require.NoError(t, client.SetFlagship(testInstance))

		obj2 := e.POST("/oidc/access_token").
			WithHost(testInstance.Domain).
			WithJSON(map[string]string{
				"client_id":     oauthClient.ClientID,
				"client_secret": oauthClient.ClientSecret,
				"scope":         "*",
				"code":          code,
			}).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
			Object()
		obj2.Value("token_type").String().Equal("bearer")
		obj2.Value("scope").String().Equal("*")
		obj2.Value("access_token").String().NotEmpty()
		obj2.Value("refresh_token").String().NotEmpty()

		// Verify the session ID was retrieved from the delegated code (not from id_token in AccessToken)
		storedClient, err := oauth.FindClient(testInstance, oauthClient.ClientID)
		require.NoError(t, err)
		require.Equal(t, "cloudery-session-789", storedClient.OIDCSessionID)

		boundClients, err := oidcbinding.ListOAuthClients("", "cloudery-session-789")
		require.NoError(t, err)
		require.Len(t, boundClients, 1)
		require.Equal(t, testInstance.ContextName, boundClients[0].OIDCProviderKey)
		require.Equal(t, testInstance.Domain, boundClients[0].Domain)
		require.Equal(t, oauthClient.ClientID, boundClients[0].OAuthClientID)
	})

	t.Run("LoginHintWithOIDCID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		oidcID := "user-oidc-sub-123"
		// Set OIDCID without old_domain to test OIDCID priority
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
			OIDCID:             oidcID,
		})

		// Test that login_hint uses OIDCID when old_domain is not present
		u := e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		loginHint := redirectURL.Query().Get("login_hint")
		// If old_domain exists from previous test, it should take priority
		// Otherwise, OIDCID should be used
		if testInstance.OldDomain != "" {
			assert.Equal(t, testInstance.OldDomain, loginHint, "login_hint should use old_domain when present (priority)")
		} else {
			assert.NotEmpty(t, loginHint, "login_hint should be present when OIDCID is set")
			assert.Equal(t, oidcID, loginHint, "login_hint should use OIDCID when old_domain is not present")
		}
	})

	t.Run("LoginHintWithOrgDomain", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		orgDomain := "company.com"
		email := "user@company.com"

		// Clear old_domain and OIDCID, set OrgDomain and email
		testInstance.OrgDomain = orgDomain
		testInstance.OldDomain = ""
		testInstance.OIDCID = ""
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
			Email:              email,
		})

		// Test that login_hint uses email when OrgDomain is present (highest priority)
		u := e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		loginHint := redirectURL.Query().Get("login_hint")
		assert.NotEmpty(t, loginHint, "login_hint should be present when OrgDomain is set with email")
		assert.Equal(t, email, loginHint, "login_hint should use email when OrgDomain is present (highest priority)")

		// Clean up
		testInstance.OrgDomain = ""
	})

	t.Run("LoginHintWithOldDomain", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		oldDomain := "old.example.com"
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
			OldDomain:          oldDomain,
		})

		// Test that login_hint uses old_domain when present (highest priority)
		u := e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		loginHint := redirectURL.Query().Get("login_hint")
		assert.NotEmpty(t, loginHint, "login_hint should be present when old_domain is set")
		assert.Equal(t, oldDomain, loginHint, "login_hint should use old_domain when present (highest priority)")
	})

	t.Run("LoginHintWithFranceConnect", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		oldDomain := "old.franceconnect.example.com"
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
			OldDomain:          oldDomain,
		})

		// Test that login_hint is present for FranceConnect flow
		u := e.GET("/oidc/franceconnect").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		loginHint := redirectURL.Query().Get("login_hint")
		assert.NotEmpty(t, loginHint, "login_hint should be present for FranceConnect flow")
		assert.Equal(t, oldDomain, loginHint, "login_hint should use old_domain for FranceConnect")
	})

	t.Run("LoginHintWithoutOldDomainOrOIDCID", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		onboardingFinished := true
		_ = lifecycle.Patch(testInstance, &lifecycle.Options{
			OnboardingFinished: &onboardingFinished,
		})

		// Test that login_hint behavior depends on current instance state
		// If old_domain or OIDCID exist from previous tests, they will be used
		// Otherwise, no login_hint should be present
		u := e.GET("/oidc/start").
			WithHost(testInstance.Domain).
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		loginHint := redirectURL.Query().Get("login_hint")
		// Check current state - if neither old_domain nor OIDCID is set, login_hint should be empty
		if testInstance.OldDomain == "" && testInstance.OIDCID == "" {
			assert.Empty(t, loginHint, "login_hint should not be present when neither old_domain nor OIDCID is set")
		} else {
			// If one of them exists, it should be used (this tests the priority logic)
			assert.NotEmpty(t, loginHint, "login_hint should be present if old_domain or OIDCID is set")
		}
	})

	t.Run("LoginHintWithoutDomain", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		// Test Bitwarden flow where domain is empty - login_hint should not be present
		// Note: This tests the BitwardenStart endpoint which passes empty domain
		u := e.GET("/oidc/bitwarden/foocontext").
			WithQuery("redirect_uri", "cozypass://login").
			WithRedirectPolicy(httpexpect.DontFollowRedirects).
			Expect().Status(303).
			Header("Location").Raw()

		redirectURL, err := url.Parse(u)
		require.NoError(t, err)

		loginHint := redirectURL.Query().Get("login_hint")
		assert.Empty(t, loginHint, "login_hint should not be present when domain is empty (Bitwarden case)")
	})
}

func TestFlagshipOIDCLoginLogoutIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	config.GetConfig().Assets = "../../assets"
	testutils.NeedCouchdb(t)

	// Reset state storage to avoid leaking codes between tests
	globalStorageMutex.Lock()
	globalStorage = nil
	globalStorageMutex.Unlock()

	setup := testutils.NewSetup(t, t.Name())

	// Prepare mock OIDC provider
	sub := "flagship-user-sub"
	email := "flagship@example.org"
	sessionID := "flagship-session-123"

	logoutCh := make(chan string, 1)

	var oidcServer *httptest.Server
	oidcServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			resp := map[string]string{
				"issuer":                 oidcServer.URL,
				"authorization_endpoint": oidcServer.URL + "/authorize",
				"token_endpoint":         oidcServer.URL + "/token",
				"userinfo_endpoint":      oidcServer.URL + "/userinfo",
				"end_session_endpoint":   oidcServer.URL + "/logout",
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/userinfo":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"sub":   sub,
				"email": email,
			})
		case "/logout":
			logoutCh <- r.URL.Query().Get("session_id")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(oidcServer.Close)

	cfg := config.GetConfig()
	cfg.Authentication = map[string]interface{}{
		"flagship-ctx": map[string]interface{}{
			"oidc": map[string]interface{}{
				"client_id":             "provider-client-id",
				"client_secret":         "provider-secret",
				"scope":                 "openid profile email",
				"redirect_uri":          "https://example.org/callback",
				"authorize_url":         oidcServer.URL + "/authorize",
				"token_url":             oidcServer.URL + "/token",
				"userinfo_url":          oidcServer.URL + "/userinfo",
				"allow_custom_instance": true,
			},
		},
	}

	cache := cfg.CacheStorage
	cache.Clear("oidc-config:" + oidcServer.URL + "/.well-known/openid-configuration")

	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/oidc":       Routes,
		"/admin-oidc": AdminRoutes,
		"/auth":       auth.Routes,
	})
	ts.Config.Handler.(*echo.Echo).Renderer = render
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	onboardingFinished := true
	testInstance := setup.GetTestInstance(&lifecycle.Options{
		ContextName:        "flagship-ctx",
		OnboardingFinished: &onboardingFinished,
		OIDCID:             sub,
	})

	flagshipClient := &oauth.Client{
		RedirectURIs:    []string{"cozy://flagship"},
		ClientName:      "Cozy Flagship",
		ClientKind:      "mobile",
		SoftwareID:      "cozy-flagship",
		SoftwareVersion: "1.0.0",
	}
	require.Nil(t, flagshipClient.Create(testInstance))

	clientID := flagshipClient.ClientID
	clientSecret := flagshipClient.ClientSecret
	registrationToken := flagshipClient.RegistrationToken

	client, err := oauth.FindClient(testInstance, clientID)
	require.NoError(t, err)
	client.CertifiedFromStore = true
	require.NoError(t, client.SetFlagship(testInstance))

	client, err = oauth.FindClient(testInstance, clientID)
	require.NoError(t, err)
	require.True(t, client.Flagship)

	e := testutils.CreateTestClient(t, ts.URL)

	// Call GetDelegatedCode endpoint (simulating cloudery)
	delegatedCodeResp := e.POST("/admin-oidc/flagship-ctx/generic/code").
		WithHeader("Content-Type", "application/json").
		WithJSON(map[string]string{
			"access_token": "cloudery-access-token",
			"id_token":     makeUnsignedJWT(map[string]interface{}{"sid": sessionID}),
		}).
		Expect().Status(http.StatusOK).
		JSON().
		Object()
	delegatedCodeResp.Value("sub").String().IsEqual(sub)
	delegatedCodeResp.Value("email").String().IsEqual(email)
	delegatedCode := delegatedCodeResp.Value("delegated_code").String().NotEmpty().Raw()

	// Call AccessToken with only the delegated code (no id_token)
	accessTokenResp := e.POST("/oidc/access_token").
		WithHost(testInstance.Domain).
		WithJSON(map[string]string{
			"client_id":     clientID,
			"client_secret": clientSecret,
			"scope":         "*",
			"code":          delegatedCode,
		}).
		Expect().Status(http.StatusOK).
		JSON().
		Object()
	accessTokenResp.Value("token_type").String().Equal("bearer")
	accessTokenResp.Value("scope").String().Equal("*")
	accessTokenResp.Value("access_token").String().NotEmpty()
	accessTokenResp.Value("refresh_token").String().NotEmpty()

	// Verify the session ID was stored in the OAuth client
	client, err = oauth.FindClient(testInstance, clientID)
	require.NoError(t, err)
	require.Equal(t, sessionID, client.OIDCSessionID)

	e.DELETE("/auth/register/"+clientID).
		WithHost(testInstance.Domain).
		WithHeader("Authorization", "Bearer "+registrationToken).
		Expect().
		Status(http.StatusNoContent)

	select {
	case sid := <-logoutCh:
		require.Equal(t, sessionID, sid)
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for OIDC end_session call")
	}

	boundClients, err := oidcbinding.ListOAuthClients("", sessionID)
	require.NoError(t, err)
	require.Empty(t, boundClients)
}

func TestOIDCLogout(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	setup := testutils.NewSetup(t, t.Name())
	_ = setup.GetTestInstance() // Not used in current tests, but needed for setup

	t.Run("PerformOIDCLogoutWithValidConfiguration", func(t *testing.T) {
		// Mock HTTP server for OIDC provider
		var mockServer *httptest.Server
		mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/.well-known/openid-configuration":
				config := oidcprovider.Metadata{
					Issuer:             mockServer.URL,
					EndSessionEndpoint: mockServer.URL + "/logout",
				}
				json.NewEncoder(w).Encode(config)
			case "/logout":
				// Verify session_id parameter is present
				sessionID := r.URL.Query().Get("session_id")
				assert.Equal(t, "test-session-123", sessionID)
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer mockServer.Close()

		// Configure test context with mock server
		testConfig := map[string]interface{}{
			"token_url": mockServer.URL + "/token",
		}
		config.GetConfig().Authentication = map[string]interface{}{
			"test-context": map[string]interface{}{
				"oidc": testConfig,
			},
		}

		err := oauth.PerformOIDCLogout("test-context", "test-session-123")
		assert.NoError(t, err)
	})

	t.Run("CachingBehavior", func(t *testing.T) {
		requestCount := 0
		var mockServer *httptest.Server
		mockServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/.well-known/openid-configuration" {
				requestCount++
				config := oidcprovider.Metadata{
					Issuer:             mockServer.URL,
					EndSessionEndpoint: mockServer.URL + "/logout",
				}
				json.NewEncoder(w).Encode(config)
			}
		}))
		defer mockServer.Close()

		// Configure test context
		testConfig := map[string]interface{}{
			"token_url": mockServer.URL + "/token",
		}
		config.GetConfig().Authentication = map[string]interface{}{
			"cache-test": map[string]interface{}{
				"oidc": testConfig,
			},
		}

		// First call should fetch from server
		_ = oauth.PerformOIDCLogout("cache-test", "session-1")
		assert.Equal(t, 1, requestCount)

		// Second call should use cache
		_ = oauth.PerformOIDCLogout("cache-test", "session-2")
		assert.Equal(t, 1, requestCount, "Configuration should be cached")
	})

	t.Run("BackchannelLogoutUsesBoundSIDWithinMatchingContext", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)
		privateKey, kid, jwksURL := newBackchannelSigningKey(t)

		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: "logout-bound-context",
		})

		config.GetConfig().Authentication = map[string]interface{}{
			"logout-bound-context": map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/logout-bound-context",
					"provider-client-id",
					jwksURL,
				),
			},
		}

		target, err := session.New(firstInst, session.NormalRun, "bound-sid")
		require.NoError(t, err)
		other, err := session.New(firstInst, session.NormalRun, "other-sid")
		require.NoError(t, err)

		logoutToken := makeSignedJWT(t, privateKey, kid, makeBackchannelLogoutClaims(
			"https://issuer.example/logout-bound-context",
			"provider-client-id",
			"bound-sid",
			map[string]interface{}{"domain": firstInst.Domain},
		))

		env.client.POST("/oidc/logout-bound-context/logout").
			WithHost(firstInst.Domain).
			WithFormField("logout_token", logoutToken).
			Expect().
			Status(http.StatusOK)

		requireSessionDeleted(t, firstInst, target.ID())
		requireSessionExists(t, firstInst, other.ID())
	})

	t.Run("ContextfulBackchannelLogoutFailsWhenStrictIssuerCannotBeResolved", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)
		privateKey, kid, jwksURL := newBackchannelSigningKey(t)

		contextName := "strict-issuer-resolution-failure-context"
		inst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: contextName,
		})

		oidcConfig := makeTestOIDCConfig(
			"",
			"strict-issuer-client-id",
			jwksURL,
		)
		oidcConfig["token_url"] = "://bad-token-url"
		config.GetConfig().Authentication = map[string]interface{}{
			contextName: map[string]interface{}{"oidc": oidcConfig},
		}

		logoutToken := makeSignedJWT(t, privateKey, kid, makeBackchannelLogoutClaims(
			"https://issuer.example/strict-issuer-resolution-failure-context",
			"strict-issuer-client-id",
			"strict-issuer-resolution-sid",
			map[string]interface{}{"sub": inst.OIDCID},
		))

		env.client.POST("/oidc/"+contextName+"/logout").
			WithHost(inst.Domain).
			WithFormField("logout_token", logoutToken).
			Expect().
			Status(http.StatusBadRequest).
			JSON().
			Object().
			Value("error").String().Equal("invalid logout token")
	})

	t.Run("ContextlessBackchannelLogoutUsesSIDAndIssuerAudience", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)
		privateKey, kid, jwksURL := newBackchannelSigningKey(t)

		targetContext := "z-contextless-logout-bound-context"
		otherContext := "a-other-contextless-logout-bound-context"

		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: targetContext,
		})
		secondDomain := "second-" + firstInst.Domain
		_ = lifecycle.Destroy(secondDomain)
		secondInst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      secondDomain,
			ContextName: otherContext,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(secondDomain) })

		config.GetConfig().Authentication = map[string]interface{}{
			targetContext: map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/contextless-logout-bound-context",
					"provider-client-id",
					jwksURL,
				),
			},
			otherContext: map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/other-contextless-logout-bound-context",
					"other-provider-client-id",
					jwksURL,
				),
			},
		}

		target, err := session.New(firstInst, session.NormalRun, "bound-sid")
		require.NoError(t, err)
		sameSIDOtherContext, err := session.New(secondInst, session.NormalRun, "bound-sid")
		require.NoError(t, err)
		other, err := session.New(firstInst, session.NormalRun, "other-sid")
		require.NoError(t, err)

		logoutToken := makeSignedJWT(t, privateKey, kid, makeBackchannelLogoutClaims(
			"https://issuer.example/contextless-logout-bound-context",
			"provider-client-id",
			"bound-sid",
			map[string]interface{}{"domain": firstInst.Domain},
		))

		env.client.POST("/oidc/logout").
			WithHost(firstInst.Domain).
			WithFormField("logout_token", logoutToken).
			Expect().
			Status(http.StatusOK)

		requireSessionDeleted(t, firstInst, target.ID())
		requireSessionExists(t, secondInst, sameSIDOtherContext.ID())
		requireSessionExists(t, firstInst, other.ID())
	})

	t.Run("ContextlessBackchannelLogoutDeletesBoundOAuthClients", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)
		privateKey, kid, jwksURL := newBackchannelSigningKey(t)

		targetContext := "z-oauth-client-logout-context"
		otherContext := "a-oauth-client-logout-context"
		targetSID := "oauth-client-bound-sid"

		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: targetContext,
			OIDCID:      "oauth-client-target-sub",
		})
		secondDomain := "other-oauth-" + firstInst.Domain
		_ = lifecycle.Destroy(secondDomain)
		secondInst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      secondDomain,
			ContextName: otherContext,
			OIDCID:      "oauth-client-other-sub",
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(secondDomain) })

		targetOIDCConfig := makeTestOIDCConfig(
			"https://issuer.example/oauth-client-logout-context",
			"oauth-client-logout-client-id",
			jwksURL,
		)
		targetOIDCConfig["allow_oauth_token"] = true
		otherOIDCConfig := makeTestOIDCConfig(
			"https://issuer.example/other-oauth-client-logout-context",
			"other-oauth-client-logout-client-id",
			jwksURL,
		)
		otherOIDCConfig["allow_oauth_token"] = true
		config.GetConfig().Authentication = map[string]interface{}{
			targetContext: map[string]interface{}{"oidc": targetOIDCConfig},
			otherContext:  map[string]interface{}{"oidc": otherOIDCConfig},
		}

		targetClient := createFlagshipOAuthClient(t, firstInst)
		otherClient := createFlagshipOAuthClient(t, secondInst)

		issueOIDCAccessTokenWithDelegatedCode(t, env.client, firstInst, targetClient, getStorage().CreateCodeData(firstInst.OIDCID, targetSID, firstInst.ContextName, oidcprovider.GenericProvider))
		issueOIDCAccessTokenWithDelegatedCode(t, env.client, secondInst, otherClient, getStorage().CreateCodeData(secondInst.OIDCID, targetSID, secondInst.ContextName, oidcprovider.GenericProvider))

		boundClients, err := oidcbinding.ListOAuthClients("", targetSID)
		require.NoError(t, err)
		require.Len(t, boundClients, 2)

		logoutToken := makeSignedJWT(t, privateKey, kid, makeBackchannelLogoutClaims(
			"https://issuer.example/oauth-client-logout-context",
			"oauth-client-logout-client-id",
			targetSID,
			map[string]interface{}{"sub": firstInst.OIDCID},
		))

		env.client.POST("/oidc/logout").
			WithHost(firstInst.Domain).
			WithFormField("logout_token", logoutToken).
			Expect().
			Status(http.StatusOK)

		requireOAuthClientDeleted(t, firstInst, targetClient.ClientID)
		requireOAuthClientExists(t, secondInst, otherClient.ClientID)

		boundClients, err = oidcbinding.ListOAuthClients("", targetSID)
		require.NoError(t, err)
		require.Len(t, boundClients, 1)
		require.Equal(t, otherContext, boundClients[0].OIDCProviderKey)
		require.Equal(t, otherClient.ClientID, boundClients[0].OAuthClientID)
	})

	t.Run("ContextlessBackchannelLogoutWithoutSIDFallsBackToDomainClaim", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)
		privateKey, kid, jwksURL := newBackchannelSigningKey(t)

		contextName := "domain-fallback-context"
		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: contextName,
		})
		secondDomain := "other-" + firstInst.Domain
		_ = lifecycle.Destroy(secondDomain)
		secondInst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      secondDomain,
			ContextName: contextName,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(secondDomain) })

		config.GetConfig().Authentication = map[string]interface{}{
			contextName: map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/domain-fallback-context",
					"domain-fallback-client-id",
					jwksURL,
				),
			},
		}

		first, err := session.New(firstInst, session.NormalRun, "")
		require.NoError(t, err)
		second, err := session.New(firstInst, session.NormalRun, "")
		require.NoError(t, err)
		kept, err := session.New(secondInst, session.NormalRun, "")
		require.NoError(t, err)

		logoutToken := makeSignedJWT(t, privateKey, kid, makeBackchannelLogoutClaims(
			"https://issuer.example/domain-fallback-context",
			"domain-fallback-client-id",
			"",
			map[string]interface{}{
				"sub":    "domain-fallback-sub",
				"domain": firstInst.Domain,
			},
		))

		env.client.POST("/oidc/logout").
			WithHost(firstInst.Domain).
			WithFormField("logout_token", logoutToken).
			Expect().
			Status(http.StatusOK)

		requireSessionDeleted(t, firstInst, first.ID())
		requireSessionDeleted(t, firstInst, second.ID())
		requireSessionExists(t, secondInst, kept.ID())
	})

	t.Run("ContextlessBackchannelLogoutWithoutSIDFallsBackToSubForCustomInstance", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)
		privateKey, kid, jwksURL := newBackchannelSigningKey(t)

		contextName := "custom-instance-fallback-context"
		targetSub := "custom-instance-target-sub"
		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: contextName,
			OIDCID:      targetSub,
		})
		secondDomain := "other-custom-" + firstInst.Domain
		_ = lifecycle.Destroy(secondDomain)
		secondInst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      secondDomain,
			ContextName: contextName,
			OIDCID:      "other-custom-instance-sub",
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(secondDomain) })

		oidcConfig := makeTestOIDCConfig(
			"https://issuer.example/custom-instance-fallback-context",
			"custom-instance-fallback-client-id",
			jwksURL,
		)
		oidcConfig["allow_custom_instance"] = true
		delete(oidcConfig, "userinfo_instance_field")
		config.GetConfig().Authentication = map[string]interface{}{
			contextName: map[string]interface{}{
				"oidc": oidcConfig,
			},
		}

		first, err := session.New(firstInst, session.NormalRun, "")
		require.NoError(t, err)
		second, err := session.New(firstInst, session.NormalRun, "")
		require.NoError(t, err)
		kept, err := session.New(secondInst, session.NormalRun, "")
		require.NoError(t, err)

		logoutToken := makeSignedJWT(t, privateKey, kid, makeBackchannelLogoutClaims(
			"https://issuer.example/custom-instance-fallback-context",
			"custom-instance-fallback-client-id",
			"",
			map[string]interface{}{
				"sub": targetSub,
			},
		))

		env.client.POST("/oidc/logout").
			WithHost(firstInst.Domain).
			WithFormField("logout_token", logoutToken).
			Expect().
			Status(http.StatusOK)

		requireSessionDeleted(t, firstInst, first.ID())
		requireSessionDeleted(t, firstInst, second.ID())
		requireSessionExists(t, secondInst, kept.ID())
	})
}

func TestResolveLogoutContext(t *testing.T) {
	t.Run("ContextHintWins", func(t *testing.T) {
		contextName, err := resolveLogoutContext("hinted-context", jwt.MapClaims{
			"sid": "ignored-sid",
			"iss": "https://issuer.example/ignored",
			"aud": "ignored-client",
		})
		require.NoError(t, err)
		require.Equal(t, "hinted-context", contextName)
	})

	t.Run("ResolvesByIssuerAudienceWithoutSID", func(t *testing.T) {
		config.UseTestFile(t)
		config.GetConfig().Authentication = map[string]interface{}{
			"issuer-audience-match": map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/issuer-audience-match",
					"issuer-audience-client-id",
					"https://example.org/jwks",
				),
			},
			"issuer-audience-other": map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/issuer-audience-other",
					"other-client-id",
					"https://example.org/jwks",
				),
			},
		}

		contextName, err := resolveLogoutContext("", jwt.MapClaims{
			"iss": "https://issuer.example/issuer-audience-match",
			"aud": "issuer-audience-client-id",
			"sub": "issuer-audience-sub",
		})
		require.NoError(t, err)
		require.Equal(t, "issuer-audience-match", contextName)
	})
}

func TestResolveLogoutContextWithSIDBindings(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	t.Run("UsesSIDCandidatesToNarrowIssuerAudienceMatches", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)

		targetContext := "z-resolve-target-context"
		otherContext := "a-resolve-other-context"
		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: targetContext,
		})
		secondDomain := "resolve-other-" + firstInst.Domain
		_ = lifecycle.Destroy(secondDomain)
		secondInst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      secondDomain,
			ContextName: otherContext,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(secondDomain) })

		config.GetConfig().Authentication = map[string]interface{}{
			targetContext: map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/resolve-target-context",
					"resolve-target-client-id",
					"https://example.org/jwks",
				),
			},
			otherContext: map[string]interface{}{
				"oidc": makeTestOIDCConfig(
					"https://issuer.example/resolve-other-context",
					"resolve-other-client-id",
					"https://example.org/jwks",
				),
			},
		}

		_, err = session.New(firstInst, session.NormalRun, "resolve-shared-sid")
		require.NoError(t, err)
		_, err = session.New(secondInst, session.NormalRun, "resolve-shared-sid")
		require.NoError(t, err)

		contextName, err := resolveLogoutContext("", jwt.MapClaims{
			"sid": "resolve-shared-sid",
			"iss": "https://issuer.example/resolve-target-context",
			"aud": "resolve-target-client-id",
		})
		require.NoError(t, err)
		require.Equal(t, targetContext, contextName)
	})

	t.Run("ReturnsAmbiguousWhenSIDMatchesMultipleContextsWithoutIssuerAudience", func(t *testing.T) {
		env := newBackchannelLogoutTestEnv(t)

		firstInst := env.setup.GetTestInstance(&lifecycle.Options{
			ContextName: "sid-only-ambiguous-target-context",
		})
		secondDomain := "sid-only-other-" + firstInst.Domain
		_ = lifecycle.Destroy(secondDomain)
		secondInst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      secondDomain,
			ContextName: "sid-only-ambiguous-other-context",
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(secondDomain) })

		_, err = session.New(firstInst, session.NormalRun, "sid-only-ambiguous")
		require.NoError(t, err)
		_, err = session.New(secondInst, session.NormalRun, "sid-only-ambiguous")
		require.NoError(t, err)

		_, err = resolveLogoutContext("", jwt.MapClaims{
			"sid": "sid-only-ambiguous",
		})
		require.ErrorIs(t, err, ErrAmbiguousLogout)
	})
}

func TestValidateLogoutTokenClaims(t *testing.T) {
	conf := &Config{ClientID: "provider-client-id"}
	makeClaims := func() jwt.MapClaims {
		claims := jwt.MapClaims(makeBackchannelLogoutClaims(
			"https://issuer.example/claims",
			"provider-client-id",
			"claims-sid",
			nil,
		))
		claims["iat"] = float64(time.Now().Unix())
		return claims
	}

	t.Run("ValidSubOnlyToken", func(t *testing.T) {
		claims := makeClaims()
		delete(claims, "sid")
		claims["sub"] = "claims-sub"
		require.NoError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true))
	})

	t.Run("MissingIssuer", func(t *testing.T) {
		claims := makeClaims()
		delete(claims, "iss")
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token is missing iss")
	})

	t.Run("WrongAudience", func(t *testing.T) {
		claims := makeClaims()
		claims["aud"] = "other-client-id"
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token audience mismatch")
	})

	t.Run("MissingIAT", func(t *testing.T) {
		claims := makeClaims()
		delete(claims, "iat")
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token is missing iat")
	})

	t.Run("FutureIAT", func(t *testing.T) {
		claims := makeClaims()
		claims["iat"] = float64(time.Now().Add(10 * time.Minute).Unix())
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token iat is in the future")
	})

	t.Run("MissingJTI", func(t *testing.T) {
		claims := makeClaims()
		delete(claims, "jti")
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token is missing jti")
	})

	t.Run("MissingEvents", func(t *testing.T) {
		claims := makeClaims()
		delete(claims, "events")
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token is missing events")
	})

	t.Run("MissingSIDAndSub", func(t *testing.T) {
		claims := makeClaims()
		delete(claims, "sid")
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/claims", true), "logout token must contain sid or sub")
	})

	t.Run("IssuerMismatchWhenStrict", func(t *testing.T) {
		claims := makeClaims()
		require.EqualError(t, oidcprovider.ValidateLogoutTokenClaims(claims, conf, "https://issuer.example/other", true), "logout token issuer mismatch: https://issuer.example/claims")
	})
}

type backchannelLogoutTestEnv struct {
	setup  *testutils.TestSetup
	server *httptest.Server
	client *httpexpect.Expect
}

func newBackchannelLogoutTestEnv(t *testing.T) *backchannelLogoutTestEnv {
	t.Helper()

	config.UseTestFile(t)
	config.GetConfig().Assets = "../../assets"
	testutils.NeedCouchdb(t)

	setup := testutils.NewSetup(t, t.Name())
	render, _ := statik.NewDirRenderer("../../assets")
	middlewares.BuildTemplates()

	handler := echo.New()
	group := handler.Group("/oidc")
	Routes(group)
	handler.Renderer = render
	handler.HTTPErrorHandler = errors.ErrorHandler
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	return &backchannelLogoutTestEnv{
		setup:  setup,
		server: server,
		client: testutils.CreateTestClient(t, server.URL),
	}
}

func newBackchannelSigningKey(t *testing.T) (*rsa.PrivateKey, string, string) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	kid := "backchannel-logout-test-key"
	jwksServer := newJWKSHandler(t, privateKey, kid)
	t.Cleanup(jwksServer.Close)

	return privateKey, kid, jwksServer.URL + "/jwks"
}

func makeTestOIDCConfig(issuer, clientID, jwksURL string) map[string]interface{} {
	return map[string]interface{}{
		"issuer":                  issuer,
		"client_id":               clientID,
		"client_secret":           "provider-secret",
		"scope":                   "openid profile email",
		"redirect_uri":            "https://example.org/callback",
		"authorize_url":           "https://example.org/authorize",
		"token_url":               "https://example.org/token",
		"userinfo_url":            "https://example.org/userinfo",
		"userinfo_instance_field": "domain",
		"id_token_jwk_url":        jwksURL,
	}
}

func requireSessionDeleted(t *testing.T, inst *instance.Instance, sessionID string) {
	t.Helper()

	_, err := session.Get(inst, sessionID)
	require.ErrorIs(t, err, session.ErrInvalidID)
}

func requireSessionExists(t *testing.T, inst *instance.Instance, sessionID string) {
	t.Helper()

	s, err := session.Get(inst, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionID, s.ID())
}

func createFlagshipOAuthClient(t *testing.T, inst *instance.Instance) *oauth.Client {
	t.Helper()

	client := &oauth.Client{
		RedirectURIs:    []string{"cozy://flagship"},
		ClientName:      "Cozy Flagship",
		ClientKind:      "mobile",
		SoftwareID:      "cozy-flagship",
		SoftwareVersion: "1.0.0",
	}
	require.Nil(t, client.Create(inst))

	storedClient, err := oauth.FindClient(inst, client.ClientID)
	require.NoError(t, err)
	storedClient.CertifiedFromStore = true
	require.NoError(t, storedClient.SetFlagship(inst))

	client, err = oauth.FindClient(inst, client.ClientID)
	require.NoError(t, err)
	require.True(t, client.Flagship)
	return client
}

func issueOIDCAccessTokenWithDelegatedCode(t *testing.T, e *httpexpect.Expect, inst *instance.Instance, client *oauth.Client, code string) {
	t.Helper()

	e.POST("/oidc/access_token").
		WithHost(inst.Domain).
		WithJSON(map[string]string{
			"client_id":     client.ClientID,
			"client_secret": client.ClientSecret,
			"scope":         "*",
			"code":          code,
		}).
		Expect().
		Status(http.StatusOK).
		JSON(httpexpect.ContentOpts{MediaType: "application/json"}).
		Object().
		Value("access_token").String().NotEmpty()
}

func requireOAuthClientDeleted(t *testing.T, inst *instance.Instance, clientID string) {
	t.Helper()

	_, err := oauth.FindClient(inst, clientID)
	require.Error(t, err)
	require.True(t, couchdb.IsNotFoundError(err))
}

func requireOAuthClientExists(t *testing.T, inst *instance.Instance, clientID string) {
	t.Helper()

	client, err := oauth.FindClient(inst, clientID)
	require.NoError(t, err)
	require.Equal(t, clientID, client.ClientID)
}

// makeUnsignedJWT creates a simple unsigned JWT for testing.
func makeUnsignedJWT(claims map[string]interface{}) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload, _ := json.Marshal(claims)
	payloadB64 := base64.RawURLEncoding.EncodeToString(payload)
	return header + "." + payloadB64 + "."
}

func makeSignedJWT(t *testing.T, privateKey *rsa.PrivateKey, kid string, claims map[string]interface{}) string {
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims(claims))
	token.Header["kid"] = kid
	signed, err := token.SignedString(privateKey)
	require.NoError(t, err)
	return signed
}

func newJWKSHandler(t *testing.T, privateKey *rsa.PrivateKey, kid string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/jwks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		e := big.NewInt(int64(privateKey.PublicKey.E)).Bytes()
		payload := map[string]interface{}{
			"keys": []map[string]string{{
				"kty": "RSA",
				"use": "sig",
				"kid": kid,
				"alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(e),
			}},
		}
		require.NoError(t, json.NewEncoder(w).Encode(payload))
	}))
}

func makeBackchannelLogoutClaims(issuer, audience, sid string, extra map[string]interface{}) map[string]interface{} {
	claims := map[string]interface{}{
		"iss":    issuer,
		"aud":    audience,
		"iat":    time.Now().Unix(),
		"jti":    "logout-jti-" + sid,
		"events": map[string]interface{}{"http://schemas.openid.net/event/backchannel-logout": map[string]interface{}{}},
	}
	if sid != "" {
		claims["sid"] = sid
	}
	for key, value := range extra {
		claims[key] = value
	}
	return claims
}
