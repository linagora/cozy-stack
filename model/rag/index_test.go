package rag

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDecodeMD5SumFromFileDoc pins the contract decodeMD5Sum relies on: a
// FileDoc serializes its digest the way the changes feed then carries it, and
// decoding that gives back the hexadecimal digest the RAG server is given.
func TestDecodeMD5SumFromFileDoc(t *testing.T) {
	// The md5sum of "test".
	sum := []byte{0x09, 0x8f, 0x6b, 0xcd, 0x46, 0x21, 0xd3, 0x73,
		0xca, 0xde, 0x4e, 0x83, 0x26, 0x27, 0xb4, 0xf6}

	raw, err := json.Marshal(&vfs.FileDoc{MD5Sum: sum})
	require.NoError(t, err)

	var doc map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &doc))

	assert.Equal(t, fmt.Sprintf("%x", sum), decodeMD5Sum(doc["md5sum"]))
}

func TestDecodeMD5Sum(t *testing.T) {
	const (
		hexSum    = "098f6bcd4621d373cade4e832627b4f6"
		base64Sum = "CY9rzUYh03PK3k6DJie09g=="
	)

	// What the changes feed carries: CouchDB serializes the digest bytes in
	// base64.
	assert.Equal(t, hexSum, decodeMD5Sum(base64Sum))

	assert.Equal(t, "", decodeMD5Sum(""))
	assert.Equal(t, "", decodeMD5Sum(nil))
	assert.Equal(t, "", decodeMD5Sum("not a digest"))
	assert.Equal(t, "", decodeMD5Sum("dG9vIHNob3J0"))   // valid base64, 9 bytes
	assert.Equal(t, "", decodeMD5Sum(map[string]int{})) // not even a string
}

func TestIsClassAllowed(t *testing.T) {
	off := &feature.Flags{M: map[string]interface{}{}}
	on := &feature.Flags{M: map[string]interface{}{
		"rag.index.image.enabled": true,
		"rag.index.video.enabled": true,
		"rag.index.audio.enabled": true,
	}}

	// Text-based files do not depend on any flag.
	assert.True(t, isClassAllowed(off, "text"))
	assert.True(t, isClassAllowed(off, ""))

	for _, class := range []string{consts.ImageClass, consts.VideoClass, consts.AudioClass} {
		assert.False(t, isClassAllowed(off, class), class)
		assert.True(t, isClassAllowed(on, class), class)
	}
}

// TestUploadMetaCreatedAt pins that the creation date of the file document
// reaches the RAG server, whether the file comes from the changes feed or
// from a VFS document, in the RFC 3339 form CouchDB stores it in.
func TestUploadMetaCreatedAt(t *testing.T) {
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	doc := &vfs.FileDoc{
		DocID:     "a1b2c3",
		DocRev:    "3-abc",
		CreatedAt: created,
		UpdatedAt: created.Add(time.Hour),
		Metadata:  vfs.Metadata{"datetime": "2026-03-01T00:00:00Z"},
	}

	t.Run("from a VFS document", func(t *testing.T) {
		meta := uploadMeta(fileInfoFromDoc(doc))
		assert.Equal(t, "2026-03-04T05:06:07Z", meta["created_at"])
		assert.Equal(t, "2026-03-01T00:00:00Z", meta["datetime"])
		assert.Equal(t, "3-abc", meta["doc_rev"])
		assert.Equal(t, consts.Files, meta["doctype"])
	})

	t.Run("from the changes feed", func(t *testing.T) {
		raw, err := json.Marshal(doc)
		require.NoError(t, err)
		var change couchdb.Change
		change.DocID = doc.DocID
		require.NoError(t, json.Unmarshal(raw, &change.Doc))

		meta := uploadMeta(fileInfoFromChange(change))
		assert.Equal(t, "2026-03-04T05:06:07Z", meta["created_at"])
		assert.Equal(t, "2026-03-01T00:00:00Z", meta["datetime"])
	})

	t.Run("a file without a creation date sends none", func(t *testing.T) {
		meta := uploadMeta(fileInfo{})
		assert.Empty(t, meta["created_at"])
	})
}

// TestClassifyConflict pins the two 409 openRAG answers on the upload route,
// captured against the real server: the id it already holds (a PUT fixes it)
// and the content it already indexed under another id (nothing fixes it).
func TestClassifyConflict(t *testing.T) {
	t.Run("the file id already exists", func(t *testing.T) {
		body := []byte(`{"detail":"File 'abc123' already exists in partition alice.cozy.example"}`)
		conflict, existingID := classifyConflict(body)
		assert.Equal(t, conflictIDExists, conflict)
		assert.Empty(t, existingID)
	})

	t.Run("the content already exists", func(t *testing.T) {
		body := []byte(`{"detail":"[DOCUMENT_CONTENT_EXISTS]: This document already exists in partition 'alice.cozy.example'.","extra":{"existing_file_id":"other456","request_id":"r-1"}}`)
		conflict, existingID := classifyConflict(body)
		assert.Equal(t, conflictContentExists, conflict)
		assert.Equal(t, "other456", existingID)
	})

	t.Run("an unknown body", func(t *testing.T) {
		for _, body := range []string{
			`{"detail":"the partition is locked"}`,
			`{"detail":[{"loc":["body"],"msg":"nope"}]}`,
			`<html>409</html>`,
			``,
		} {
			conflict, existingID := classifyConflict([]byte(body))
			assert.Equal(t, conflictUnknown, conflict, body)
			assert.Empty(t, existingID, body)
		}
	})
}
