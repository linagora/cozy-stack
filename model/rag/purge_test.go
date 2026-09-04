package rag_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPurge(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	claimed := r.writeFile("/KB/a.txt", "alpha")
	unclaimed := r.writeFile("/Other/c.txt", "gamma")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))

	// Simulate leftovers: a file of an unclaimed folder, and a file that no
	// longer exists in the VFS.
	r.fake.AddFile(unclaimed.DocID, "x")
	r.fake.AddFile("ghost", "y")

	res, err := rag.Purge(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PurgeResult{Scanned: 3, Deleted: 2}, res)
	_, ok := r.fake.File(claimed.DocID)
	assert.True(t, ok)
	_, ok = r.fake.File(unclaimed.DocID)
	assert.False(t, ok)
	_, ok = r.fake.File("ghost")
	assert.False(t, ok)
}

func TestPurgeWithGlobalTrigger(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/Other")
	doc := r.writeFile("/Other/c.txt", "gamma")
	r.addTrigger(rag.IndexMessage{Doctype: consts.Files})
	r.fake.AddFile(doc.DocID, "x")
	r.fake.AddFile("ghost", "y")

	res, err := rag.Purge(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PurgeResult{Scanned: 2, Deleted: 1}, res)
	_, ok := r.fake.File(doc.DocID)
	assert.True(t, ok, "claimed by the global trigger")
}

// TestPurgeOrphanedFileIsDeleted covers the "not found" branch of the
// parent directory lookup: the directory doc is gone (deleted outside the
// VFS, bypassing trash), so the file is orphaned and purged even though a
// global trigger would otherwise claim it.
func TestPurgeOrphanedFileIsDeleted(t *testing.T) {
	r := newRAGTest(t)
	orphan := r.mkdir("/Orphan")
	doc := r.writeFile("/Orphan/o.txt", "epsilon")
	r.addTrigger(rag.IndexMessage{Doctype: consts.Files})
	r.fake.AddFile(doc.DocID, "x")

	// Delete the directory doc directly, bypassing VFS trash semantics, so
	// the file's dir_id now points at nothing.
	require.NoError(t, couchdb.DeleteDoc(r.inst, orphan))

	res, err := rag.Purge(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PurgeResult{Scanned: 1, Deleted: 1}, res)
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok, "the file's parent directory is gone: orphaned")
}

// TestPurgeAbortsOnDirLookupError covers the "any other error" branch: a
// directory lookup failure that is not a not-found (here: an id starting
// with "_", rejected by couchdb before any HTTP round-trip, so the failure
// is deterministic) must abort the purge rather than delete the file.
func TestPurgeAbortsOnDirLookupError(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addTrigger(rag.IndexMessage{Doctype: consts.Files})
	r.fake.AddFile(doc.DocID, "x")

	raw, err := r.inst.VFS().FileByID(doc.DocID)
	require.NoError(t, err)
	raw.DirID = "_bogus"
	require.NoError(t, couchdb.UpdateDoc(r.inst, raw))

	_, err = rag.Purge(r.inst, rag.TestingLogger())
	require.Error(t, err)
	_, ok := r.fake.File(doc.DocID)
	assert.True(t, ok, "the purge aborted before touching openRAG")
}

func TestReset(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	r.addTrigger(rag.IndexMessage{Doctype: consts.Files})
	require.NoError(t, r.index(msg))
	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	require.NotEmpty(t, cp.LastSeq)

	n, err := rag.Reset(r.inst, kb.DocID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	cp, err = rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.Empty(t, cp.LastSeq)

	n, err = rag.Reset(r.inst, "")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "without dir_id every rag-index trigger is reset")

	_, err = rag.Reset(r.inst, "unknown")
	assert.ErrorIs(t, err, rag.ErrNoIndexTrigger)

}
