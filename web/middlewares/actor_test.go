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
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
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

func TestActorSharePreview(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	alice := testutils.NewSetup(t, t.Name()+"-alice").GetTestInstance()
	bob := testutils.NewSetup(t, t.Name()+"-bob").GetTestInstance()

	const bobEmail = "bob@example.net"

	newPreview := func(t *testing.T, status, memberInstance string) *permission.Permission {
		t.Helper()
		s := &sharing.Sharing{
			Active: true,
			Owner:  true,
			Rules: []sharing.Rule{{
				Title:   "test",
				DocType: consts.Files,
				Values:  []string{"some-file-id"},
			}},
			Members: []sharing.Member{
				{Status: sharing.MemberStatusOwner, Instance: "https://" + alice.Domain},
				{Status: status, Email: bobEmail, Instance: memberInstance},
			},
		}
		require.NoError(t, couchdb.CreateDoc(alice, s))
		perms, err := s.CreatePreviewPermissions(alice)
		require.NoError(t, err)
		return perms
	}

	newContext := func(token string) echo.Context {
		req := httptest.NewRequest("GET", "/", nil)
		if token != "" {
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
		}
		c := echo.New().NewContext(req, httptest.NewRecorder())
		c.Set("instance", alice)
		return c
	}

	// resolve goes through GetPermission, as a request does.
	resolve := func(t *testing.T, token string) *Actor {
		t.Helper()
		c := newContext(token)
		_, err := GetPermission(c)
		require.NoError(t, err)
		actor, ok := GetActor(c)
		require.True(t, ok)
		return actor
	}

	requireAnonymous := func(t *testing.T, actor *Actor, msg ...interface{}) {
		t.Helper()
		require.Equal(t, vfs.TrashedByKindAnonymousShare, actor.Kind, msg...)
		require.False(t, actor.Person, msg...)
		require.Nil(t, actor.Instance, msg...)
		require.Empty(t, actor.DisplayName, msg...)
		require.Empty(t, actor.Domain, msg...)
	}

	t.Run("NotAcceptedIsAnonymous", func(t *testing.T) {
		for _, status := range []string{
			sharing.MemberStatusMailNotSent,
			sharing.MemberStatusPendingInvitation,
			sharing.MemberStatusSeen,
		} {
			perms := newPreview(t, status, "https://"+bob.Domain)
			requireAnonymous(t, resolve(t, perms.Codes[bobEmail]), status)
		}
	})

	t.Run("AcceptedLocalMemberIsPerson", func(t *testing.T) {
		perms := newPreview(t, sharing.MemberStatusReady, "https://"+bob.Domain)
		actor := resolve(t, perms.Codes[bobEmail])
		require.True(t, actor.Person)
		require.Equal(t, vfs.TrashedByKindMember, actor.Kind)
		require.Equal(t, bob.Domain, actor.Domain)
		require.NotNil(t, actor.Instance)
		require.Equal(t, bob.Domain, actor.Instance.Domain)
	})

	t.Run("ShortCodeFindsTheMember", func(t *testing.T) {
		perms := newPreview(t, sharing.MemberStatusReady, "https://"+bob.Domain)
		actor := resolve(t, perms.ShortCodes[bobEmail])
		require.True(t, actor.Person)
		require.NotNil(t, actor.Instance)
		require.Equal(t, bob.Domain, actor.Instance.Domain)
	})

	t.Run("AcceptedRemoteMemberHasNoInstance", func(t *testing.T) {
		perms := newPreview(t, sharing.MemberStatusReady, "https://ghost.example.net")
		actor := resolve(t, perms.Codes[bobEmail])
		require.True(t, actor.Person)
		require.Equal(t, "ghost.example.net", actor.Domain)
		require.Nil(t, actor.Instance)
	})

	t.Run("NoMatchingMemberIsAnonymous", func(t *testing.T) {
		perms := newPreview(t, sharing.MemberStatusReady, "https://"+bob.Domain)
		c := newContext("not-the-code-of-a-member")
		require.NoError(t, ResolveRequestActor(c, alice, perms))
		actor, ok := GetActor(c)
		require.True(t, ok)
		requireAnonymous(t, actor)
	})

	t.Run("NoTokenIsAnonymous", func(t *testing.T) {
		perms := newPreview(t, sharing.MemberStatusReady, "https://"+bob.Domain)
		c := newContext("")
		require.NoError(t, ResolveRequestActor(c, alice, perms))
		actor, ok := GetActor(c)
		require.True(t, ok)
		requireAnonymous(t, actor)
	})

	t.Run("ShareByLinkStaysAnonymous", func(t *testing.T) {
		perms := newPreview(t, sharing.MemberStatusReady, "https://"+bob.Domain)
		c := newContext(perms.Codes[bobEmail])
		require.NoError(t, ResolveRequestActor(c, alice, &permission.Permission{
			Type:     permission.TypeShareByLink,
			SourceID: perms.SourceID,
		}))
		actor, ok := GetActor(c)
		require.True(t, ok)
		requireAnonymous(t, actor)
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
		require.Nil(t, memberInstance(alice, "ghost.example.net"))
	})
}
