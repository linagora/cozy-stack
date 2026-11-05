package sharings_test

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
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

// proxiedActors is what a request served by a test server left in its context.
type proxiedActors struct {
	// actor is the actor of the request.
	actor *middlewares.Actor
	// kept is the actor given by the sharing member kept in the context once
	// the handler has returned, whatever the actor of the request.
	kept *middlewares.Actor
}

func captureProxiedActors(ts *httptest.Server) <-chan proxiedActors {
	captured := make(chan proxiedActors, 8)
	ts.Config.Handler.(*echo.Echo).Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			err := next(c)
			var got proxiedActors
			got.actor, _ = middlewares.GetActor(c)
			if pdoc, perr := middlewares.GetPermission(c); perr == nil {
				middlewares.SetActor(c, nil)
				if rerr := middlewares.ResolveRequestActor(c, middlewares.GetInstance(c), pdoc); rerr == nil {
					got.kept, _ = middlewares.GetActor(c)
				}
			}
			captured <- got
			return err
		}
	})
	return captured
}

// The proxy swaps the token of the request for the drive token, on the same
// echo context. The member found for the former token, on the instance of the
// recipient, must not outlive that token.
func TestSharedDriveProxyKeptMember(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	env := setupSharedDrivesEnv(t)
	eA, eB, _ := env.createClients(t)

	// Betty's copy points to the owner's domain, so that the request takes the
	// same stack shortcut instead of the HTTP proxy.
	bettySharing, err := sharing.FindSharing(env.betty, env.firstSharingID)
	require.NoError(t, err)
	bettySharing.Members[0].Instance = "http://" + env.acme.Domain
	require.NoError(t, couchdb.UpdateDoc(env.betty, bettySharing))
	driveToken := bettySharing.Credentials[0].DriveToken

	// A sharing of Betty with Dave, unrelated to the drive. Its codes cover all
	// the files, so that the proxy accepts them on the instance of Betty.
	const daveEmail = "dave@example.net"
	wholeFiles := permission.Permission{
		Permissions: permission.Set{permission.Rule{Type: consts.Files, Verbs: permission.ALL}},
	}
	daveSharing := &sharing.Sharing{
		Active: true,
		Owner:  true,
		Rules: []sharing.Rule{{
			Title:   "dave",
			DocType: consts.Files,
			Values:  []string{"some-file-id"},
		}},
		Members: []sharing.Member{
			{Status: sharing.MemberStatusOwner, Instance: "http://" + env.betty.Domain},
			{
				Status:     sharing.MemberStatusReady,
				PublicName: "Dave",
				Email:      daveEmail,
				Instance:   "http://" + env.dave.Domain,
			},
		},
	}
	require.NoError(t, couchdb.CreateDoc(env.betty, daveSharing))

	interactCode, err := env.betty.CreateShareCode("interact:"+daveSharing.SID+":"+daveEmail,
		permission.ShareInteractPermissionID(daveSharing.SID))
	require.NoError(t, err)
	_, err = permission.CreateShareInteractSet(env.betty, daveSharing.SID,
		map[string]string{daveEmail: interactCode}, wholeFiles)
	require.NoError(t, err)

	previewCode, err := env.betty.CreateShareCode("preview:"+daveSharing.SID+":"+daveEmail, "")
	require.NoError(t, err)
	_, err = permission.CreateSharePreviewSet(env.betty, daveSharing.SID,
		map[string]string{daveEmail: previewCode}, nil, wholeFiles)
	require.NoError(t, err)

	nextActors := func(t *testing.T, captured <-chan proxiedActors) proxiedActors {
		t.Helper()
		select {
		case got := <-captured:
			return got
		case <-time.After(10 * time.Second):
			require.FailNow(t, "no request was served")
			return proxiedActors{}
		}
	}

	createArchive := func(t *testing.T, e *httpexpect.Expect, token string) {
		t.Helper()
		body := fmt.Sprintf(`{"data":{"attributes":{"ids":[%q]}}}`, env.checklistID)
		e.POST("/sharings/drives/"+env.firstSharingID+"/archive").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/vnd.api+json").
			WithBytes([]byte(body)).
			Expect().Status(200)
	}

	requireBetty := func(t *testing.T, actor *middlewares.Actor) {
		t.Helper()
		require.NotNil(t, actor)
		require.True(t, actor.Person)
		require.Equal(t, "Betty", actor.DisplayName)
		require.NotEqual(t, env.dave.Domain, actor.Domain)
	}

	proxied := captureProxiedActors(env.tsB)

	for name, code := range map[string]string{"Interact": interactCode, "Preview": previewCode} {
		t.Run(name+"CodeDoesNotOutliveTheSwap", func(t *testing.T) {
			createArchive(t, eB, code)

			got := nextActors(t, proxied)
			// The request still stands for the person of its original token...
			require.NotNil(t, got.actor)
			require.True(t, got.actor.Person)
			require.Equal(t, "Dave", got.actor.DisplayName)
			require.Equal(t, env.dave.Domain, got.actor.Domain)
			require.NotNil(t, got.actor.Instance)
			require.Equal(t, env.dave.Domain, got.actor.Instance.Domain)
			// ... but the member in the context is the one of the drive token.
			requireBetty(t, got.kept)
		})
	}

	t.Run("DriveTokenOnTheOwnerIsItsMember", func(t *testing.T) {
		direct := captureProxiedActors(env.tsA)
		createArchive(t, eA, driveToken)

		got := nextActors(t, direct)
		requireBetty(t, got.actor)
		requireBetty(t, got.kept)
	})
}
