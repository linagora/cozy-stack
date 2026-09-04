package rag

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ensureWorkspaceHTTP ---------------------------------------------------

func TestEnsureWorkspaceHTTP(t *testing.T) {
	t.Run("workspace already exists: GET 200 short-circuits", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			assert.Equal(t, http.MethodGet, req.Method)
			assert.Equal(t, "/partition/example.mycozy.cloud/workspaces/folder1", req.URL.Path)
			w.WriteHeader(http.StatusOK)
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1"}, nil }
		err := ensureWorkspaceHTTP(server, "example.mycozy.cloud", "folder1", resolve, TestingLogger())
		require.NoError(t, err)
		assert.Len(t, rec.All(), 1, "no further calls once the workspace is known to exist")
	})

	t.Run("workspace already exists: resolver is not called (laziness)", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		resolverCalled := false
		resolve := func() (string, []string, error) {
			resolverCalled = true
			return "", nil, fmt.Errorf("resolver must not be called on GET 200")
		}
		err := ensureWorkspaceHTTP(server, "example.mycozy.cloud", "folder1", resolve, TestingLogger())
		require.NoError(t, err)
		assert.False(t, resolverCalled, "resolver must stay lazy on the GET-200 short-circuit")
		assert.Len(t, rec.All(), 1)
	})

	t.Run("workspace missing: creates partition, workspace, and backfills", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				body, _ := io.ReadAll(req.Body)
				var payload map[string]string
				require.NoError(t, json.Unmarshal(body, &payload))
				assert.Equal(t, "folder1", payload["workspace_id"])
				assert.Equal(t, "My folder", payload["display_name"])
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				body, _ := io.ReadAll(req.Body)
				assert.Equal(t, []string{"file1", "file2"}, decodeFileIDs(t, body))
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1", "file2"}, nil }
		err := ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger())
		require.NoError(t, err)
		assert.Equal(t, 1, rec.CountPath("/partition/dom/workspaces/folder1"))
		assert.Equal(t, 1, rec.CountPath("/partition/dom"))
		assert.Equal(t, 1, rec.CountPath("/partition/dom/workspaces"))
		assert.Equal(t, 1, rec.CountPath("/partition/dom/workspaces/folder1/files"))
	})

	t.Run("workspace create 409 is tolerated and backfill still runs", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusConflict)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				w.WriteHeader(http.StatusConflict)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1"}, nil }
		err := ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger())
		require.NoError(t, err)
	})

	t.Run("backfill chunk 404 falls back to per-id retries (CRITICAL fix)", func(t *testing.T) {
		// openRAG's /files endpoint is all-or-nothing: any unknown id in the
		// batch 404s and adds NONE of the ids, even the indexable ones. A
		// single-id post must be attempted for every id in the chunk so
		// indexable files still get attached.
		var mu sync.Mutex
		workspaceExists := false
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				// 404 before creation; 200 for the disambiguation check that
				// follows the chunk 404.
				if workspaceExists {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				workspaceExists = true
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				body, _ := io.ReadAll(req.Body)
				ids := decodeFileIDs(t, body)
				if len(ids) > 1 {
					// batch contains the non-indexable "image1": reject all.
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if ids[0] == "image1" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1", "image1", "file2"}, nil }
		err := ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger())
		require.NoError(t, err)

		var singleIDBodies [][]string
		for _, rr := range rec.All() {
			if rr.Method == http.MethodPost && rr.Path == "/partition/dom/workspaces/folder1/files" {
				ids := decodeFileIDs(t, rr.Body)
				if len(ids) == 1 {
					singleIDBodies = append(singleIDBodies, ids)
				}
			}
		}
		// one retry per id in the rejected chunk
		assert.ElementsMatch(t, [][]string{{"file1"}, {"image1"}, {"file2"}}, singleIDBodies)
	})

	t.Run("backfill 403 is an ensure failure and rolls the workspace back", func(t *testing.T) {
		// A 4xx other than 404 (e.g. an auth or permission problem) is
		// systemic: succeeding would leave a silently incomplete workspace.
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				w.WriteHeader(http.StatusForbidden)
			case req.Method == http.MethodDelete && req.URL.Path == "/partition/dom/workspaces/folder1":
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1"}, nil }
		err := ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger())
		require.Error(t, err)
		var deletes int
		for _, rr := range rec.All() {
			if rr.Method == http.MethodDelete && rr.Path == "/partition/dom/workspaces/folder1" {
				deletes++
			}
		}
		assert.Equal(t, 1, deletes)
	})

	t.Run("backfill 404 with a vanished workspace is an ensure failure", func(t *testing.T) {
		// A chunk 404 normally means "some file id is not indexed" and is
		// retried per id. But when the workspace itself is gone (e.g. deleted
		// concurrently), the ensure must fail so the query does not proceed
		// scoped to a nonexistent workspace.
		var mu sync.Mutex
		created := false
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				// 404 on the initial check AND on the post-chunk-404
				// disambiguation check: the workspace vanished after its
				// creation was acknowledged.
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				created = true
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodDelete && req.URL.Path == "/partition/dom/workspaces/folder1":
				// rollback of the failed ensure
				w.WriteHeader(http.StatusNotFound)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1"}, nil }
		err := ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger())
		require.Error(t, err)
		mu.Lock()
		assert.True(t, created)
		mu.Unlock()
	})

	t.Run("backfill 5xx is an ensure failure and rolls the workspace back", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				w.WriteHeader(http.StatusInternalServerError)
			case req.Method == http.MethodDelete && req.URL.Path == "/partition/dom/workspaces/folder1":
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1"}, nil }
		err := ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger())
		require.Error(t, err)

		var deletes int
		for _, rr := range rec.All() {
			if rr.Method == http.MethodDelete && rr.Path == "/partition/dom/workspaces/folder1" {
				deletes++
			}
		}
		assert.Equal(t, 1, deletes, "the incomplete workspace must be deleted so a later ensure retries the backfill")
	})

	t.Run("failed backfill is retried from scratch on the next ensure", func(t *testing.T) {
		// First ensure: the backfill 5xx-fails, the workspace is rolled back.
		// Second ensure: the GET must see 404 again (not 200) and the whole
		// create+backfill sequence must rerun and succeed.
		var mu sync.Mutex
		workspaceExists := false
		failBackfill := true
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/workspaces/folder1":
				if workspaceExists {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom":
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				workspaceExists = true
				w.WriteHeader(http.StatusCreated)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/folder1/files":
				if failBackfill {
					failBackfill = false
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusOK)
				}
			case req.Method == http.MethodDelete && req.URL.Path == "/partition/dom/workspaces/folder1":
				workspaceExists = false
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		resolve := func() (string, []string, error) { return "My folder", []string{"file1"}, nil }
		require.Error(t, ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger()))
		require.NoError(t, ensureWorkspaceHTTP(server, "dom", "folder1", resolve, TestingLogger()))

		assert.Equal(t, 2, rec.CountPath("/partition/dom/workspaces"),
			"the workspace must be re-created by the second ensure")
		assert.Equal(t, 2, rec.CountPath("/partition/dom/workspaces/folder1/files"),
			"the backfill must rerun after the rollback")
	})
}

// --- reconcileMembershipHTTP -----------------------------------------------

func TestReconcileMembershipHTTP(t *testing.T) {
	t.Run("adds and removes exactly per diff", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/files/file1/workspaces":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"workspace_ids": []string{"ws-remove", "ws-keep"},
				})
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces/ws-add/files":
				body, _ := io.ReadAll(req.Body)
				assert.Equal(t, []string{"file1"}, decodeFileIDs(t, body))
				w.WriteHeader(http.StatusOK)
			case req.Method == http.MethodDelete && req.URL.Path == "/partition/dom/workspaces/ws-remove/files/file1":
				w.WriteHeader(http.StatusOK)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		known := map[string]string{"ws-add": "/a", "ws-keep": "/b", "ws-remove": "/c"}
		reconcileMembershipHTTP(server, "dom", "file1", []string{"ws-keep", "ws-add"}, known, TestingLogger())

		assert.Equal(t, 1, rec.CountPath("/partition/dom/workspaces/ws-add/files"))
		assert.Equal(t, 1, rec.CountPath("/partition/dom/workspaces/ws-remove/files/file1"))
		assert.Equal(t, 0, rec.CountPath("/partition/dom/workspaces/ws-keep/files"))
	})

	t.Run("foreign workspace in actual membership is never deleted", func(t *testing.T) {
		// The file's actual openRAG membership includes a workspace id the
		// stack does not manage (not in `known`). It must be left alone: no
		// DELETE call for it, even though it is absent from `desired`.
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/partition/dom/files/file1/workspaces":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"workspace_ids": []string{"ws-keep", "ws-foreign"},
				})
			case req.Method == http.MethodDelete:
				t.Fatalf("unexpected DELETE for foreign workspace: %s", req.URL.Path)
			default:
				t.Fatalf("unexpected request: %s %s", req.Method, req.URL.Path)
			}
		})

		known := map[string]string{"ws-keep": "/b"}
		reconcileMembershipHTTP(server, "dom", "file1", []string{"ws-keep"}, known, TestingLogger())

		for _, rr := range rec.All() {
			assert.NotEqual(t, http.MethodDelete, rr.Method, "no delete should be issued for a foreign workspace")
		}
	})

	t.Run("GET failure causes no mutations", func(t *testing.T) {
		server, rec := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})

		known := map[string]string{"ws-add": "/a"}
		reconcileMembershipHTTP(server, "dom", "file1", []string{"ws-add"}, known, TestingLogger())

		assert.Len(t, rec.All(), 1, "only the failed GET, no add/remove calls")
	})
}

func TestNilKBContextIsEmpty(t *testing.T) {
	// loadKBContext returns nil on any error or when no assistant declares a
	// knowledge base: reconcileMembership relies on empty() to gate that.
	var kb *kbContext
	assert.True(t, kb.empty())
}

func TestDirIDEscaping(t *testing.T) {
	// Folder ids are arbitrary CouchDB ids (UUIDs, fixed ids like the Drive
	// root, or anything a client stored): when used as a workspace id they
	// must stay a single URL path segment, whatever characters they contain.
	server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
		assert.Equal(t, "/partition/dom/workspaces/kb%2F..%2F1", req.URL.EscapedPath())
		w.WriteHeader(http.StatusOK)
	})

	exists, err := workspaceExists(server, "dom", "kb/../1")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestEnsureWorkspaceExists(t *testing.T) {
	t.Run("existing workspace: a single GET", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("kb")
		require.NoError(t, ensureWorkspaceExists(fake.Server, "dom", "kb", "Docs", TestingLogger()))
		assert.Len(t, fake.Rec.All(), 1)
	})

	t.Run("missing workspace: partition and workspace are created", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		require.NoError(t, ensureWorkspaceExists(fake.Server, "dom", "kb", "Docs", TestingLogger()))
		assert.True(t, fake.HasWorkspace("kb"))
		assert.Equal(t, 1, fake.Rec.Count(http.MethodPost, "/partition/dom"))
		reqs := fake.Rec.All()
		last := reqs[len(reqs)-1]
		assert.Equal(t, "/partition/dom/workspaces", last.Path)
		assert.Contains(t, string(last.Body), `"display_name":"Docs"`)
		assert.Contains(t, string(last.Body), `"workspace_id":"kb"`)
	})

	t.Run("409 on creation is tolerated", func(t *testing.T) {
		server, _ := newRAGTestServer(t, func(w http.ResponseWriter, req *http.Request) {
			switch {
			case req.Method == http.MethodGet:
				w.WriteHeader(http.StatusNotFound)
			case req.Method == http.MethodPost && req.URL.Path == "/partition/dom/workspaces":
				w.WriteHeader(http.StatusConflict)
			default:
				w.WriteHeader(http.StatusCreated)
			}
		})
		require.NoError(t, ensureWorkspaceExists(server, "dom", "kb", "Docs", TestingLogger()))
	})

	t.Run("5xx on creation is a retryable error", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.Fail = func(method, path string) int {
			if method == http.MethodPost && path == "/partition/dom/workspaces" {
				return 500
			}
			return 0
		}
		err := ensureWorkspaceExists(fake.Server, "dom", "kb", "Docs", TestingLogger())
		require.Error(t, err)
		assert.True(t, isRetryable(err))
	})
}

func TestDeleteWorkspace(t *testing.T) {
	fake := NewFakeOpenRAG(t)
	fake.AddWorkspace("kb")
	require.NoError(t, deleteWorkspace(fake.Server, "dom", "kb"))
	assert.False(t, fake.HasWorkspace("kb"))
	require.NoError(t, deleteWorkspace(fake.Server, "dom", "kb"), "404 is tolerated")
}
