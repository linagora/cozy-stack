package search

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildQuery(t *testing.T) {
	body := buildQuery("bob.example.net", "report", []string{"alice.example.net:d1"}, 20, nil)
	data, err := json.Marshal(body)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"size": 20,
		"query": {"bool": {
			"must": [{"bool": {"should": [
				{"match": {"name": {"query": "report", "minimum_should_match": "75%"}}},
				{"multi_match": {"query": "report", "type": "bool_prefix",
					"fields": ["name.prefix", "name.prefix._2gram", "name.prefix._3gram"]}},
				{"match": {"name.exact": {"query": "report", "boost": 4}}}
			], "minimum_should_match": 1}}],
			"filter": [{"bool": {"should": [
				{"term": {"owner_instance_id": "bob.example.net"}},
				{"terms": {"scoped_dir_key": ["alice.example.net:d1"]}}
			], "minimum_should_match": 1}}],
			"must_not": [{"term": {"trashed": true}}]
		}},
		"sort": [{"_score": "desc"}, {"resource_key": "asc"}]
	}`, string(data))

	t.Run("folder keys are sent in chunks", func(t *testing.T) {
		termsChunkSize = 2
		t.Cleanup(func() { termsChunkSize = 65536 })
		body := buildQuery("bob.example.net", "x", []string{"k1", "k2", "k3"}, 20, nil)
		filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		visible := filter[0].(map[string]any)["bool"].(map[string]any)["should"].([]any)
		require.Len(t, visible, 3)
		assert.Equal(t, []string{"k1", "k2"}, visible[1].(map[string]any)["terms"].(map[string]any)["scoped_dir_key"])
		assert.Equal(t, []string{"k3"}, visible[2].(map[string]any)["terms"].(map[string]any)["scoped_dir_key"])
	})

	t.Run("only own files without drives", func(t *testing.T) {
		body := buildQuery("bob.example.net", "x", nil, 20, nil)
		filter := body["query"].(map[string]any)["bool"].(map[string]any)["filter"].([]any)
		assert.Len(t, filter[0].(map[string]any)["bool"].(map[string]any)["should"], 1)
	})

	t.Run("next page", func(t *testing.T) {
		body := buildQuery("bob.example.net", "x", nil, 20, []any{1.5, "bob.example.net:f1"})
		assert.Equal(t, []any{1.5, "bob.example.net:f1"}, body["search_after"])
	})
}

func TestCursor(t *testing.T) {
	binding := cursorBinding("bob.example.net", "report")
	cursor, err := encodeCursor([]any{2.25, "alice.example.net:f1"}, binding)
	require.NoError(t, err)
	after, err := decodeCursor(cursor, binding)
	require.NoError(t, err)
	assert.Equal(t, []any{2.25, "alice.example.net:f1"}, after)

	after, err = decodeCursor("", binding)
	require.NoError(t, err)
	assert.Nil(t, after)

	for _, other := range []string{cursorBinding("bob.example.net", "budget"), cursorBinding("carol.example.net", "report")} {
		_, err := decodeCursor(cursor, other)
		assert.ErrorIs(t, err, ErrInvalidCursor, "a cursor is for the same caller and query")
	}
	for _, bad := range []string{"%%%", "bm90IGpzb24", "WzEsMiwzXQ", "WyJhIiwiYiJd"} {
		_, err := decodeCursor(bad, binding)
		assert.ErrorIs(t, err, ErrInvalidCursor, bad)
	}
}

func TestMemberScopes(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		_, _ = w.Write([]byte(`{"hits":{"hits":[
			{"_id":"s1","_source":{"sharing_id":"s1","owner_instance_id":"alice.example.net","root_dir_id":"root1",
				"active_member_principals":["bob.example.net"],
				"subtree_dir_keys":["alice.example.net:root1","alice.example.net:sub1"]}},
			{"_id":"s2","_source":{"sharing_id":"s2","owner_instance_id":"carol.example.net","root_dir_id":"root2",
				"active_member_principals":["bob.example.net","dave.example.net"],
				"subtree_dir_keys":["carol.example.net:root2"]}}]}}`))
	}))
	t.Cleanup(srv.Close)
	v := viper.New()
	v.Set("search.opensearch.url", srv.URL)
	require.NoError(t, config.UseViper(v))
	client := config.GetConfig().Search

	keys, drives, err := memberScopes(context.Background(), client, "bob.example.net")
	require.NoError(t, err)
	assert.JSONEq(t, `{"size":10000,"query":{"term":{"active_member_principals":"bob.example.net"}}}`, body)
	assert.Equal(t, map[string]string{
		"alice.example.net:root1": "s1",
		"alice.example.net:sub1":  "s1",
		"carol.example.net:root2": "s2",
	}, keys)
	assert.Equal(t, map[string]Drive{
		"s1": {OwnerInstanceID: "alice.example.net", RootDirID: "root1"},
		"s2": {OwnerInstanceID: "carol.example.net", RootDirID: "root2"},
	}, drives)
}

func TestSearchWithOpenSearch(t *testing.T) {
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
	caller := testutils.NewSetup(t, t.Name()+"_caller").GetTestInstance()
	client := config.GetConfig().Search
	ctx := context.Background()
	require.NoError(t, EnsureIndices(ctx, client))

	// Files of the caller
	own := createTextFile(t, caller, consts.RootDirID, "budget report.txt")
	trashed := createTextFile(t, caller, consts.RootDirID, "old report.txt")
	_, err := vfs.TrashFile(caller.VFS(), trashed)
	require.NoError(t, err)

	// A drive of the owner where the caller is a member, and one where not
	member := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://" + caller.Domain}
	stranger := sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://stranger.example.net"}
	shared := createDrive(t, owner, caller, "drive-member", "/Team", member)
	inDrive := createTextFile(t, owner, shared.ID(), "team report.txt")
	other := createDrive(t, owner, caller, "drive-stranger", "/Other", stranger)
	createTextFile(t, owner, other.ID(), "secret report.txt")
	createTextFile(t, owner, consts.RootDirID, "private report.txt")

	log := logger.WithNamespace("search-test")
	require.NoError(t, Index(ctx, owner, log))
	require.NoError(t, Index(ctx, caller, log))

	names := func(res *Results) map[string]SearchHit {
		out := make(map[string]SearchHit)
		for _, h := range res.Hits {
			out[h.Name] = h
		}
		return out
	}
	var res *Results
	require.Eventually(t, func() bool {
		res, err = Search(ctx, client, caller, "report", 0, "")
		return err == nil && len(res.Hits) == 2
	}, 5*time.Second, 200*time.Millisecond)
	found := names(res)
	require.Contains(t, found, "budget report.txt")
	require.Contains(t, found, "team report.txt")
	assert.Equal(t, SearchHit{
		DocID: own.ID(), OwnerInstanceID: caller.Domain, DirID: consts.RootDirID,
		Type: "file", Name: "budget report.txt", Mime: "text/plain", Class: "text",
		UpdatedAt: found["budget report.txt"].UpdatedAt,
	}, found["budget report.txt"])
	assert.Equal(t, "drive-member", found["team report.txt"].DriveID)
	assert.Equal(t, inDrive.ID(), found["team report.txt"].DocID)
	assert.Empty(t, res.Cursor, "a single page")

	t.Run("pages", func(t *testing.T) {
		first, err := Search(ctx, client, caller, "report", 1, "")
		require.NoError(t, err)
		require.Len(t, first.Hits, 1)
		require.NotEmpty(t, first.Cursor)
		second, err := Search(ctx, client, caller, "report", 1, first.Cursor)
		require.NoError(t, err)
		require.Len(t, second.Hits, 1)
		assert.NotEqual(t, first.Hits[0].DocID, second.Hits[0].DocID)
		assert.Empty(t, second.Cursor, "the last page, even if full")
	})

	t.Run("prefix of a word being typed", func(t *testing.T) {
		res, err := Search(ctx, client, caller, "budg", 0, "")
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "budget report.txt", res.Hits[0].Name)
	})

	t.Run("invalid cursor", func(t *testing.T) {
		_, err := Search(ctx, client, caller, "report", 0, "nope")
		assert.ErrorIs(t, err, ErrInvalidCursor)
		first, err := Search(ctx, client, caller, "report", 1, "")
		require.NoError(t, err)
		_, err = Search(ctx, client, caller, "budget", 1, first.Cursor)
		assert.ErrorIs(t, err, ErrInvalidCursor, "a cursor is for its own query")
		_, err = Search(ctx, client, owner, "report", 1, first.Cursor)
		assert.ErrorIs(t, err, ErrInvalidCursor, "and its own caller")
	})
}

// createDrive creates a folder shared as a drive by owner: the sharing on
// both sides, and the scope doc.
func createDrive(t *testing.T, owner, recipient *instance.Instance, id, path string, member sharing.Member) *vfs.DirDoc {
	t.Helper()
	root, err := vfs.Mkdir(owner.VFS(), path, nil)
	require.NoError(t, err)
	members := []sharing.Member{{Status: sharing.MemberStatusOwner, Instance: "https://" + owner.Domain}, member}
	rules := []sharing.Rule{{DocType: consts.Files, Values: []string{root.ID()}}}
	ownerSide := &sharing.Sharing{SID: id, Drive: true, Active: true, Owner: true, Description: path[1:], Rules: rules, Members: members}
	require.NoError(t, couchdb.CreateNamedDoc(owner, ownerSide))
	recipientSide := &sharing.Sharing{SID: id, Drive: true, Active: true, Description: path[1:], Rules: rules, Members: members}
	require.NoError(t, couchdb.CreateNamedDoc(recipient, recipientSide))
	doc, keep, err := NewScopeDocument(owner, ownerSide)
	require.NoError(t, err)
	require.True(t, keep)
	require.NoError(t, UpsertScope(context.Background(), config.GetConfig().Search, doc))
	return root
}
