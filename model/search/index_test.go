package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
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

func TestBulkActions(t *testing.T) {
	changes := []couchdb.Change{
		{DocID: "f1", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "f1", "type": "file", "name": "a.txt", "dir_id": "d1"}}},
		{DocID: "f2", Deleted: true, Doc: couchdb.JSONDoc{M: map[string]any{"_id": "f2", "_deleted": true}}},
		{DocID: consts.RootDirID, Doc: couchdb.JSONDoc{M: map[string]any{"_id": consts.RootDirID, "type": "directory"}}},
		{DocID: "_design/by-path", Deleted: true},
	}
	assert.Equal(t,
		`{"index":{"_id":"alice.example.net:f1"}}`+"\n"+
			`{"resource_key":"alice.example.net:f1","owner_instance_id":"alice.example.net","doc_id":"f1","dir_id":"d1",`+
			`"scoped_dir_key":"alice.example.net:d1","type":"file","name":"a.txt","trashed":false}`+"\n"+
			`{"delete":{"_id":"alice.example.net:f2"}}`+"\n",
		string(bulkActions(domain, changes)))
	assert.Empty(t, bulkActions(domain, nil))
}

func TestIndex(t *testing.T) {
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
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	client := config.GetConfig().Search
	require.NotNil(t, client)
	ctx := context.Background()
	require.NoError(t, EnsureIndices(ctx, client))
	log := logger.WithNamespace("search-test")

	var pushes int
	pushJob = func(*instance.Instance) error { pushes++; return nil }
	t.Cleanup(func() { pushJob = defaultPushJob })

	dir, err := vfs.Mkdir(inst.VFS(), "/Reports", nil)
	require.NoError(t, err)
	file := createTextFile(t, inst, dir.ID(), "2026.txt")

	t.Run("backfill", func(t *testing.T) {
		require.NoError(t, Index(ctx, inst, log))
		assert.Equal(t, 0, pushes)
		docs := indexed(t, client, inst.Domain, dir.ID(), file.ID(), consts.RootDirID, consts.TrashDirID)
		assert.Equal(t, "Reports", docs[dir.ID()]["name"])
		assert.Equal(t, "2026.txt", docs[file.ID()]["name"])
		assert.Equal(t, Key(inst.Domain, dir.ID()), docs[file.ID()]["scoped_dir_key"])
		assert.NotContains(t, docs, consts.RootDirID)
		assert.NotContains(t, docs, consts.TrashDirID)
	})

	t.Run("rename and delete", func(t *testing.T) {
		newName := "2026-final.txt"
		renamed, err := vfs.ModifyFileMetadata(inst.VFS(), file, &vfs.DocPatch{Name: &newName})
		require.NoError(t, err)
		other := createTextFile(t, inst, dir.ID(), "draft.txt")
		require.NoError(t, Index(ctx, inst, log))
		docs := indexed(t, client, inst.Domain, renamed.ID(), other.ID())
		assert.Equal(t, newName, docs[renamed.ID()]["name"])
		assert.Contains(t, docs, other.ID())

		require.NoError(t, inst.VFS().DestroyFile(other))
		require.NoError(t, Index(ctx, inst, log))
		docs = indexed(t, client, inst.Domain, other.ID())
		assert.NotContains(t, docs, other.ID())
	})

	t.Run("no change", func(t *testing.T) {
		require.NoError(t, Index(ctx, inst, log))
		assert.Equal(t, 0, pushes)
	})

	t.Run("a refused file does not hold the checkpoint, an outage does", func(t *testing.T) {
		status, answer := 0, ""
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(answer))
		}))
		t.Cleanup(fake.Close)
		saved := config.GetConfig().Search
		u, _ := url.Parse(fake.URL)
		config.GetConfig().Search = opensearch.NewClient(opensearch.Options{URL: u})
		t.Cleanup(func() { config.GetConfig().Search = saved })
		lastSeq := func() string {
			cp, err := couchdb.LoadCheckpoint(inst, consts.Files, checkpointDocID)
			require.NoError(t, err)
			return cp.LastSeq
		}

		createTextFile(t, inst, dir.ID(), "outage.txt")
		before := lastSeq()
		status, answer = http.StatusServiceUnavailable, `{"error":{"type":"unavailable","reason":"down"},"status":503}`
		require.Error(t, Index(ctx, inst, log))
		assert.Equal(t, before, lastSeq(), "the batch will be retried")

		status, answer = http.StatusOK, `{"errors":true,"items":[{"index":{"_id":"x","status":400,"error":{"type":"mapper_parsing_exception","reason":"bad"}}}]}`
		require.NoError(t, Index(ctx, inst, log))
		assert.NotEqual(t, before, lastSeq(), "the checkpoint moves on")
	})

	t.Run("a big feed is indexed by several jobs", func(t *testing.T) {
		batchSize = 1
		t.Cleanup(func() { batchSize = 500 })
		createTextFile(t, inst, dir.ID(), "a.txt")
		createTextFile(t, inst, dir.ID(), "b.txt")
		require.NoError(t, Index(ctx, inst, log))
		assert.Equal(t, 1, pushes, "changes remain: another job is pushed")
	})
}

var defaultPushJob = pushJob

func createTextFile(t *testing.T, inst *instance.Instance, dirID, name string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, dirID, 5, nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = f.Write([]byte("hello"))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return doc
}

// indexed returns the sources of the given docs found in the files index, by
// doc id.
func indexed(t *testing.T, client *opensearch.Client, domain string, ids ...string) map[string]map[string]any {
	t.Helper()
	keys := make([]string, len(ids))
	for i, id := range ids {
		keys[i] = Key(domain, id)
	}
	var res struct {
		Docs []struct {
			Found  bool           `json:"found"`
			Source map[string]any `json:"_source"`
		} `json:"docs"`
	}
	require.NoError(t, client.Mget(context.Background(), FilesIndex, keys, &res))
	out := make(map[string]map[string]any)
	for _, doc := range res.Docs {
		if doc.Found {
			out[doc.Source["doc_id"].(string)] = doc.Source
		}
	}
	return out
}
