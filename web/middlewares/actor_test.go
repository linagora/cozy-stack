package middlewares

import (
	"net/http/httptest"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func TestResolveRequestActor(t *testing.T) {
	e := echo.New()
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	inst := &instance.Instance{Domain: "alice.cozy.local"}

	t.Run("AnonymousShare", func(t *testing.T) {
		err := ResolveRequestActor(c, inst, &permission.Permission{Type: permission.TypeShareByLink})
		require.NoError(t, err)

		actor, ok := GetActor(c)
		require.True(t, ok)
		require.Equal(t, vfs.TrashedByKindAnonymousShare, actor.Kind)
		require.Empty(t, actor.DisplayName)
		require.Empty(t, actor.Domain)
		require.Nil(t, actor.Instance)
		require.False(t, actor.Person)
	})

	t.Run("RegisterHasNoActor", func(t *testing.T) {
		err := ResolveRequestActor(c, inst, &permission.Permission{Type: permission.TypeRegister})
		require.NoError(t, err)

		_, ok := GetActor(c)
		require.False(t, ok)
	})
}

func TestActorPersonAndInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	alice := testutils.NewSetup(t, t.Name()+"-alice").GetTestInstance()
	bob := testutils.NewSetup(t, t.Name()+"-bob").GetTestInstance()

	resolve := func(t *testing.T, pdoc *permission.Permission, claims *permission.Claims) *Actor {
		t.Helper()
		e := echo.New()
		c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
		if claims != nil {
			c.Set("claims", *claims)
		}
		require.NoError(t, ResolveRequestActor(c, alice, pdoc))
		actor, ok := GetActor(c)
		require.True(t, ok)
		require.Equal(t, alice.Domain, actor.Domain)
		require.Same(t, alice, actor.Instance)
		return actor
	}

	t.Run("SessionAppTokenIsPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{Type: permission.TypeWebapp},
			&permission.Claims{SessionID: "session-id"})
		require.True(t, actor.Person)
	})

	t.Run("AppTokenWithoutSessionIsNotPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{Type: permission.TypeWebapp},
			&permission.Claims{})
		require.False(t, actor.Person)
	})

	t.Run("KonnectorIsNotPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{Type: permission.TypeKonnector}, nil)
		require.False(t, actor.Person)
	})

	t.Run("CLIIsNotPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{Type: permission.TypeCLI}, nil)
		require.False(t, actor.Person)
	})

	t.Run("FlagshipClientIsPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{
			Type:   permission.TypeOauth,
			Client: &oauth.Client{Flagship: true},
		}, &permission.Claims{Scope: "io.cozy.files"})
		require.True(t, actor.Person)
	})

	t.Run("DesktopClientIsPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{
			Type:   permission.TypeOauth,
			Client: &oauth.Client{ClientKind: "desktop"},
		}, &permission.Claims{Scope: "io.cozy.files"})
		require.True(t, actor.Person)
	})

	t.Run("WholeScopeOAuthTokenIsPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{
			Type:   permission.TypeOauth,
			Client: &oauth.Client{ClientKind: "mobile"},
		}, &permission.Claims{Scope: "*"})
		require.True(t, actor.Person)
	})

	t.Run("OtherOAuthClientIsNotPerson", func(t *testing.T) {
		actor := resolve(t, &permission.Permission{
			Type:   permission.TypeOauth,
			Client: &oauth.Client{ClientKind: "mobile"},
		}, &permission.Claims{Scope: "io.cozy.files"})
		require.False(t, actor.Person)
	})

	t.Run("ReadyLocalMemberHasInstance", func(t *testing.T) {
		actor := actorFromMember(alice, &sharing.Member{
			Status:   sharing.MemberStatusReady,
			Instance: "https://" + bob.Domain,
		})
		require.True(t, actor.Person)
		require.Equal(t, bob.Domain, actor.Domain)
		require.NotNil(t, actor.Instance)
		require.Equal(t, bob.Domain, actor.Instance.Domain)
	})

	t.Run("NotReadyLocalMemberHasNoInstance", func(t *testing.T) {
		for _, status := range []string{
			sharing.MemberStatusMailNotSent,
			sharing.MemberStatusPendingInvitation,
			sharing.MemberStatusSeen,
		} {
			actor := actorFromMember(alice, &sharing.Member{
				Status:   status,
				Instance: "https://" + bob.Domain,
			})
			require.True(t, actor.Person, status)
			require.Equal(t, bob.Domain, actor.Domain, status)
			require.Nil(t, actor.Instance, status)
		}
	})

	t.Run("RemoteMemberHasNoInstance", func(t *testing.T) {
		actor := actorFromMember(alice, &sharing.Member{
			Status:   sharing.MemberStatusReady,
			Instance: "https://ghost.example.net",
		})
		require.True(t, actor.Person)
		require.Equal(t, "ghost.example.net", actor.Domain)
		require.Nil(t, actor.Instance)
		require.Nil(t, MemberInstance(alice, "ghost.example.net"))
	})
}
