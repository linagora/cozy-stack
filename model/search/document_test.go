package search

import (
	"encoding/json"
	"testing"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const domain = "alice.example.net"

func TestNewFileDocument(t *testing.T) {
	file := map[string]any{
		"_id": "f1", "_rev": "1-a", "type": "file", "name": "report.pdf",
		"dir_id": "d1", "mime": "application/pdf", "class": "pdf",
		"updated_at": "2026-11-15T10:00:00Z", "trashed": false, "size": "42",
	}
	dir := map[string]any{
		"_id": "d1", "type": "directory", "name": "Work", "dir_id": consts.RootDirID,
		"path": "/Work", "updated_at": "2026-11-14T09:00:00Z",
	}

	t.Run("file", func(t *testing.T) {
		fd, ok := NewFileDocument(domain, couchdb.JSONDoc{M: file})
		require.True(t, ok)
		assert.Equal(t, &FileDocument{
			ResourceKey:     "alice.example.net:f1",
			OwnerInstanceID: domain,
			DocID:           "f1",
			DirID:           "d1",
			ScopedDirKey:    "alice.example.net:d1",
			Type:            "file",
			Name:            "report.pdf",
			Mime:            "application/pdf",
			Class:           "pdf",
			UpdatedAt:       "2026-11-15T10:00:00Z",
		}, fd)
	})

	t.Run("folder is in its own scope", func(t *testing.T) {
		fd, ok := NewFileDocument(domain, couchdb.JSONDoc{M: dir})
		require.True(t, ok)
		assert.Equal(t, &FileDocument{
			ResourceKey:     "alice.example.net:d1",
			OwnerInstanceID: domain,
			DocID:           "d1",
			DirID:           consts.RootDirID,
			ScopedDirKey:    "alice.example.net:d1",
			Type:            "directory",
			Name:            "Work",
			UpdatedAt:       "2026-11-14T09:00:00Z",
		}, fd)
	})

	t.Run("JSON sent to OpenSearch", func(t *testing.T) {
		fd, ok := NewFileDocument(domain, couchdb.JSONDoc{M: file})
		require.True(t, ok)
		data, err := json.Marshal(fd)
		require.NoError(t, err)
		assert.JSONEq(t, `{"resource_key":"alice.example.net:f1","owner_instance_id":"alice.example.net",
			"doc_id":"f1","dir_id":"d1","scoped_dir_key":"alice.example.net:d1","type":"file",
			"name":"report.pdf","mime":"application/pdf","class":"pdf",
			"updated_at":"2026-11-15T10:00:00Z","trashed":false}`, string(data))

		// A folder without path: no mime, no class, not trashed
		noPath := map[string]any{"_id": "d2", "type": "directory", "name": "Old", "dir_id": consts.RootDirID}
		fd, ok = NewFileDocument(domain, couchdb.JSONDoc{M: noPath})
		require.True(t, ok)
		data, err = json.Marshal(fd)
		require.NoError(t, err)
		assert.JSONEq(t, `{"resource_key":"alice.example.net:d2","owner_instance_id":"alice.example.net",
			"doc_id":"d2","dir_id":"io.cozy.files.root-dir","scoped_dir_key":"alice.example.net:d2",
			"type":"directory","name":"Old","trashed":false}`, string(data))
	})

	trashed := []struct {
		name string
		doc  map[string]any
	}{
		{"trashed file", with(file, "trashed", true)},
		{"folder in the trash", with(dir, "path", "/.cozy_trash/Work")},
		{"folder deep in the trash", with(dir, "path", "/.cozy_trash/Work/2026")},
	}
	for _, tc := range trashed {
		t.Run(tc.name, func(t *testing.T) {
			fd, ok := NewFileDocument(domain, couchdb.JSONDoc{M: tc.doc})
			require.True(t, ok)
			assert.True(t, fd.Trashed)
		})
	}

	t.Run("folder named like the trash is not trashed", func(t *testing.T) {
		fd, ok := NewFileDocument(domain, couchdb.JSONDoc{M: with(dir, "path", "/.cozy_trash_old")})
		require.True(t, ok)
		assert.False(t, fd.Trashed)
	})

	skipped := []struct {
		name string
		doc  map[string]any
	}{
		{"root folder", with(dir, "_id", consts.RootDirID)},
		{"trash folder", with(dir, "_id", consts.TrashDirID)},
		{"design doc", map[string]any{"_id": "_design/by-path", "language": "query"}},
		{"deleted doc", map[string]any{"_id": "f1", "_rev": "2-b", "_deleted": true}},
		{"no id", map[string]any{"type": "file", "name": "x"}},
		{"no known type", with(file, "type", "")},
	}
	for _, tc := range skipped {
		t.Run("skip "+tc.name, func(t *testing.T) {
			_, ok := NewFileDocument(domain, couchdb.JSONDoc{M: tc.doc})
			assert.False(t, ok)
		})
	}
}

// with returns a copy of doc with key set to value.
func with(doc map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(doc)+1)
	for k, v := range doc {
		out[k] = v
	}
	out[key] = value
	return out
}
