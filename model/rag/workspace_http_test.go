package rag

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func TestCheckWorkspaceMissing(t *testing.T) {
	fake := NewFakeOpenRAG(t)
	exists, err := workspaceExists(fake.Server, "dom", "kb")
	require.NoError(t, err)
	assert.False(t, exists)
	fake.AddWorkspace("kb")
	exists, err = workspaceExists(fake.Server, "dom", "kb")
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
