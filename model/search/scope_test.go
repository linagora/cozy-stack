package search

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeDrive(rootID string, members ...sharing.Member) *sharing.Sharing {
	owner := sharing.Member{Status: sharing.MemberStatusOwner, Instance: "https://owner.example.net"}
	return &sharing.Sharing{
		SID:     "sharing-1",
		Drive:   true,
		Owner:   true,
		Active:  true,
		Rules:   []sharing.Rule{{DocType: consts.Files, Values: []string{rootID}}},
		Members: append([]sharing.Member{owner}, members...),
	}
}

func TestActivePrincipals(t *testing.T) {
	s := makeDrive("root",
		sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://bob.example.net"},
		sharing.Member{Status: sharing.MemberStatusPendingInvitation, Instance: "https://carol.example.net"},
		sharing.Member{Status: sharing.MemberStatusRevoked, Instance: "https://dave.example.net"},
		sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://erin.example.net:8443"},
		sharing.Member{Status: sharing.MemberStatusReady}, // no instance yet
	)
	assert.Equal(t, []string{"bob.example.net", "erin.example.net:8443"}, activePrincipals(s))
}

func TestScopeWrites(t *testing.T) {
	var path, body string
	mgetAnswer := `{"docs":[{"_id":"sharing-1","found":true,"_source":{"owner_instance_id":"owner.example.net"}}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.HasSuffix(r.URL.Path, "/_mget") {
			_, _ = w.Write([]byte(mgetAnswer))
			return
		}
		path, body = r.URL.Path, string(data)
		_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
	}))
	t.Cleanup(srv.Close)
	v := viper.New()
	v.Set("search.opensearch.url", srv.URL)
	require.NoError(t, config.UseViper(v))
	client := opensearch.New(config.GetConfig().Search)

	doc := &ScopeDocument{
		SharingID:              "sharing-1",
		OwnerInstanceID:        "owner.example.net",
		RootDirID:              "root",
		ActiveMemberPrincipals: []string{"bob.example.net"},
		SubtreeDirKeys:         []string{"owner.example.net:root"},
	}
	require.NoError(t, UpsertScope(context.Background(), client, doc))
	assert.Equal(t, "/share_scopes_v1/_bulk", path)
	assert.Equal(t, `{"index":{"_id":"sharing-1"}}`+"\n"+
		`{"sharing_id":"sharing-1","owner_instance_id":"owner.example.net","root_dir_id":"root",`+
		`"active_member_principals":["bob.example.net"],"subtree_dir_keys":["owner.example.net:root"]}`+"\n", body)

	body = ""
	require.NoError(t, DeleteScope(context.Background(), client, "bob.example.net", "sharing-1"))
	assert.Empty(t, body, "a recipient does not delete the owner's scope")

	require.NoError(t, DeleteScope(context.Background(), client, "owner.example.net", "sharing-1"))
	assert.Equal(t, `{"delete":{"_id":"sharing-1"}}`+"\n", body)

	body = ""
	mgetAnswer = `{"docs":[{"_id":"sharing-1","found":false}]}`
	require.NoError(t, DeleteScope(context.Background(), client, "owner.example.net", "sharing-1"))
	assert.Empty(t, body, "nothing to delete")
}

func TestNewScopeDocument(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	root, err := vfs.MkdirAll(fs, "/Drives/Team")
	require.NoError(t, err)
	a, err := vfs.MkdirAll(fs, "/Drives/Team/a")
	require.NoError(t, err)
	b, err := vfs.MkdirAll(fs, "/Drives/Team/a/b")
	require.NoError(t, err)
	c, err := vfs.MkdirAll(fs, "/Drives/Team/c")
	require.NoError(t, err)
	// Outside the drive, including a sibling whose name starts like the root
	_, err = vfs.MkdirAll(fs, "/Drives/Team2/x")
	require.NoError(t, err)
	_, err = vfs.MkdirAll(fs, "/Drives/other")
	require.NoError(t, err)

	bob := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://bob.example.net"}

	t.Run("drive with nested folders", func(t *testing.T) {
		doc, keep, err := NewScopeDocument(inst, makeDrive(root.ID(), bob))
		require.NoError(t, err)
		require.True(t, keep)
		assert.Equal(t, "sharing-1", doc.SharingID)
		assert.Equal(t, inst.Domain, doc.OwnerInstanceID)
		assert.Equal(t, root.ID(), doc.RootDirID)
		assert.Equal(t, []string{"bob.example.net"}, doc.ActiveMemberPrincipals)
		assert.Equal(t, Key(inst.Domain, root.ID()), doc.SubtreeDirKeys[0], "root first")
		expected := []string{
			Key(inst.Domain, root.ID()), Key(inst.Domain, a.ID()),
			Key(inst.Domain, b.ID()), Key(inst.Domain, c.ID()),
		}
		got := append([]string{}, doc.SubtreeDirKeys...)
		sort.Strings(expected)
		sort.Strings(got)
		assert.Equal(t, expected, got)
	})

	t.Run("folders are loaded page by page", func(t *testing.T) {
		subtreePageSize = 1
		t.Cleanup(func() { subtreePageSize = 10000 })
		doc, keep, err := NewScopeDocument(inst, makeDrive(root.ID(), bob))
		require.NoError(t, err)
		require.True(t, keep)
		assert.Len(t, doc.SubtreeDirKeys, 4)
	})

	t.Run("no accepted recipient", func(t *testing.T) {
		pending := sharing.Member{Status: sharing.MemberStatusPendingInvitation, Instance: "https://bob.example.net"}
		_, keep, err := NewScopeDocument(inst, makeDrive(root.ID(), pending))
		require.NoError(t, err)
		assert.False(t, keep)
	})

	t.Run("inactive, not owned or not a drive", func(t *testing.T) {
		for _, edit := range []func(*sharing.Sharing){
			func(s *sharing.Sharing) { s.Active = false },
			func(s *sharing.Sharing) { s.Owner = false },
			func(s *sharing.Sharing) { s.Drive = false },
		} {
			s := makeDrive(root.ID(), bob)
			edit(s)
			_, keep, err := NewScopeDocument(inst, s)
			require.NoError(t, err)
			assert.False(t, keep)
		}
	})

	t.Run("missing root", func(t *testing.T) {
		_, keep, err := NewScopeDocument(inst, makeDrive("does-not-exist", bob))
		require.NoError(t, err)
		assert.False(t, keep)
	})

	t.Run("root in the trash", func(t *testing.T) {
		trashed, err := vfs.MkdirAll(fs, "/Drives/Old")
		require.NoError(t, err)
		trashed, err = vfs.TrashDir(fs, trashed)
		require.NoError(t, err)
		_, keep, err := NewScopeDocument(inst, makeDrive(trashed.ID(), bob))
		require.NoError(t, err)
		assert.False(t, keep)
	})
}
