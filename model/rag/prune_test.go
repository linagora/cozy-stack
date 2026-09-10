package rag_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrune(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	claimed := r.writeFile("/KB/a.txt", "alpha")
	unclaimed := r.writeFile("/Other/c.txt", "gamma")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	// Simulate leftovers: a file of a folder in no knowledge base, and a
	// file that no longer exists in the VFS.
	r.fake.AddFile(unclaimed.DocID, "x")
	r.fake.AddFile("ghost", "y")

	res, err := rag.Prune(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PruneResult{FilesScanned: 3, FilesDeleted: 2}, res)
	_, ok := r.fake.File(claimed.DocID)
	assert.True(t, ok)
	_, ok = r.fake.File(unclaimed.DocID)
	assert.False(t, ok)
	_, ok = r.fake.File("ghost")
	assert.False(t, ok)
}

func TestPruneWithRootAssistant(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/Other")
	doc := r.writeFile("/Other/c.txt", "gamma")
	r.addAssistant("Everything", consts.RootDirID)
	r.fake.AddWorkspace(rag.RootWorkspaceID)
	r.fake.AddFile(doc.DocID, "x", rag.RootWorkspaceID)
	r.fake.AddFile("ghost", "y")

	res, err := rag.Prune(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PruneResult{FilesScanned: 2, FilesDeleted: 1}, res)
	_, ok := r.fake.File(doc.DocID)
	assert.True(t, ok, "claimed by the root assistant")
	assert.True(t, r.fake.HasWorkspace(rag.RootWorkspaceID), "the whole-Drive workspace is the root folder's")
}

// TestPruneDropsStaleRootWorkspace: the whole-Drive workspace is resolved
// back to the root folder, which no assistant uses any more.
func TestPruneDropsStaleRootWorkspace(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	r.fake.AddWorkspace(rag.RootWorkspaceID)
	r.fake.AddFile(doc.DocID, "x", kb.DocID, rag.RootWorkspaceID)

	res, err := rag.Prune(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PruneResult{FilesScanned: 1, WorkspacesDeleted: 1}, res)
	assert.False(t, r.fake.HasWorkspace(rag.RootWorkspaceID))
	assert.True(t, r.fake.HasWorkspace(kb.DocID))
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "the file is still claimed by /KB: its membership was removed first")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

// TestPruneOrphanedFileIsDeleted covers the "not found" branch of the
// parent directory lookup: the directory doc is gone (deleted outside the
// VFS, bypassing trash), so the file is orphaned and pruned even though the
// root assistant would otherwise claim it.
func TestPruneOrphanedFileIsDeleted(t *testing.T) {
	r := newRAGTest(t)
	orphan := r.mkdir("/Orphan")
	doc := r.writeFile("/Orphan/o.txt", "epsilon")
	r.addAssistant("Everything", consts.RootDirID)
	r.fake.AddFile(doc.DocID, "x")

	// Delete the directory doc directly, bypassing VFS trash semantics, so
	// the file's dir_id now points at nothing.
	require.NoError(t, couchdb.DeleteDoc(r.inst, orphan))

	res, err := rag.Prune(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PruneResult{FilesScanned: 1, FilesDeleted: 1}, res)
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok, "the file's parent directory is gone: orphaned")
}

// TestPruneAbortsOnDirLookupError covers the "any other error" branch: a
// directory lookup failure that is not a not-found (here: an id starting
// with "_", rejected by couchdb before any HTTP round-trip, so the failure
// is deterministic) must abort the prune rather than delete the file.
func TestPruneAbortsOnDirLookupError(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("Everything", consts.RootDirID)
	r.fake.AddFile(doc.DocID, "x")

	raw, err := r.inst.VFS().FileByID(doc.DocID)
	require.NoError(t, err)
	raw.DirID = "_bogus"
	require.NoError(t, couchdb.UpdateDoc(r.inst, raw))

	_, err = rag.Prune(r.inst, rag.TestingLogger())
	require.Error(t, err)
	_, ok := r.fake.File(doc.DocID)
	assert.True(t, ok, "the prune aborted before touching openRAG")
}

func TestPruneDropsWorkspacesWithoutAssistant(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	// A workspace whose folder is in no knowledge base, still holding a file
	// that the live workspace also holds.
	r.fake.AddWorkspace("old-kb")
	r.fake.AddFile(inKB.DocID, "x", kb.DocID, "old-kb")

	res, err := rag.Prune(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PruneResult{FilesScanned: 1, WorkspacesDeleted: 1}, res)
	assert.False(t, r.fake.HasWorkspace("old-kb"))
	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the workspace of the live folder is kept")
	ff, ok := r.fake.File(inKB.DocID)
	require.True(t, ok, "the file is claimed by the live folder")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

func TestPruneKeepsClaimedFilesOfDroppedWorkspace(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/Docs")
	doc := r.writeFile("/Docs/a.txt", "alpha")
	r.addAssistant("Everything", consts.RootDirID)
	// The folder used to be a knowledge base: no assistant references it any
	// more, the workspace and the membership are still there.
	r.fake.AddWorkspace("old-kb")
	r.fake.AddFile(doc.DocID, "x", "old-kb")

	res, err := rag.Prune(r.inst, rag.TestingLogger())
	require.NoError(t, err)
	assert.Equal(t, rag.PruneResult{FilesScanned: 1, WorkspacesDeleted: 1}, res)
	assert.False(t, r.fake.HasWorkspace("old-kb"))
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "openRAG would have deleted it with the workspace: the membership was removed first")
	assert.Empty(t, ff.Workspaces)
}

func TestReset(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	lastSeq, _ := r.checkpoint()
	require.NotEmpty(t, lastSeq)

	require.NoError(t, rag.Reset(r.inst))

	lastSeq, _ = r.checkpoint()
	assert.Empty(t, lastSeq)

	jobs, err := job.GetAllJobs(r.inst)
	require.NoError(t, err)
	var ragJobs []*job.Job
	for _, j := range jobs {
		if j.WorkerType == "rag-index" {
			ragJobs = append(ragJobs, j)
		}
	}
	require.Len(t, ragJobs, 1, "a single rag-index job was pushed")
	assert.JSONEq(t, `{"doctype":"io.cozy.files"}`, string(ragJobs[0].Message))
	assert.True(t, ragJobs[0].Manual)
}

func TestReconcile(t *testing.T) {
	r := newRAGTest(t)
	dirA := r.mkdir("/A")
	dirB := r.mkdir("/B")
	r.addAssistant("A", dirA.DocID)
	r.addAssistant("B", dirB.DocID)

	n, err := rag.Reconcile(r.inst, dirA.DocID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	n, err = rag.Reconcile(r.inst, "")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "without dir_id every knowledge base folder is reconciled")

	_, err = rag.Reconcile(r.inst, dirB.DocID)
	require.NoError(t, err)

	_, err = rag.Reconcile(r.inst, "not-a-knowledge-base")
	assert.ErrorIs(t, err, rag.ErrUnknownFolder)
}
