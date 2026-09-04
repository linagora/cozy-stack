package rag_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoveFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/KB/sub")
	a := r.writeFile("/KB/a.txt", "alpha")
	b := r.writeFile("/KB/sub/b.txt", "beta")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))
	require.True(t, r.fake.HasWorkspace(kb.DocID))

	require.NoError(t, r.index(rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID, Action: rag.ActionRemove}))

	_, ok := r.fake.File(a.DocID)
	assert.False(t, ok)
	_, ok = r.fake.File(b.DocID)
	assert.False(t, ok)
	assert.False(t, r.fake.HasWorkspace(kb.DocID))
	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.Equal(t, rag.Checkpoint{}, cp)

	require.NoError(t, r.index(rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID, Action: rag.ActionRemove}), "idempotent")
}

func TestRemoveFolderKeepsFilesClaimedElsewhere(t *testing.T) {
	r := newRAGTest(t)
	outer := r.mkdir("/Outer")
	inner := r.mkdir("/Outer/Inner")
	doc := r.writeFile("/Outer/Inner/x.txt", "x")
	msgOuter := rag.IndexMessage{Doctype: consts.Files, DirID: outer.DocID}
	msgInner := rag.IndexMessage{Doctype: consts.Files, DirID: inner.DocID}
	r.addTrigger(msgOuter)
	r.addTrigger(msgInner)
	require.NoError(t, r.index(msgOuter))
	require.NoError(t, r.index(msgInner))

	require.NoError(t, r.index(rag.IndexMessage{Doctype: consts.Files, DirID: inner.DocID, Action: rag.ActionRemove}))

	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "still claimed by the outer workspace")
	assert.Equal(t, []string{outer.DocID}, ff.Workspaces)
	assert.False(t, r.fake.HasWorkspace(inner.DocID))
	assert.True(t, r.fake.HasWorkspace(outer.DocID))
}

func TestRemoveFolderWithGlobalTrigger(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	global := rag.IndexMessage{Doctype: consts.Files}
	scoped := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(global)
	r.addTrigger(scoped)
	require.NoError(t, r.index(scoped))

	require.NoError(t, r.index(rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID, Action: rag.ActionRemove}))

	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "the global trigger still claims it")
	assert.Empty(t, ff.Workspaces)
}

func TestRemoveFolderGone(t *testing.T) {
	r := newRAGTest(t)
	// A folder destroyed after its trigger ran: openRAG still has the
	// workspace and the stack still has the checkpoint.
	r.fake.AddWorkspace("gone-dir")
	require.NoError(t, rag.SaveCheckpoint(r.inst, consts.Files, "rag-index-gone-dir", rag.Checkpoint{LastSeq: "3-abc"}))

	require.NoError(t, r.index(rag.IndexMessage{Doctype: consts.Files, DirID: "gone-dir", Action: rag.ActionRemove}))
	assert.False(t, r.fake.HasWorkspace("gone-dir"))
	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-gone-dir")
	require.NoError(t, err)
	assert.Equal(t, rag.Checkpoint{}, cp)
}

func TestRemoveRequiresDirID(t *testing.T) {
	r := newRAGTest(t)
	err := r.index(rag.IndexMessage{Doctype: consts.Files, Action: rag.ActionRemove})
	require.Error(t, err)
}
