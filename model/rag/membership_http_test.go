package rag

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexedMD5SumRetryable(t *testing.T) {
	fake := NewFakeOpenRAG(t)
	fake.AddFile("f1", "abc")
	fake.Fail = func(method, path string) int { return 503 }
	_, _, err := indexedMD5Sum(fake.Server, "dom", "f1")
	require.Error(t, err)
	assert.True(t, isRetryable(err))
}

func TestDeleteFromRAGHTTPRetryable(t *testing.T) {
	fake := NewFakeOpenRAG(t)
	fake.AddFile("f1", "abc")
	require.NoError(t, deleteFromRAGHTTP(fake.Server, "dom", "f1"))
	_, ok := fake.File("f1")
	assert.False(t, ok)
	require.NoError(t, deleteFromRAGHTTP(fake.Server, "dom", "f1"), "404 is tolerated")

	fake.Fail = func(method, path string) int { return 500 }
	err := deleteFromRAGHTTP(fake.Server, "dom", "f1")
	require.Error(t, err)
	assert.True(t, isRetryable(err))
}

func TestDetachFileHTTP(t *testing.T) {
	t.Run("removes the membership and deletes an unclaimed file", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("ws")
		fake.AddFile("f1", "abc", "ws")

		deleted, err := detachFileHTTP(fake.Server, "dom", "f1", "ws", TestingLogger())
		require.NoError(t, err)
		assert.True(t, deleted)
		_, ok := fake.File("f1")
		assert.False(t, ok)
	})

	t.Run("keeps a file still claimed by another workspace", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("ws")
		fake.AddWorkspace("other")
		fake.AddFile("f1", "abc", "ws", "other")

		deleted, err := detachFileHTTP(fake.Server, "dom", "f1", "ws", TestingLogger())
		require.NoError(t, err)
		assert.False(t, deleted)
		ff, ok := fake.File("f1")
		require.True(t, ok)
		assert.Equal(t, []string{"other"}, ff.Workspaces)
	})

	t.Run("deletes an indexed file that belongs to no workspace", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("ws")
		fake.AddFile("f1", "abc")

		deleted, err := detachFileHTTP(fake.Server, "dom", "f1", "ws", TestingLogger())
		require.NoError(t, err)
		assert.True(t, deleted)
		_, ok := fake.File("f1")
		assert.False(t, ok)
		assert.Equal(t, 0, fake.Rec.Count(http.MethodDelete, "/partition/dom/workspaces/ws/files/f1"), "nothing to remove")
	})

	t.Run("does nothing for a file openRAG does not know", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		deleted, err := detachFileHTTP(fake.Server, "dom", "nope", "ws", TestingLogger())
		require.NoError(t, err)
		assert.False(t, deleted)
		assert.Equal(t, 0, fake.Rec.Count(http.MethodDelete, "/indexer/partition/dom/file/nope"))
	})

	t.Run("5xx on the membership listing is retryable and mutates nothing", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("ws")
		fake.AddFile("f1", "abc", "ws")
		fake.Fail = func(method, path string) int {
			if method == http.MethodGet {
				return 502
			}
			return 0
		}
		_, err := detachFileHTTP(fake.Server, "dom", "f1", "ws", TestingLogger())
		require.Error(t, err)
		assert.True(t, isRetryable(err))
		ff, ok := fake.File("f1")
		require.True(t, ok)
		assert.Equal(t, []string{"ws"}, ff.Workspaces)
	})
}

func TestSyncMembership(t *testing.T) {
	t.Run("adds the missing and removes the extra memberships", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("keep")
		fake.AddWorkspace("add")
		fake.AddWorkspace("drop")
		fake.AddFile("f1", "abc", "keep", "drop")

		require.NoError(t, syncMembership(fake.Server, "dom", "f1", []string{"add", "keep"}))

		ff, _ := fake.File("f1")
		assert.ElementsMatch(t, []string{"keep", "add"}, ff.Workspaces)
	})

	t.Run("does nothing for a file openRAG does not know", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("ws")
		require.NoError(t, syncMembership(fake.Server, "dom", "nope", []string{"ws"}))
		assert.Equal(t, 0, fake.Rec.Count(http.MethodPost, "/partition/dom/workspaces/ws/files"))
	})

	t.Run("no request when already in sync", func(t *testing.T) {
		fake := NewFakeOpenRAG(t)
		fake.AddWorkspace("ws")
		fake.AddFile("f1", "abc", "ws")
		require.NoError(t, syncMembership(fake.Server, "dom", "f1", []string{"ws"}))
		assert.Len(t, fake.Rec.All(), 1, "only the listing")
	})
}
