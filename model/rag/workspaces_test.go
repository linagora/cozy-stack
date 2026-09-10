package rag_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileWorkspacesCreatesAndRemoves(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	gone := r.mkdir("/Gone")
	inGone := r.writeFile("/Gone/g.txt", "g")
	r.fake.AddWorkspace(gone.DocID)
	r.fake.AddWorkspace("orphan-of-destroyed-folder")
	r.fake.AddFile(inGone.DocID, "x", gone.DocID)
	r.addAssistant("KB assistant", kb.DocID)

	var pushed []string
	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, kb.DocID, func(dirID string) error {
		pushed = append(pushed, dirID)
		return nil
	}))

	assert.True(t, r.fake.HasWorkspace(kb.DocID), "missing workspace created")
	assert.Equal(t, []string{kb.DocID}, pushed, "reconcile job pushed for the new folder")
	assert.False(t, r.fake.HasWorkspace(gone.DocID), "workspace without assistant removed")
	assert.False(t, r.fake.HasWorkspace("orphan-of-destroyed-folder"), "unresolvable workspace removed too")
	_, ok := r.fake.File(inGone.DocID)
	assert.False(t, ok, "file of the removed workspace deleted (no membership left)")
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPost, "/partition/"+r.inst.Domain+"/workspaces"))
}

func TestReconcileWorkspacesRootDisplayName(t *testing.T) {
	r := newRAGTest(t)
	r.addAssistant("Everything", consts.RootDirID)
	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, consts.RootDirID, func(string) error { return nil }))
	assert.True(t, r.fake.HasWorkspace(rag.RootWorkspaceID))
	reqs := r.fake.Rec.All()
	var body string
	for _, req := range reqs {
		if req.Method == http.MethodPost && req.Path == "/partition/"+r.inst.Domain+"/workspaces" {
			body = string(req.Body)
		}
	}
	assert.Contains(t, body, `"display_name":"Drive"`)
	assert.Contains(t, body, `"workspace_id":"`+rag.RootWorkspaceID+`"`, "openRAG refuses an id with dots")
}

// TestReconcileWorkspacesRemovesStaleRootWorkspace checks the reverse
// mapping: the whole-Drive workspace is resolved back to the root folder,
// which no assistant uses any more, so it is removed with its files.
func TestReconcileWorkspacesRemovesStaleRootWorkspace(t *testing.T) {
	r := newRAGTest(t)
	doc := r.writeFile("/top.txt", "top")
	r.fake.AddWorkspace(rag.RootWorkspaceID)
	r.fake.AddFile(doc.DocID, "x", rag.RootWorkspaceID)

	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, "", func(string) error { return nil }))

	assert.False(t, r.fake.HasWorkspace(rag.RootWorkspaceID))
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok, "the live files of the whole Drive were detached, then deleted")
}

// TestReconcileWorkspacesPushesAfterCreation pins the order: the reconcile
// job is pushed only once its workspace exists on openRAG.
func TestReconcileWorkspacesPushesAfterCreation(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)

	var existedOnPush bool
	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, "", func(dirID string) error {
		existedOnPush = r.fake.HasWorkspace(kb.DocID)
		return nil
	}))
	assert.True(t, existedOnPush, "the workspace exists before its reconcile job is pushed")
}

// TestReconcileWorkspacesNoPushWhenCreationFails: nothing to index into, no
// job; the failure is logged, not returned, and the next run retries.
func TestReconcileWorkspacesNoPushWhenCreationFails(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)
	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/partition/"+r.inst.Domain+"/workspaces" {
			return http.StatusInternalServerError
		}
		return 0
	}

	var pushed []string
	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, "", func(dirID string) error {
		pushed = append(pushed, dirID)
		return nil
	}))
	assert.Empty(t, pushed)
	assert.False(t, r.fake.HasWorkspace(kb.DocID))
}

// TestReconcileWorkspacesRollsBackOnPushFailure: an empty workspace without
// its job would claim the folder is indexed, so it is deleted again.
func TestReconcileWorkspacesRollsBackOnPushFailure(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)

	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, "", func(string) error {
		return errors.New("the job queue is unreachable")
	}))
	assert.False(t, r.fake.HasWorkspace(kb.DocID))
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodDelete, "/partition/"+r.inst.Domain+"/workspaces/"+kb.DocID))

	// The next run retries both.
	var pushed []string
	require.NoError(t, rag.ReconcileWorkspacesForTest(r.inst, "", func(dirID string) error {
		pushed = append(pushed, dirID)
		return nil
	}))
	assert.True(t, r.fake.HasWorkspace(kb.DocID))
	assert.Equal(t, []string{kb.DocID}, pushed)
}
