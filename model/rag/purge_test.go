package rag_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
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
