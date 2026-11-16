package sharing

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/oauth"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTryTokenForMovedSharing(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	// safehttp blocks loopback addresses outside dev mode; enable it so the
	// in-memory httptest server is reachable.
	oldBuildMode := build.BuildMode
	build.BuildMode = build.ModeDev
	t.Cleanup(func() { build.BuildMode = oldBuildMode })

	token := func(scope string) string {
		enc := func(v any) string {
			b, err := json.Marshal(v)
			require.NoError(t, err)
			return base64.RawURLEncoding.EncodeToString(b)
		}
		return enc(map[string]string{"alg": "HS256", "typ": "JWT"}) + "." +
			enc(map[string]string{"scope": scope}) + ".AAAA"
	}

	// The old instance accepts every refresh token.
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		w.Header().Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": r.PostForm.Get("refresh_token")})
	}))
	t.Cleanup(old.Close)

	newClient := func(name string) *oauth.Client {
		cli, err := CreateOAuthClient(inst, &Member{Instance: "https://" + name + ".example.net", PublicName: name})
		require.NoError(t, err)
		return cli
	}
	movedSharing := func(id string, inbound *oauth.Client) *Sharing {
		s := &Sharing{
			SID:       id,
			Active:    true,
			MovedFrom: old.URL,
			UpdatedAt: time.Now(),
			Rules:     []Rule{{Title: "t", DocType: consts.Files, Values: []string{"abc"}}},
			Members: []Member{
				{Status: MemberStatusOwner, Instance: "https://owner.example.net"},
				{Status: MemberStatusReady, Instance: inst.PageURL("", nil)},
			},
			Credentials: []Credentials{{InboundClientID: inbound.ClientID}},
		}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, s))
		return s
	}

	t.Run("the inbound client of the sharing gets a new refresh token", func(t *testing.T) {
		cli := newClient("owner")
		s := movedSharing("moved-ok", cli)
		scope := consts.Sharings + ":ALL:" + s.SID

		refresh, claims, ok := TryTokenForMovedSharing(inst, cli, token(scope))
		require.True(t, ok)
		assert.NotEmpty(t, refresh)
		assert.Equal(t, scope, claims.Scope)
	})

	t.Run("another client is refused", func(t *testing.T) {
		s := movedSharing("moved-other-client", newClient("owner2"))
		other := newClient("mallory")

		_, _, ok := TryTokenForMovedSharing(inst, other, token(consts.Sharings+":ALL:"+s.SID))
		assert.False(t, ok)
	})

	t.Run("a scope with several rules is refused", func(t *testing.T) {
		cli := newClient("owner3")
		s := movedSharing("moved "+consts.Files, cli)

		_, _, ok := TryTokenForMovedSharing(inst, cli, token(consts.Sharings+":ALL:"+s.SID))
		assert.False(t, ok)
	})
}
