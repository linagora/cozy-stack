package intents

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/intent"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/gavv/httpexpect/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestIntents(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	var err error
	var intentID string

	config.UseTestFile(t)
	const (
		contextName         = "intents-session-code-test"
		linkedAppSlug       = "drive"
		linkedAppSoftwareID = "registry://drive/stable"
	)
	conf := config.GetConfig()
	if conf.Authentication == nil {
		conf.Authentication = map[string]interface{}{}
	}
	conf.Authentication[contextName] = map[string]interface{}{
		"oidc": map[string]interface{}{
			"allow_app_token_exchange": true,
			"app_token_exchange": map[string]interface{}{
				"drive-web": map[string]interface{}{
					"software_id": linkedAppSoftwareID,
				},
			},
		},
	}
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	ins := setup.GetTestInstance(&lifecycle.Options{
		Domain:      "cozy.example.net",
		ContextName: contextName,
	})
	_, _ = setup.GetTestClient(consts.Settings)

	webapp := &couchdb.JSONDoc{
		Type: consts.Apps,
		M: map[string]interface{}{
			"_id":  consts.Apps + "/app",
			"slug": "app",
		},
	}
	require.NoError(t, couchdb.CreateNamedDoc(ins, webapp))

	appPerms, err := permission.CreateWebappSet(ins, "app", permission.Set{}, "1.0.0")
	if err != nil {
		require.NoError(t, err)
	}
	appToken := ins.BuildAppToken("app", "")

	customApp := &couchdb.JSONDoc{
		Type: consts.Apps,
		M: map[string]interface{}{
			"_id":             consts.Apps + "/custom",
			"slug":            "custom",
			"client_url_flag": "custom_url_flag",
		},
	}
	require.NoError(t, couchdb.CreateNamedDoc(ins, customApp))

	customAppPerms, err := permission.CreateWebappSet(ins, "custom", permission.Set{}, "1.0.0")
	require.NoError(t, err)
	customAppToken := ins.BuildAppToken("custom", "")
	files := &couchdb.JSONDoc{
		Type: consts.Apps,
		M: map[string]interface{}{
			"_id":  consts.Apps + "/files",
			"slug": "files",
			"intents": []app.Intent{
				{
					Action: "PICK",
					Types:  []string{"io.cozy.files", "image/gif"},
					Href:   "/pick",
				},
			},
		},
	}

	require.NoError(t, couchdb.CreateNamedDoc(ins, files))
	if _, err := permission.CreateWebappSet(ins, "files", permission.Set{}, "1.0.0"); err != nil {
		require.NoError(t, err)
	}
	filesToken := ins.BuildAppToken("files", "")

	driveApp := &couchdb.JSONDoc{
		Type: consts.Apps,
		M: map[string]interface{}{
			"_id":  consts.Apps + "/drive",
			"slug": "drive",
		},
	}
	require.NoError(t, couchdb.CreateNamedDoc(ins, driveApp))
	if _, err := permission.CreateWebappSet(ins, "drive", permission.Set{}, "1.0.0"); err != nil {
		require.NoError(t, err)
	}

	ts := setup.GetTestServer("/intents", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	createEligibleSessionCodeClient := func(t *testing.T, clientName string) *oauth.Client {
		t.Helper()

		oauthClient := &oauth.Client{
			ClientName:   clientName,
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   linkedAppSoftwareID,
		}
		require.Nil(t, oauthClient.Create(ins, oauth.SoftwareIDPrevalidated))
		return oauthClient
	}
	createEligibleSessionCodeToken := func(t *testing.T, clientName string) string {
		t.Helper()

		oauthClient := createEligibleSessionCodeClient(t, clientName)
		tok, err := ins.MakeJWT(consts.AccessTokenAudience,
			oauthClient.ClientID, oauth.BuildLinkedAppScope(linkedAppSlug), "", time.Now())
		require.NoError(t, err)
		return tok
	}

	t.Run("CreateIntent", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/intents").
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		intentID = checkIntentResult(obj, appPerms, true, "https://app.cozy.example.net")
		href := firstServiceHref(t, obj)
		u, err := url.Parse(href)
		require.NoError(t, err)
		require.Equal(t, intentID, u.Query().Get("intent"))
		require.Empty(t, u.Query().Get("session_code"))
	})

	t.Run("NewAPIIntentMatchesGetEndpoint", func(t *testing.T) {
		doc := &intent.Intent{}
		require.NoError(t, couchdb.GetDoc(ins, consts.Intents, intentID, doc))

		api := NewAPIIntent(doc, ins)
		raw, err := jsonapi.MarshalObject(api)
		require.NoError(t, err)

		var got map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &got))

		require.Equal(t, "io.cozy.intents", got["type"])
		require.Equal(t, intentID, got["id"])

		attrs := got["attributes"].(map[string]interface{})
		require.Equal(t, "PICK", attrs["action"])
		require.Equal(t, "io.cozy.files", attrs["type"])
		require.Equal(t, "https://app.cozy.example.net", attrs["client"])

		links := got["links"].(map[string]interface{})
		require.Equal(t, "/intents/"+intentID, links["self"])
	})

	t.Run("GetIntent", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.GET("/intents/"+intentID).
			WithHeader("Authorization", "Bearer "+filesToken).
			WithHeader("Accept", "application/vnd.api+json").
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		checkIntentResult(obj, appPerms, true, "https://app.cozy.example.net")
	})

	t.Run("GetIntentNotFromTheService", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/intents/"+intentID).
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Accept", "application/vnd.api+json").
			Expect().Status(403)
	})

	t.Run("CreateIntentOAuth", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/intents").
			WithHeader("Authorization", "Bearer "+appToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		checkIntentResult(obj, appPerms, false, "")
	})

	t.Run("CreateIntentWithOAuthLinkedApp", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		oauthClient := &oauth.Client{
			ClientName:   "test-oauth-linked-app",
			RedirectURIs: []string{"https://foobar"},
			SoftwareID:   "registry://drive/stable",
		}
		require.Nil(t, oauthClient.Create(ins, oauth.SoftwareIDPrevalidated))

		// Issue an OAuth access token (AccessTokenAudience) for that client,
		// with the linked-app scope so GetForOauth translates it via the
		// "drive" manifest.
		tok, err := ins.MakeJWT(consts.AccessTokenAudience,
			oauthClient.ClientID, "@io.cozy.apps/drive", "", time.Now())
		require.NoError(t, err)

		obj := e.POST("/intents").
			WithHeader("Authorization", "Bearer "+tok).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		data := obj.Value("data").Object()
		attrs := data.Value("attributes").Object()
		attrs.ValueEqual("client", "https://drive.cozy.example.net")
	})

	t.Run("CreateIntentWithForcedSessionCode", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		tok := createEligibleSessionCodeToken(t, "test-forced-session-linked-app")

		obj := expectPickIntentCreated(newPickIntentRequest(e, tok).
			WithQuery("force_session_id", "true"))

		data := obj.Value("data").Object()
		forcedIntentID := data.Value("id").String().NotEmpty().Raw()

		href := firstServiceHref(t, obj)
		u, err := url.Parse(href)
		require.NoError(t, err)
		require.Equal(t, forcedIntentID, u.Query().Get("intent"))
		sessionCode := u.Query().Get("session_code")
		require.NotEmpty(t, sessionCode)
		require.True(t, ins.CheckAndClearSessionCode(sessionCode))

		stored := &intent.Intent{}
		require.NoError(t, couchdb.GetDoc(ins, consts.Intents, forcedIntentID, stored))
		require.Len(t, stored.Services, 1)
		require.NotContains(t, stored.Services[0].Href, "session_code")
	})

	t.Run("CreateIntentWithForcedSessionCodeRejectsInvalidRequest", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		tok := createEligibleSessionCodeToken(t, "test-forced-session-invalid-intent")

		e.POST("/intents").
			WithQuery("force_session_id", "true").
			WithHeader("Authorization", "Bearer "+tok).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(422)
	})

	t.Run("CreateIntentWithForcedSessionCodeRejectsWebappToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		newPickIntentRequest(e, appToken).
			WithQuery("force_session_id", "true").
			Expect().Status(403)
	})

	t.Run("CreateIntentWithForcedSessionCodeRejectsForgedToken", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		oauthClient := createEligibleSessionCodeClient(t, "test-forged-session-linked-app")

		claims := permission.Claims{
			RegisteredClaims: jwt.RegisteredClaims{
				Audience:  jwt.ClaimStrings{consts.AccessTokenAudience},
				ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
				IssuedAt:  jwt.NewNumericDate(time.Now()),
				Issuer:    ins.Domain,
				Subject:   oauthClient.ClientID,
			},
			Scope: oauth.BuildLinkedAppScope(linkedAppSlug),
		}
		token := jwt.NewWithClaims(crypto.SigningMethod, claims)
		forgedAccessToken, err := token.SignedString([]byte("wrong-secret"))
		require.NoError(t, err)

		newPickIntentRequest(e, forgedAccessToken).
			WithQuery("force_session_id", "true").
			Expect().Status(403)
	})

	t.Run("CreateIntentWithClientURLMatchingFlag", func(t *testing.T) {
		ins.FeatureFlags = map[string]interface{}{"custom_url_flag": "https://flag.example.com"}
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/intents").
			WithHeader("Authorization", "Bearer "+customAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		checkIntentResult(obj, customAppPerms, true, "https://flag.example.com")
	})

	t.Run("CreateIntentWithClientURLNoMatchingFlag", func(t *testing.T) {
		ins.FeatureFlags = nil
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/intents").
			WithHeader("Authorization", "Bearer "+customAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		checkIntentResult(obj, customAppPerms, true, "https://custom.cozy.example.net")
	})

	t.Run("CreateIntentWithClientURLInvalidFlagValue", func(t *testing.T) {
		ins.FeatureFlags = map[string]interface{}{"custom_url_flag": "not-a-url"}
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/intents").
			WithHeader("Authorization", "Bearer "+customAppToken).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithHeader("Accept", "application/vnd.api+json").
			WithBytes([]byte(`{
        "data": {
          "type": "io.cozy.settings",
          "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
          }
        }
      }`)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		checkIntentResult(obj, customAppPerms, true, "https://custom.cozy.example.net")
	})
}

const pickIntentPayload = `{
  "data": {
    "type": "io.cozy.settings",
    "attributes": {
      "action": "PICK",
      "type": "io.cozy.files",
      "permissions": ["GET"]
    }
  }
}`

func newPickIntentRequest(e *httpexpect.Expect, token string) *httpexpect.Request {
	return e.POST("/intents").
		WithHeader("Authorization", "Bearer "+token).
		WithHeader("Content-Type", "application/vnd.api+json").
		WithHeader("Accept", "application/vnd.api+json").
		WithBytes([]byte(pickIntentPayload))
}

func expectPickIntentCreated(req *httpexpect.Request) *httpexpect.Object {
	return req.Expect().Status(200).
		JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
		Object()
}

func firstServiceHref(t *testing.T, obj *httpexpect.Object) string {
	t.Helper()

	attrs := obj.Value("data").Object().Value("attributes").Object()
	services := attrs.Value("services").Array()
	services.Length().Equal(1)
	return services.First().Object().Value("href").String().NotEmpty().Raw()
}

func checkIntentResult(obj *httpexpect.Object, appPerms *permission.Permission, fromWeb bool, expectedClient string) string {
	data := obj.Value("data").Object()
	data.ValueEqual("type", "io.cozy.intents")
	intentID := data.Value("id").String().NotEmpty().Raw()

	attrs := data.Value("attributes").Object()
	attrs.ValueEqual("action", "PICK")
	attrs.ValueEqual("type", "io.cozy.files")

	perms := attrs.Value("permissions").Array()
	perms.Length().Equal(1)
	perms.First().String().Equal("GET")

	if !fromWeb {
		return intentID
	}

	attrs.ValueEqual("client", expectedClient)

	links := data.Value("links").Object()
	links.ValueEqual("self", "/intents/"+intentID)
	links.ValueEqual("permissions", "/permissions/"+appPerms.ID())

	return intentID
}
