package sharings_test

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

// captureActors gives the actor of each request served by ts, as left in the
// echo context once the handler has returned (nil when there is none).
func captureActors(ts *httptest.Server) <-chan *middlewares.Actor {
	actors := make(chan *middlewares.Actor, 8)
	ts.Config.Handler.(*echo.Echo).Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			actor, _ := middlewares.GetActor(c)
			actors <- actor
			return err
		}
	})
	return actors
}

func TestSharedDriveProxyActor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	_, eB, _ := env.createClients(t)

	// Betty's copy points to the owner's domain, so that the request takes the
	// same stack shortcut instead of the HTTP proxy.
	bettySharing, err := sharing.FindSharing(env.betty, env.firstSharingID)
	require.NoError(t, err)
	bettySharing.Members[0].Instance = "http://" + env.acme.Domain
	require.NoError(t, couchdb.UpdateDoc(env.betty, bettySharing))

	// A token of a client of the desktop kind stands for Betty, whereas
	// env.bettyToken is an app token with no session, which does not.
	client := oauth.Client{
		RedirectURIs: []string{"http://localhost/oauth/callback"},
		ClientName:   "drive-proxy-actor",
		ClientKind:   "desktop",
		SoftwareID:   "github.com/cozy/cozy-stack/testing/drive-proxy-actor",
	}
	require.Nil(t, client.Create(env.betty, oauth.NotPending))
	personToken, err := env.betty.MakeJWT(consts.AccessTokenAudience, client.ClientID, consts.Files, "", time.Now())
	require.NoError(t, err)

	actors := captureActors(env.tsB)
	nextActor := func(t *testing.T) *middlewares.Actor {
		t.Helper()
		select {
		case actor := <-actors:
			return actor
		case <-time.After(10 * time.Second):
			require.FailNow(t, "no request was served")
			return nil
		}
	}

	createArchive := func(t *testing.T, token string) string {
		t.Helper()
		body := fmt.Sprintf(`{"data":{"attributes":{"ids":[%q]}}}`, env.checklistID)
		return eB.POST("/sharings/drives/"+env.firstSharingID+"/archive").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(body)).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Path("$.links.related").String().NotEmpty().Raw()
	}

	t.Run("PersonTokenStaysPerson", func(t *testing.T) {
		createArchive(t, personToken)

		actor := nextActor(t)
		require.NotNil(t, actor)
		require.True(t, actor.Person)
		require.Equal(t, env.betty.Domain, actor.Domain)
		require.NotNil(t, actor.Instance)
		require.Equal(t, env.betty.Domain, actor.Instance.Domain)
	})

	t.Run("OtherTokenIsNotTakenForDriveMember", func(t *testing.T) {
		createArchive(t, env.bettyToken)

		actor := nextActor(t)
		require.NotNil(t, actor)
		require.False(t, actor.Person)
		require.Equal(t, env.betty.Domain, actor.Domain)
	})

	t.Run("RouteWithoutAuthenticationIsNotPerson", func(t *testing.T) {
		related := createArchive(t, personToken)
		require.True(t, nextActor(t).Person)

		eB.GET(related).Expect().Status(200)

		actor := nextActor(t)
		require.NotNil(t, actor)
		require.False(t, actor.Person)
		require.Nil(t, actor.Instance)
		require.Equal(t, vfs.TrashedByKindAnonymousShare, actor.Kind)
		require.Empty(t, actor.Domain)
	})
}
