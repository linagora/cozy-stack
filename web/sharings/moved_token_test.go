package sharings_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/sharing"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func unsignedJWT(t *testing.T, scope string) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
		enc(map[string]string{"scope": scope}) + ".AAAA"
}

func TestSharingRequestCannotForgeMovedToken(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	// safehttp blocks loopback addresses outside dev mode; enable it so the
	// in-memory httptest server is reachable.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	ts := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
		"/auth": func(g *echo.Group) {
			g.Use(middlewares.LoadSession)
			auth.Routes(g)
		},
		"/sharings": sharings.Routes,
	})
	t.Cleanup(ts.Close)

	sharingID := "x " + consts.Files + " " + consts.Contacts
	forged := unsignedJWT(t, consts.Sharings+":ALL:"+sharingID)

	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": forged})
	}))
	t.Cleanup(peer.Close)

	e := httpexpect.Default(t, ts.URL)

	e.PUT("/sharings/x").
		WithHeader("Content-Type", "application/vnd.api+json").
		WithJSON(map[string]any{"data": map[string]any{
			"id": sharingID, "type": consts.Sharings,
			"attributes": map[string]any{
				"description": "d",
				"moved_from":  peer.URL,
				"rules":       []sharing.Rule{{Title: "t", DocType: consts.Files, Values: []string{"abc"}}},
				"members": []map[string]any{
					{"status": "owner", "instance": peer.URL},
					{"status": "pending", "email": "a@example.net"},
				},
			},
		}}).
		Expect().Status(http.StatusCreated)

	stored, err := sharing.FindSharing(inst, sharingID)
	require.NoError(t, err)
	assert.Empty(t, stored.MovedFrom)

	client := e.POST("/auth/register").
		WithHeader("Accept", "application/json").
		WithJSON(map[string]any{
			"redirect_uris": []string{"https://attacker.example/cb"},
			"client_name":   "c",
			"software_id":   "s",
			"client_kind":   "sharing",
		}).
		Expect().Status(http.StatusCreated).
		JSON().Object()
	client.NotContainsKey("client_kind")

	e.POST("/auth/access_token").
		WithFormField("grant_type", "refresh_token").
		WithFormField("client_id", client.Value("client_id").String().Raw()).
		WithFormField("client_secret", client.Value("client_secret").String().Raw()).
		WithFormField("refresh_token", forged).
		Expect().Status(http.StatusBadRequest)
}
