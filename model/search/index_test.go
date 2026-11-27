package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
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
		// Its parent was not loaded: indexed
		{DocID: "f4", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "f4", "type": "file", "name": "b.txt", "dir_id": "gone"}}},
		{DocID: "f2", Deleted: true, Doc: couchdb.JSONDoc{M: map[string]any{"_id": "f2", "_deleted": true}}},
		{DocID: consts.RootDirID, Doc: couchdb.JSONDoc{M: map[string]any{"_id": consts.RootDirID, "type": "directory"}}},
		{DocID: "_design/by-path", Deleted: true},
		// In a skipped tree: removed, but the skipped folder itself is indexed
		{DocID: "f3", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "f3", "type": "file", "name": "index.js", "dir_id": "nm"}}},
		{DocID: "d2", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "d2", "type": "directory", "name": "lib", "path": "/code/node_modules/lib"}}},
		{DocID: "nm", Doc: couchdb.JSONDoc{M: map[string]any{"_id": "nm", "type": "directory", "name": "node_modules", "path": "/code/node_modules", "dir_id": "code"}}},
	}
	parents := map[string]*vfs.DirDoc{
		"d1": {DocID: "d1", Fullpath: "/Documents"},
		"nm": {DocID: "nm", Fullpath: "/code/node_modules"},
	}
	assert.Equal(t,
		`{"index":{"_id":"alice.example.net:f1"}}`+"\n"+
			`{"resource_key":"alice.example.net:f1","owner_instance_id":"alice.example.net","doc_id":"f1","dir_id":"d1",`+
			`"scoped_dir_key":"alice.example.net:d1","type":"file","name":"a.txt","trashed":false}`+"\n"+
			`{"index":{"_id":"alice.example.net:f4"}}`+"\n"+
			`{"resource_key":"alice.example.net:f4","owner_instance_id":"alice.example.net","doc_id":"f4","dir_id":"gone",`+
			`"scoped_dir_key":"alice.example.net:gone","type":"file","name":"b.txt","trashed":false}`+"\n"+
			`{"delete":{"_id":"alice.example.net:f2"}}`+"\n"+
			`{"delete":{"_id":"alice.example.net:f3"}}`+"\n"+
			`{"delete":{"_id":"alice.example.net:d2"}}`+"\n"+
			`{"index":{"_id":"alice.example.net:nm"}}`+"\n"+
			`{"resource_key":"alice.example.net:nm","owner_instance_id":"alice.example.net","doc_id":"nm","dir_id":"code",`+
			`"scoped_dir_key":"alice.example.net:nm","type":"directory","name":"node_modules","trashed":false}`+"\n",
		string(bulkActions(domain, changes, parents)))
	assert.Empty(t, bulkActions(domain, nil, nil))
}

func TestSkippedFolders(t *testing.T) {
	assert.False(t, contentSkipped("/"))
	assert.False(t, contentSkipped("/code/src"))
	assert.False(t, contentSkipped("/code/node_modules_old"))
	assert.False(t, contentSkipped("/code/Node_Modules"), "the names are case sensitive")
	assert.False(t, contentSkipped("/Purchases/vendor"), "vendor can hold documents")
	assert.True(t, contentSkipped("/code/node_modules"))
	assert.True(t, contentSkipped("/code/.git/objects"))
	for _, name := range []string{".venv", "__pycache__", ".next"} {
		assert.True(t, contentSkipped("/code/"+name+"/a"), name)
	}
	assert.False(t, folderSkipped("/code/node_modules"), "a skipped folder itself is indexed")
	assert.True(t, folderSkipped("/code/node_modules/pkg"))
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
	deleteTestIndices(t, osURL, client)
	ctx := context.Background()
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

	t.Run("skipped trees", func(t *testing.T) {
		pkg, err := vfs.MkdirAll(inst.VFS(), "/Reports/node_modules/pkg")
		require.NoError(t, err)
		nodeModulesID := pkg.DirID
		inside := createTextFile(t, inst, pkg.ID(), "index.js")
		reports, err := inst.VFS().DirByPath("/Reports")
		require.NoError(t, err)
		moved := createTextFile(t, inst, reports.ID(), "moved.js")
		require.NoError(t, Index(ctx, inst, log))
		docs := indexed(t, client, inst.Domain, nodeModulesID, pkg.ID(), inside.ID(), moved.ID())
		assert.Contains(t, docs, nodeModulesID, "node_modules itself is indexed")
		assert.NotContains(t, docs, pkg.ID())
		assert.NotContains(t, docs, inside.ID())
		require.Contains(t, docs, moved.ID())

		// A file moved into the skipped tree is removed
		_, err = vfs.ModifyFileMetadata(inst.VFS(), moved, &vfs.DocPatch{DirID: &nodeModulesID})
		require.NoError(t, err)
		require.NoError(t, Index(ctx, inst, log))
		assert.NotContains(t, indexed(t, client, inst.Domain, moved.ID()), moved.ID())
	})

	t.Run("no change", func(t *testing.T) {
		require.NoError(t, Index(ctx, inst, log))
		assert.Equal(t, 0, pushes)
	})

	t.Run("the first job of a client creates the indices", func(t *testing.T) {
		var mu sync.Mutex
		var requests []string
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.Path)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
		}))
		t.Cleanup(fake.Close)
		saved := config.GetConfig().Search
		u, _ := url.Parse(fake.URL)
		config.GetConfig().Search = opensearch.NewClient(opensearch.Options{URL: u})
		t.Cleanup(func() { config.GetConfig().Search = saved })

		createTextFile(t, inst, dir.ID(), "first.txt")
		require.NoError(t, Index(ctx, inst, log))
		createTextFile(t, inst, dir.ID(), "second.txt")
		require.NoError(t, Index(ctx, inst, log))
		mu.Lock()
		defer mu.Unlock()
		require.Len(t, requests, 4)
		assert.ElementsMatch(t, []string{"PUT /files_v1", "PUT /share_scopes_v1"}, requests[:2])
		assert.Equal(t, []string{"POST /files_v1/_bulk", "POST /files_v1/_bulk"}, requests[2:])
	})

	t.Run("an outage holds the checkpoint, a refused file does not", func(t *testing.T) {
		saved := config.GetConfig().Search
		t.Cleanup(func() { config.GetConfig().Search = saved })
		// useBulkAnswer installs a new client, whose indices are created and
		// whose bulk requests get this answer.
		useBulkAnswer := func(status int, answer string) {
			fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut {
					return
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(answer))
			}))
			t.Cleanup(fake.Close)
			u, _ := url.Parse(fake.URL)
			config.GetConfig().Search = opensearch.NewClient(opensearch.Options{URL: u})
		}
		item := func(status int, errType string) string {
			return fmt.Sprintf(`{"errors":true,"items":[{"index":{"_id":"x","status":%d,"error":{"type":%q,"reason":"r"}}}]}`,
				status, errType)
		}
		lastSeq := func() string {
			cp, err := couchdb.LoadCheckpoint(inst, consts.Files, checkpointDocID)
			require.NoError(t, err)
			return cp.LastSeq
		}

		createTextFile(t, inst, dir.ID(), "outage.txt")
		before := lastSeq()
		for _, tc := range []struct {
			name   string
			status int
			answer string
		}{
			{"a 503 on the request", http.StatusServiceUnavailable, `{"error":{"type":"unavailable","reason":"down"},"status":503}`},
			// A refused type, but for the request, not a document: no DocID
			{"a 4xx on the request", http.StatusBadRequest, `{"error":{"type":"mapper_parsing_exception","reason":"bad"},"status":400}`},
			{"an item in 429", http.StatusOK, item(http.StatusTooManyRequests, "es_rejected_execution_exception")},
			{"an item in 5xx", http.StatusOK, item(http.StatusInternalServerError, "exception")},
			{"a closed index", http.StatusOK, item(http.StatusBadRequest, "index_closed_exception")},
			{"a missing index", http.StatusOK, item(http.StatusNotFound, "index_not_found_exception")},
		} {
			useBulkAnswer(tc.status, tc.answer)
			assert.Error(t, Index(ctx, inst, log), tc.name)
			assert.Equal(t, before, lastSeq(), "%s: the batch will be retried", tc.name)
		}
		assert.Nil(t, readyClient.Load(), "the retry creates the missing index again")

		useBulkAnswer(http.StatusOK, item(http.StatusBadRequest, "mapper_parsing_exception"))
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
