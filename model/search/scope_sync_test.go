package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func folderChange(id, path string) couchdb.Change {
	return couchdb.Change{DocID: id, Doc: couchdb.JSONDoc{M: map[string]any{
		"_id": id, "type": consts.DirType, "path": path,
	}}}
}

func TestAffectedDrives(t *testing.T) {
	team := &sharing.Sharing{SID: "team", Drive: true, Owner: true}
	hr := &sharing.Sharing{SID: "hr", Drive: true, Owner: true}
	drives := []ownedDrive{
		{sharing: team, rootPath: "/Team", keys: map[string]bool{Key(domain, "team-root"): true, Key(domain, "old"): true}},
		{sharing: hr, rootPath: "/HR", keys: map[string]bool{Key(domain, "hr-root"): true}},
	}
	ids := func(list []*sharing.Sharing) []string {
		var out []string
		for _, s := range list {
			out = append(out, s.SID)
		}
		return out
	}

	cases := []struct {
		name    string
		folders []couchdb.Change
		want    []string
	}{
		{"created in a drive", []couchdb.Change{folderChange("new", "/Team/a")}, []string{"team"}},
		{"created deep in a drive", []couchdb.Change{folderChange("new", "/HR/a/b/c")}, []string{"hr"}},
		{"outside of the drives", []couchdb.Change{folderChange("new", "/Private")}, nil},
		{"sibling with the same prefix", []couchdb.Change{folderChange("new", "/Teamwork")}, nil},
		{"renamed in a drive", []couchdb.Change{folderChange("old", "/Team/renamed")}, nil},
		{"the root renamed", []couchdb.Change{folderChange("team-root", "/Team")}, nil},
		{"skipped folder created in a drive", []couchdb.Change{folderChange("new", "/Team/node_modules")}, []string{"team"}},
		{"created under a skipped folder", []couchdb.Change{folderChange("new", "/Team/node_modules/pkg")}, nil},
		{"moved under a skipped folder", []couchdb.Change{folderChange("old", "/Team/node_modules/old")}, []string{"team"}},
		{"moved out of a drive", []couchdb.Change{folderChange("old", "/Private/old")}, []string{"team"}},
		{"moved from a drive to another", []couchdb.Change{folderChange("old", "/HR/old")}, []string{"team", "hr"}},
		{"trashed", []couchdb.Change{folderChange("old", "/.cozy_trash/old")}, []string{"team"}},
		{"deleted", []couchdb.Change{{DocID: "old", Deleted: true}}, []string{"team"}},
		{"deleted elsewhere", []couchdb.Change{{DocID: "other", Deleted: true}}, nil},
		{"several changes, one rebuild per drive", []couchdb.Change{
			folderChange("n1", "/Team/a"), folderChange("n2", "/Team/b"), folderChange("old", "/Team/c"),
		}, []string{"team"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rebuild, deleted := affectedDrives(domain, tc.folders, nil, drives)
			assert.Equal(t, tc.want, ids(rebuild))
			assert.Empty(t, deleted)
		})
	}

	t.Run("sharing changes", func(t *testing.T) {
		sharings := []couchdb.Change{
			{DocID: "team", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "team", "drive": true, "owner": true, "active": true}}},
			{DocID: "recipient", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "recipient", "drive": true, "active": true}}},
			{DocID: "not-a-drive", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "not-a-drive", "owner": true}}},
			{DocID: "gone", Deleted: true},
			{DocID: "_design/sharings", Deleted: true},
		}
		rebuild, deleted := affectedDrives(domain, []couchdb.Change{folderChange("new", "/Team/a")}, sharings, drives)
		assert.Equal(t, []string{"team"}, ids(rebuild), "the folder and the sharing change: one rebuild")
		assert.Equal(t, []string{"gone"}, deleted)
	})
}

func TestSyncScopesWithOpenSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	osURL := os.Getenv("COZY_TEST_OPENSEARCH_URL")
	if osURL == "" {
		t.Skip("COZY_TEST_OPENSEARCH_URL is not set")
	}
	t.Setenv("COZY_SEARCH_OPENSEARCH_URL", osURL)
	t.Setenv("COZY_SEARCH_OPENSEARCH_INDEX_PREFIX", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	owner := testutils.NewSetup(t, t.Name()+"_owner").GetTestInstance()
	recipient := testutils.NewSetup(t, t.Name()+"_recipient").GetTestInstance()
	client := config.GetConfig().Search
	ctx := context.Background()
	log := logger.WithNamespace("search-test")
	pushJob = func(*instance.Instance) error { return nil }
	t.Cleanup(func() { pushJob = defaultPushJob })

	scope := func() *ScopeDocument {
		scopes, err := ownedScopes(ctx, client, owner.Domain, []string{"drive-1"})
		require.NoError(t, err)
		return scopes[0]
	}

	root, err := vfs.Mkdir(owner.VFS(), "/Team", nil)
	require.NoError(t, err)
	outside, err := vfs.Mkdir(owner.VFS(), "/Private", nil)
	require.NoError(t, err)
	folder, err := vfs.Mkdir(owner.VFS(), "/Private/Specs", nil)
	require.NoError(t, err)

	bob := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://" + recipient.Domain}
	carol := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://carol.example.net"}
	members := []sharing.Member{{Status: sharing.MemberStatusOwner, Instance: "https://" + owner.Domain}, bob, carol}
	rules := []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}}
	drive := &sharing.Sharing{SID: "drive-1", Drive: true, Active: true, Owner: true, Description: "Team", Rules: rules, Members: members}
	require.NoError(t, couchdb.CreateNamedDoc(owner, drive))
	copyOnRecipient := &sharing.Sharing{SID: "drive-1", Drive: true, Active: true, Description: "Team", Rules: rules, Members: members}
	require.NoError(t, couchdb.CreateNamedDoc(recipient, copyOnRecipient))

	t.Run("the backfill creates the scope of the drive", func(t *testing.T) {
		require.NoError(t, Index(ctx, owner, log))
		doc := scope()
		require.NotNil(t, doc)
		assert.Equal(t, []string{Key(owner.Domain, root.ID())}, doc.SubtreeDirKeys)
		assert.ElementsMatch(t, []string{recipient.Domain, "carol.example.net"}, doc.ActiveMemberPrincipals)
	})

	t.Run("a folder moved into the drive", func(t *testing.T) {
		rootID := root.ID()
		folder, err = vfs.ModifyDirMetadata(owner.VFS(), folder, &vfs.DocPatch{DirID: &rootID})
		require.NoError(t, err)
		// A sharing change in the same batch, for its checkpoint
		drive.Description = "Team drive"
		require.NoError(t, couchdb.UpdateDoc(owner, drive))

		// The files are indexed but not the scopes: both checkpoints stay
		checkpoints := func() [2]string {
			files, err := couchdb.LoadCheckpoint(owner, consts.Files, checkpointDocID)
			require.NoError(t, err)
			sharings, err := couchdb.LoadCheckpoint(owner, consts.Sharings, sharingsCheckpointDocID)
			require.NoError(t, err)
			return [2]string{files.LastSeq, sharings.LastSeq}
		}
		before := checkpoints()
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPut: // the indices exist
			case strings.HasSuffix(r.URL.Path, "/_mget"):
				w.WriteHeader(http.StatusServiceUnavailable)
			default:
				_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
			}
		}))
		t.Cleanup(fake.Close)
		u, _ := url.Parse(fake.URL)
		config.GetConfig().Search = opensearch.NewClient(opensearch.Options{URL: u})
		assert.Error(t, Index(ctx, owner, log))
		config.GetConfig().Search = client
		assert.Equal(t, before, checkpoints(), "the batch will be retried")

		require.NoError(t, Index(ctx, owner, log))
		assert.ElementsMatch(t, []string{Key(owner.Domain, root.ID()), Key(owner.Domain, folder.ID())}, scope().SubtreeDirKeys)
	})

	t.Run("a folder moved out of the drive", func(t *testing.T) {
		outsideID := outside.ID()
		folder, err = vfs.ModifyDirMetadata(owner.VFS(), folder, &vfs.DocPatch{DirID: &outsideID})
		require.NoError(t, err)
		require.NoError(t, Index(ctx, owner, log))
		assert.Equal(t, []string{Key(owner.Domain, root.ID())}, scope().SubtreeDirKeys)
	})

	t.Run("a folder deleted in the drive", func(t *testing.T) {
		tmp, err := vfs.Mkdir(owner.VFS(), "/Team/tmp", nil)
		require.NoError(t, err)
		require.NoError(t, Index(ctx, owner, log))
		require.Len(t, scope().SubtreeDirKeys, 2)
		require.NoError(t, owner.VFS().DestroyDirAndContent(tmp, func(vfs.TrashJournal) error { return nil }))
		require.NoError(t, Index(ctx, owner, log))
		assert.Equal(t, []string{Key(owner.Domain, root.ID())}, scope().SubtreeDirKeys)
	})

	t.Run("a recipient deleting its copy does not touch the scope", func(t *testing.T) {
		require.NoError(t, couchdb.DeleteDoc(recipient, copyOnRecipient))
		require.NoError(t, Index(ctx, recipient, log))
		assert.NotNil(t, scope())
	})

	t.Run("a member revoked", func(t *testing.T) {
		drive.Members[2].Status = sharing.MemberStatusRevoked
		require.NoError(t, couchdb.UpdateDoc(owner, drive))
		require.NoError(t, Index(ctx, owner, log))
		assert.Equal(t, []string{recipient.Domain}, scope().ActiveMemberPrincipals)
	})

	t.Run("the last member revoked", func(t *testing.T) {
		drive.Members[1].Status = sharing.MemberStatusRevoked
		require.NoError(t, couchdb.UpdateDoc(owner, drive))
		require.NoError(t, Index(ctx, owner, log))
		assert.Nil(t, scope())
	})
}
