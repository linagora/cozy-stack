package rag_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"slices"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	// The real worker lives in worker/rag, which model/rag cannot import.
	// Register a no-op worker of the same name so triggers and jobs can be
	// created in these tests.
	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "rag-index",
		Concurrency: 1,
		WorkerFunc:  func(*job.TaskContext) error { return nil },
	})
}

// ragTest wires a test instance to a fake openRAG.
type ragTest struct {
	t    *testing.T
	inst *instance.Instance
	fake *rag.FakeOpenRAG
	// reconciles records the reconcile jobs the worker pushed: the no-op
	// test worker runs a pushed job at once, so the queue cannot be read.
	reconciles []string
}

func newRAGTest(t *testing.T) *ragTest {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fake := rag.NewFakeOpenRAG(t)
	previous := config.GetConfig().RAGServers
	config.GetConfig().RAGServers = map[string]config.RAGServer{config.DefaultInstanceContext: fake.Server}
	t.Cleanup(func() { config.GetConfig().RAGServers = previous })
	r := &ragTest{t: t, inst: inst, fake: fake}
	t.Cleanup(rag.SetReconcilePushForTest(func(_ *instance.Instance, dirID string) error {
		r.reconciles = append(r.reconciles, dirID)
		return nil
	}))
	return r
}

func (r *ragTest) mkdir(path string) *vfs.DirDoc {
	r.t.Helper()
	dir, err := vfs.MkdirAll(r.inst.VFS(), path)
	require.NoError(r.t, err)
	return dir
}

func (r *ragTest) writeFile(path, content string) *vfs.FileDoc {
	r.t.Helper()
	f, err := vfs.Create(r.inst.VFS(), path)
	require.NoError(r.t, err)
	_, err = f.Write([]byte(content))
	require.NoError(r.t, err)
	require.NoError(r.t, f.Close())
	doc, err := r.inst.VFS().FileByPath(path)
	require.NoError(r.t, err)
	return doc
}

// rewriteFile replaces the content of an existing file, the way a re-upload
// from the client does: a new revision, a new md5sum, the same doc id.
func (r *ragTest) rewriteFile(path, content string) *vfs.FileDoc {
	r.t.Helper()
	f, err := vfs.OpenFile(r.inst.VFS(), path, os.O_WRONLY, 0)
	require.NoError(r.t, err)
	_, err = f.Write([]byte(content))
	require.NoError(r.t, err)
	require.NoError(r.t, f.Close())
	doc, err := r.inst.VFS().FileByPath(path)
	require.NoError(r.t, err)
	return doc
}

func (r *ragTest) index() error {
	return rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files})
}

// drainReconcileJobs returns the reconcile jobs pushed so far, and forgets
// them.
func (r *ragTest) drainReconcileJobs() []string {
	pushed := r.reconciles
	r.reconciles = nil
	return pushed
}

// runUntilSettled runs the job, then any reconcile job it pushed, until no
// job is left: the no-op test worker does not execute pushed jobs.
func (r *ragTest) runUntilSettled() {
	r.t.Helper()
	require.NoError(r.t, r.index())
	for _, dirID := range r.drainReconcileJobs() {
		require.NoError(r.t, rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files, ReconcileDirID: dirID}))
	}
	assert.Empty(r.t, r.reconciles, "a reconcile job must not push reconcile jobs")
}

// checkpoint reads the checkpoint as the worker stores it, in a CouchDB
// local document. Both values are zero when there is none.
func (r *ragTest) checkpoint() (lastSeq string, retries int) {
	r.t.Helper()
	doc, err := couchdb.GetLocal(r.inst, consts.Files, "rag-index")
	if couchdb.IsNotFoundError(err) {
		return "", 0
	}
	require.NoError(r.t, err)
	lastSeq, _ = doc["last_seq"].(string)
	if n, ok := doc["retries"].(float64); ok {
		retries = int(n)
	}
	return lastSeq, retries
}

func (r *ragTest) seedCheckpoint(lastSeq string, retries int) {
	r.t.Helper()
	doc := map[string]interface{}{"last_seq": lastSeq, "retries": retries}
	require.NoError(r.t, couchdb.PutLocal(r.inst, consts.Files, "rag-index", doc))
}

// addAssistant creates an assistant whose knowledge base is the folder.
func (r *ragTest) addAssistant(name, dirID string) string {
	r.t.Helper()
	doc := couchdb.JSONDoc{Type: consts.ChatAssistants, M: map[string]interface{}{
		"name": name,
		"knowledgeBase": []map[string]interface{}{
			{"doctype": consts.Files, "dirId": dirID},
		},
	}}
	require.NoError(r.t, couchdb.CreateDoc(r.inst, &doc))
	return doc.ID()
}

// setAssistantFolder replaces the knowledge base folder of an assistant
// (dirID empty: no folder at all).
func (r *ragTest) setAssistantFolder(assistantID, dirID string) {
	r.t.Helper()
	var doc couchdb.JSONDoc
	require.NoError(r.t, couchdb.GetDoc(r.inst, consts.ChatAssistants, assistantID, &doc))
	doc.Type = consts.ChatAssistants
	if dirID == "" {
		doc.M["knowledgeBase"] = []map[string]interface{}{}
	} else {
		doc.M["knowledgeBase"] = []map[string]interface{}{{"doctype": consts.Files, "dirId": dirID}}
	}
	require.NoError(r.t, couchdb.UpdateDoc(r.inst, &doc))
}

func (r *ragTest) deleteAssistant(assistantID string) {
	r.t.Helper()
	var doc couchdb.JSONDoc
	require.NoError(r.t, couchdb.GetDoc(r.inst, consts.ChatAssistants, assistantID, &doc))
	doc.Type = consts.ChatAssistants
	require.NoError(r.t, couchdb.DeleteDoc(r.inst, &doc))
}

func (r *ragTest) uploads(doc *vfs.FileDoc) int {
	return r.fake.Rec.Count(http.MethodPost, "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID) +
		r.fake.Rec.Count(http.MethodPut, "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID)
}

// workspaceIDs is the sorted list openRAG receives as workspace_ids: the
// folder ids are sorted, then mapped to workspace ids.
func workspaceIDs(dirIDs ...string) []string {
	slices.Sort(dirIDs)
	ids := make([]string, len(dirIDs))
	for i, dirID := range dirIDs {
		if dirID == consts.RootDirID {
			ids[i] = rag.RootWorkspaceID
		} else {
			ids[i] = dirID
		}
	}
	return ids
}

func TestIndexAssistantFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	sub := r.mkdir("/KB/sub")
	r.mkdir("/Other")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	inSub := r.writeFile("/KB/sub/b.txt", "beta")
	outside := r.writeFile("/Other/c.txt", "gamma")
	r.addAssistant("KB assistant", kb.DocID)

	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the workspace of the knowledge base folder is created")
	assert.False(t, r.fake.HasWorkspace(sub.DocID))
	assert.ElementsMatch(t, []string{inKB.DocID, inSub.DocID}, r.fake.FileIDs())
	ff, _ := r.fake.File(inKB.DocID)
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
	assert.Equal(t, hex.EncodeToString(inKB.MD5Sum), ff.MD5)
	_, ok := r.fake.File(outside.DocID)
	assert.False(t, ok, "out-of-scope files never reach openRAG")

	lastSeq, _ := r.checkpoint()
	assert.NotEmpty(t, lastSeq)

	// Second run: nothing changed, no upload.
	uploads := r.uploads(inKB)
	r.runUntilSettled()
	assert.Equal(t, uploads, r.uploads(inKB))
}

func TestIndexRootAssistant(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/Sub")
	top := r.writeFile("/top.txt", "top")
	inSub := r.writeFile("/Sub/s.txt", "sub")
	r.addAssistant("Everything", consts.RootDirID)

	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(rag.RootWorkspaceID), "the root folder id is not a valid workspace id")
	assert.False(t, r.fake.HasWorkspace(consts.RootDirID))
	assert.ElementsMatch(t, []string{top.DocID, inSub.DocID}, r.fake.FileIDs())
	ff, _ := r.fake.File(inSub.DocID)
	assert.Equal(t, []string{rag.RootWorkspaceID}, ff.Workspaces)

	var body string
	for _, req := range r.fake.Rec.All() {
		if req.Method == http.MethodPost && req.Path == "/partition/"+r.inst.Domain+"/workspaces" {
			body = string(req.Body)
		}
	}
	assert.Contains(t, body, `"display_name":"Drive"`)
	assert.Contains(t, body, `"workspace_id":"`+rag.RootWorkspaceID+`"`)
}

// TestIndexRootAndFolderAssistants covers a file claimed by both the root
// assistant and a folder one: the upload carries both workspace ids.
func TestIndexRootAndFolderAssistants(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	outside := r.writeFile("/top.txt", "top")
	r.addAssistant("Everything", consts.RootDirID)
	r.addAssistant("KB assistant", kb.DocID)

	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(rag.RootWorkspaceID))
	assert.True(t, r.fake.HasWorkspace(kb.DocID))
	ff, ok := r.fake.File(inKB.DocID)
	require.True(t, ok)
	// The folder ids are sorted, then mapped: a hexadecimal id sorts before
	// the root workspace id.
	assert.Equal(t, []string{kb.DocID, rag.RootWorkspaceID}, ff.Workspaces)
	assert.Equal(t, workspaceIDs(kb.DocID, consts.RootDirID), ff.Workspaces)
	assert.Equal(t, 1, r.uploads(inKB), "uploaded once, with both memberships")
	ff, ok = r.fake.File(outside.DocID)
	require.True(t, ok)
	assert.Equal(t, []string{rag.RootWorkspaceID}, ff.Workspaces)
}

// TestIndexSharedDrivesAssistant covers the other well-known folder ids: a
// user can pick "Shared drives" as a knowledge base, and its id has dots
// too, so openRAG would refuse it as a workspace id.
func TestIndexSharedDrivesAssistant(t *testing.T) {
	r := newRAGTest(t)
	drives, err := r.inst.EnsureSharedDrivesDir()
	require.NoError(t, err)
	require.Equal(t, consts.SharedDrivesDirID, drives.DocID)
	doc := r.writeFile(drives.Fullpath+"/d.txt", "drive")
	r.addAssistant("Shared drives", consts.SharedDrivesDirID)

	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace("io-cozy-files-shared-drives-dir"))
	assert.False(t, r.fake.HasWorkspace(consts.SharedDrivesDirID))
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.Equal(t, []string{"io-cozy-files-shared-drives-dir"}, ff.Workspaces)
}

// TestIndexRootAssistantDeleted checks that the whole-Drive workspace is
// removed, and its files with it, when the last root assistant is gone.
func TestIndexRootAssistantDeleted(t *testing.T) {
	r := newRAGTest(t)
	doc := r.writeFile("/top.txt", "top")
	assistant := r.addAssistant("Everything", consts.RootDirID)
	r.runUntilSettled()
	require.True(t, r.fake.HasWorkspace(rag.RootWorkspaceID))

	r.deleteAssistant(assistant)
	r.runUntilSettled()

	assert.False(t, r.fake.HasWorkspace(rag.RootWorkspaceID))
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok, "its files were detached and deleted")
}

// TestIndexStaleRootWorkspaceRemoved covers the removal side of the mapping:
// a leftover whole-Drive workspace, with no root assistant any more, must be
// resolved back to the root folder and removed.
func TestIndexStaleRootWorkspaceRemoved(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.fake.AddWorkspace(rag.RootWorkspaceID)
	r.fake.AddFile(inKB.DocID, "x", rag.RootWorkspaceID)

	r.runUntilSettled()

	assert.False(t, r.fake.HasWorkspace(rag.RootWorkspaceID), "no assistant indexes the whole Drive any more")
	assert.True(t, r.fake.HasWorkspace(kb.DocID))
	ff, ok := r.fake.File(inKB.DocID)
	require.True(t, ok, "the file is still claimed by /KB")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

// TestIndexNoReconcileJobWhenWorkspaceCreationFails is the ordering rule:
// nothing is walked into a workspace that does not exist. The job itself
// succeeds, and the next run retries the creation.
func TestIndexNoReconcileJobWhenWorkspaceCreationFails(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)

	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/partition/"+r.inst.Domain+"/workspaces" {
			return http.StatusInternalServerError
		}
		return 0
	}
	require.NoError(t, r.index(), "a creation failure is logged, not raised")
	assert.False(t, r.fake.HasWorkspace(kb.DocID))
	assert.Empty(t, r.reconciles, "no reconcile job without a workspace to index into")

	r.fake.Fail = nil
	r.runUntilSettled()
	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the next run retries the creation")
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "and pushes the reconcile job that indexes the subtree")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

// TestIndexWorkspaceRolledBackWhenPushFails is the other half: a workspace
// whose reconcile job could not be pushed is deleted again. Left there, it
// would claim the folder is indexed (checkWorkspace succeeds) while nothing
// walks its subtree any more.
func TestIndexWorkspaceRolledBackWhenPushFails(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)

	restore := rag.SetReconcilePushForTest(func(*instance.Instance, string) error {
		return errors.New("the job queue is unreachable")
	})
	require.NoError(t, r.index(), "a push failure is logged, not raised")
	restore()
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPost, "/partition/"+r.inst.Domain+"/workspaces"), "the workspace was created first")
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodDelete, "/partition/"+r.inst.Domain+"/workspaces/"+kb.DocID), "then rolled back")
	assert.False(t, r.fake.HasWorkspace(kb.DocID))

	r.runUntilSettled()
	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the next run creates it and pushes the job")
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

func TestIndexNestedAssistants(t *testing.T) {
	r := newRAGTest(t)
	outer := r.mkdir("/Outer")
	inner := r.mkdir("/Outer/Inner")
	doc := r.writeFile("/Outer/Inner/x.txt", "x")
	r.addAssistant("Outer", outer.DocID)
	r.addAssistant("Inner", inner.DocID)

	r.runUntilSettled()

	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.Equal(t, workspaceIDs(outer.DocID, inner.DocID), ff.Workspaces)
	assert.Equal(t, 1, r.uploads(doc), "uploaded once, with both memberships")
}

func TestIndexNoAssistant(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	// Leftovers of a knowledge base that is not one any more.
	r.fake.AddWorkspace(kb.DocID)
	r.fake.AddFile(doc.DocID, hex.EncodeToString(doc.MD5Sum), kb.DocID)

	r.runUntilSettled()

	assert.False(t, r.fake.HasWorkspace(kb.DocID), "a workspace without assistant is removed")
	assert.Empty(t, r.fake.FileIDs(), "its files belong to no knowledge base any more")
}

func TestIndexAssistantCreatedOnFullFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/KB/sub")
	a := r.writeFile("/KB/a.txt", "alpha")
	b := r.writeFile("/KB/sub/b.txt", "beta")

	// A first run without any assistant: the checkpoint moves past the files.
	r.runUntilSettled()
	require.Empty(t, r.fake.FileIDs())

	// Creating the assistant changes no file document: the subtree can only
	// be indexed by the reconcile job the workspace diff pushes.
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(kb.DocID))
	assert.ElementsMatch(t, []string{a.DocID, b.DocID}, r.fake.FileIDs())
	ff, _ := r.fake.File(b.DocID)
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

func TestIndexAssistantFolderChanged(t *testing.T) {
	r := newRAGTest(t)
	dirA := r.mkdir("/A")
	dirB := r.mkdir("/B")
	inA := r.writeFile("/A/a.txt", "alpha")
	inB := r.writeFile("/B/b.txt", "beta")
	assistant := r.addAssistant("KB assistant", dirA.DocID)
	r.runUntilSettled()
	require.Equal(t, []string{inA.DocID}, r.fake.FileIDs())

	r.setAssistantFolder(assistant, dirB.DocID)
	r.runUntilSettled()

	assert.False(t, r.fake.HasWorkspace(dirA.DocID), "the workspace of the old folder is gone")
	_, ok := r.fake.File(inA.DocID)
	assert.False(t, ok, "its files were detached and deleted")
	assert.True(t, r.fake.HasWorkspace(dirB.DocID))
	ff, ok := r.fake.File(inB.DocID)
	require.True(t, ok, "the new folder is indexed")
	assert.Equal(t, []string{dirB.DocID}, ff.Workspaces)
}

func TestIndexAssistantDeleted(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	assistant := r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	require.True(t, r.fake.HasWorkspace(kb.DocID))

	r.deleteAssistant(assistant)
	r.runUntilSettled()

	assert.False(t, r.fake.HasWorkspace(kb.DocID))
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok)
	var status rag.IndexStatus
	err := couchdb.GetDoc(r.inst, consts.ChatRAG, doc.DocID, &status)
	assert.True(t, couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err), "its index status is gone too")
}

func TestIndexSharedFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	first := r.addAssistant("First", kb.DocID)
	r.addAssistant("Second", kb.DocID)
	r.runUntilSettled()
	require.True(t, r.fake.HasWorkspace(kb.DocID))

	r.deleteAssistant(first)
	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the second assistant still uses the folder")
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

func TestIndexMoveOutOfScope(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	other := r.mkdir("/Other")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	_, ok := r.fake.File(doc.DocID)
	require.True(t, ok)

	_, err := vfs.ModifyFileMetadata(r.inst.VFS(), doc, &vfs.DocPatch{DirID: &other.DocID})
	require.NoError(t, err)
	r.runUntilSettled()

	_, ok = r.fake.File(doc.DocID)
	assert.False(t, ok, "in no knowledge base any more: deleted")
	var status rag.IndexStatus
	err = couchdb.GetDoc(r.inst, consts.ChatRAG, doc.DocID, &status)
	assert.True(t, couchdb.IsNotFoundError(err), "its index status is gone too")
}

func TestIndexMoveBetweenFolders(t *testing.T) {
	r := newRAGTest(t)
	dirA := r.mkdir("/A")
	dirB := r.mkdir("/B")
	doc := r.writeFile("/A/a.txt", "alpha")
	r.addAssistant("A", dirA.DocID)
	r.addAssistant("B", dirB.DocID)
	r.runUntilSettled()
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	require.Equal(t, []string{dirA.DocID}, ff.Workspaces)

	_, err := vfs.ModifyFileMetadata(r.inst.VFS(), doc, &vfs.DocPatch{DirID: &dirB.DocID})
	require.NoError(t, err)
	r.runUntilSettled()

	ff, ok = r.fake.File(doc.DocID)
	require.True(t, ok, "the file stays on openRAG: no delete/re-upload cycle")
	assert.Equal(t, []string{dirB.DocID}, ff.Workspaces)
	assert.Equal(t, 1, r.uploads(doc), "the memberships were diffed, not the content re-sent")
}

func TestIndexNewFileOutOfScopeSkipsOpenRAG(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	doc := r.writeFile("/Other/c.txt", "gamma")
	r.runUntilSettled()

	assert.Equal(t, "1-", doc.DocRev[:2])
	assert.Equal(t, 0, r.uploads(doc))
	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodGet, "/partition/"+r.inst.Domain+"/files/"+doc.DocID+"/workspaces"), "a rev 1- file out of scope costs no openRAG call")
}

func TestIndexSubtreeMove(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	moving := r.mkdir("/Moving")
	doc := r.writeFile("/Moving/d.txt", "delta")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	_, ok := r.fake.File(doc.DocID)
	require.False(t, ok)

	// Drag /Moving into /KB: only the dir doc changes.
	_, err := vfs.ModifyDirMetadata(r.inst.VFS(), moving, &vfs.DocPatch{DirID: &kb.DocID})
	require.NoError(t, err)
	r.runUntilSettled()
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "files of a subtree dragged into a knowledge base are indexed")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)

	// Drag it back out.
	moved, err := r.inst.VFS().DirByID(moving.DocID)
	require.NoError(t, err)
	root := consts.RootDirID
	_, err = vfs.ModifyDirMetadata(r.inst.VFS(), moved, &vfs.DocPatch{DirID: &root})
	require.NoError(t, err)
	r.runUntilSettled()
	_, ok = r.fake.File(doc.DocID)
	assert.False(t, ok, "files of a subtree dragged out are deleted")
}

func TestIndexTrashAndRestoreKBFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	trashed, err := vfs.TrashDir(r.inst.VFS(), kb)
	require.NoError(t, err)
	r.runUntilSettled()
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok, "a trashed knowledge base folder contains nothing")
	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the workspace is kept while an assistant references it")

	_, err = vfs.RestoreDir(r.inst.VFS(), trashed)
	require.NoError(t, err)
	r.runUntilSettled()
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "restored: indexed again")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

func TestIndexRenameKBFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	require.Equal(t, []string{kb.DocID}, ff.Workspaces)
	uploads := r.uploads(doc)

	// The scope follows the folder by id, not by path.
	name := "Knowledge"
	_, err := vfs.ModifyDirMetadata(r.inst.VFS(), kb, &vfs.DocPatch{Name: &name})
	require.NoError(t, err)
	r.runUntilSettled()

	ff, ok = r.fake.File(doc.DocID)
	require.True(t, ok, "a renamed knowledge base folder still claims its files")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
	assert.Equal(t, uploads, r.uploads(doc), "a rename is not a new content")
}

func TestIndexReuploadKeepsMemberships(t *testing.T) {
	r := newRAGTest(t)
	outer := r.mkdir("/Outer")
	inner := r.mkdir("/Outer/Inner")
	doc := r.writeFile("/Outer/Inner/x.txt", "x")
	r.addAssistant("Outer", outer.DocID)
	r.addAssistant("Inner", inner.DocID)
	r.runUntilSettled()
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	require.Equal(t, workspaceIDs(outer.DocID, inner.DocID), ff.Workspaces)

	// A new content: openRAG replaces the membership list on every upload,
	// so the upload has to carry every desired workspace.
	r.rewriteFile("/Outer/Inner/x.txt", "x and more")
	r.runUntilSettled()

	assert.Equal(t, 2, r.uploads(doc), "the new content was sent")
	ff, ok = r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.Equal(t, workspaceIDs(outer.DocID, inner.DocID), ff.Workspaces)
}

func TestIndexDeletedFileIsRemoved(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	_, err := vfs.TrashFile(r.inst.VFS(), doc)
	require.NoError(t, err)
	r.runUntilSettled()
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok)
}

func TestIndexNotSupportedClassStatus(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	img := r.writeFile("/KB/pic.jpg", "not really a jpeg")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()

	assert.Equal(t, 0, r.uploads(img), "images are not indexed without the flag")
	var status rag.IndexStatus
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatRAG, img.DocID, &status))
	assert.Equal(t, rag.StatusNotSupported, status.Status)
}

func TestIndexKeepsCheckpointOnRetryableError(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)

	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID {
			return 503
		}
		return 0
	}
	err := r.index()
	require.Error(t, err)
	lastSeq, retries := r.checkpoint()
	assert.Empty(t, lastSeq, "checkpoint not advanced")
	assert.Equal(t, 1, retries, "the retryable error counts as an attempt")

	r.fake.Fail = nil
	require.NoError(t, r.index())
	_, ok := r.fake.File(doc.DocID)
	assert.True(t, ok)
	lastSeq, retries = r.checkpoint()
	assert.NotEmpty(t, lastSeq)
	assert.Equal(t, 0, retries)
}

func TestIndexGivesUpAfterMaxRetries(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.seedCheckpoint("", rag.MaxBatchRetries)

	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID {
			return 503
		}
		return 0
	}
	err := r.index()
	require.Error(t, err)
	lastSeq, retries := r.checkpoint()
	assert.NotEmpty(t, lastSeq, "past the cap the batch advances anyway")
	assert.Equal(t, 0, retries)
}

func TestIndexNonRetryableErrorIsSkipped(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	bad := r.writeFile("/KB/bad.txt", "bad")
	good := r.writeFile("/KB/good.txt", "good")
	r.addAssistant("KB assistant", kb.DocID)
	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+bad.DocID {
			return 415
		}
		return 0
	}

	// A 4xx is logged and skipped: the job itself succeeds, replaying it
	// would only hit the same 4xx again.
	require.NoError(t, r.index())
	_, ok := r.fake.File(good.DocID)
	assert.True(t, ok, "the batch continued past the 4xx")
	lastSeq, _ := r.checkpoint()
	assert.NotEmpty(t, lastSeq, "4xx does not hold the checkpoint")
}

// TestReconcileJobStaysOnItsOwnFolder covers the race where a knowledge base
// folder appears while a reconcile job is queued: the reconcile job must not
// create the other folder's workspace. It pushes no reconcile job, and the
// files of that folder did not change, so the feed job that follows would
// see nothing to do while the workspace claims the folder is indexed.
func TestReconcileJobStaysOnItsOwnFolder(t *testing.T) {
	r := newRAGTest(t)
	dirA := r.mkdir("/A")
	dirB := r.mkdir("/B")
	inA := r.writeFile("/A/a.txt", "alpha")
	inB := r.writeFile("/B/b.txt", "beta")
	r.addAssistant("A", dirA.DocID)
	r.addAssistant("B", dirB.DocID)

	// The reconcile job of /A, running before any feed job.
	require.NoError(t, rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{
		Doctype: consts.Files, ReconcileDirID: dirA.DocID,
	}))

	assert.True(t, r.fake.HasWorkspace(dirA.DocID), "its own workspace is created")
	assert.False(t, r.fake.HasWorkspace(dirB.DocID), "another folder's workspace is left to a feed job")
	assert.Empty(t, r.reconciles, "a reconcile job pushes no reconcile job")
	ff, ok := r.fake.File(inA.DocID)
	require.True(t, ok, "its own subtree is indexed")
	assert.Equal(t, []string{dirA.DocID}, ff.Workspaces)
	_, ok = r.fake.File(inB.DocID)
	assert.False(t, ok, "the other folder is not indexed by this job")

	// The feed job that follows creates /B's workspace and reconciles it.
	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(dirB.DocID))
	ff, ok = r.fake.File(inB.DocID)
	require.True(t, ok, "the feed job pushed the reconcile job of /B")
	assert.Equal(t, []string{dirB.DocID}, ff.Workspaces)
}

// TestReconcileJobDoesNotRemoveWorkspaces checks the other half of the
// invariant: removals are the feed job's business, a reconcile job is
// bounded to its own folder.
func TestReconcileJobDoesNotRemoveWorkspaces(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	// A workspace of a folder that is in no knowledge base any more.
	r.fake.AddWorkspace("stale")
	r.fake.AddFile("ghost", "x", "stale")

	require.NoError(t, rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{
		Doctype: consts.Files, ReconcileDirID: kb.DocID,
	}))

	assert.True(t, r.fake.HasWorkspace("stale"), "a reconcile job removes no workspace")
	_, ok := r.fake.File("ghost")
	assert.True(t, ok, "and detaches nothing")

	r.runUntilSettled()
	assert.False(t, r.fake.HasWorkspace("stale"), "the feed job removes it")
}

// TestReconcileSkipsFileRefusedByIndexer is the resilience rule of a walk:
// one file the indexer refuses for good must not fail the whole reconcile
// job, or every retry replays the same walk to the same refusal.
func TestReconcileSkipsFileRefusedByIndexer(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	bad := r.writeFile("/KB/bad.txt", "bad")
	good := r.writeFile("/KB/good.txt", "good")
	r.addAssistant("KB assistant", kb.DocID)
	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+bad.DocID {
			return http.StatusUnsupportedMediaType
		}
		return 0
	}

	require.NoError(t, rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{
		Doctype: consts.Files, ReconcileDirID: kb.DocID,
	}), "a 4xx on one file does not fail the job")

	_, ok := r.fake.File(good.DocID)
	assert.True(t, ok, "the walk continued past the refused file")
	_, ok = r.fake.File(bad.DocID)
	assert.False(t, ok, "the refused file is not indexed")

	// The workspace exists now, so no reconcile job is pushed any more: the
	// refused file stays unindexed until it changes, or until an operator
	// runs `cozy-stack rag reconcile --dir-id`.
	require.NoError(t, r.index())
	assert.Empty(t, r.reconciles, "the folder is not walked again")
	_, ok = r.fake.File(bad.DocID)
	assert.False(t, ok)
}

// TestReconcileRetryableFileErrorFailsTheJob is the other half of the rule:
// a transient failure on a file must fail the job, so that the worker walks
// the folder again. The walk still covers the rest of the subtree.
func TestReconcileRetryableFileErrorFailsTheJob(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	other := r.writeFile("/KB/b.txt", "beta")
	r.addAssistant("KB assistant", kb.DocID)
	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID {
			return http.StatusInternalServerError
		}
		return 0
	}

	err := rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{
		Doctype: consts.Files, ReconcileDirID: kb.DocID,
	})

	require.Error(t, err)
	assert.True(t, rag.IsRetryableForTest(err), "a 5xx on one file asks for another walk")
	_, ok := r.fake.File(other.DocID)
	assert.True(t, ok, "the walk went on past the transient failure")
	_, ok = r.fake.File(doc.DocID)
	assert.False(t, ok)
}

// TestIndexPendingFileIsReuploadedWithPut covers the asynchronous indexing of
// openRAG: between the POST and the end of the indexing task, the file is
// unknown to a GET but a second POST conflicts. The upload must fall back to
// a PUT instead of failing the job.
func TestIndexPendingFileIsReuploadedWithPut(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	// Its indexing task is still running on openRAG: GET 404, POST 409.
	r.fake.AddPendingFile(doc.DocID)

	r.runUntilSettled()

	path := "/indexer/partition/" + r.inst.Domain + "/file/" + doc.DocID
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPost, path), "one POST, refused with a 409")
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPut, path), "then one PUT, accepted")
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.Equal(t, hex.EncodeToString(doc.MD5Sum), ff.MD5, "the content was sent again")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces, "the memberships survived the PUT")
}

// TestIndexDuplicateContentIsSkipped covers openRAG's content deduplication:
// a partition holds one document per distinct content, so a file whose
// content is already indexed under another id is refused with a 409
// DOCUMENT_CONTENT_EXISTS. It is skipped for good — no PUT fallback, no
// failure of the batch.
func TestIndexDuplicateContentIsSkipped(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)
	// The workspace now exists: the files below are indexed by the batch
	// alone, without a reconcile job walking them a second time.
	r.runUntilSettled()

	// Two batches, so that the file openRAG holds the content of is the
	// first one whatever order the changes feed gives them in.
	first := r.writeFile("/KB/a.txt", "the very same content")
	require.NoError(t, r.index())
	require.Equal(t, []string{first.DocID}, r.fake.FileIDs())

	dup := r.writeFile("/KB/b.txt", "the very same content")
	require.NoError(t, r.index(), "a content duplicate does not fail the batch")
	assert.Empty(t, r.reconciles)

	assert.Equal(t, []string{first.DocID}, r.fake.FileIDs(), "the duplicate is not indexed")
	path := "/indexer/partition/" + r.inst.Domain + "/file/" + dup.DocID
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPost, path), "one POST, refused with a 409")
	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodPut, path), "and no PUT: a PUT would answer 404")

	lastSeq, retries := r.checkpoint()
	assert.NotEmpty(t, lastSeq, "the checkpoint advances")
	assert.Zero(t, retries)
}

// TestReconcileSkipsDuplicateContent is the same rule on the walk of a
// knowledge base folder: the duplicate is skipped, the other files are
// indexed and the job succeeds.
func TestReconcileSkipsDuplicateContent(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	// The walk visits the children in the order of the dir-children index
	// (dir_id, _id), which says nothing about their names or their creation
	// order: which of the two copies openRAG keeps is not for this test to
	// decide, only that exactly one of them is kept.
	a := r.writeFile("/KB/a.txt", "the very same content")
	b := r.writeFile("/KB/b.txt", "the very same content")
	other := r.writeFile("/KB/c.txt", "something else")
	r.addAssistant("KB assistant", kb.DocID)

	err := rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{
		Doctype: consts.Files, ReconcileDirID: kb.DocID,
	})
	require.NoError(t, err, "a content duplicate does not fail the reconcile job")

	indexed := r.fake.FileIDs()
	require.Len(t, indexed, 2, "the file with its own content, and one of the two copies")
	assert.Contains(t, indexed, other.DocID)
	dup := b
	if !slices.Contains(indexed, a.DocID) {
		dup = a
	}
	assert.NotContains(t, indexed, dup.DocID, "the second copy of the content is not indexed")
	path := "/indexer/partition/" + r.inst.Domain + "/file/" + dup.DocID
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPost, path), "one POST, refused with a 409")
	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodPut, path))

	// The workspace exists now: the following feed job pushes no reconcile
	// job. It carries the three files, so openRAG is offered the duplicate
	// once more (and refuses it once more, at the cost of one upload); it is
	// never asked for a PUT, and the batch still succeeds.
	r.runUntilSettled()
	assert.Empty(t, r.reconciles)
	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodPut, path))
	assert.ElementsMatch(t, indexed, r.fake.FileIDs())
}

// TestReconcileSkipsFileWithMissingContent covers a file whose content
// vanished from local storage (observed on the test instance: "open
// …/TestDocs/test-docs.md: no such file or directory"). resolveContent then
// gets os.ErrNotExist from the VFS: that content will never come back, so
// the file must be skipped like an indexer refusal, not retried like a
// transient read error.
func TestReconcileSkipsFileWithMissingContent(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	broken := r.writeFile("/KB/broken.txt", "will vanish")
	good := r.writeFile("/KB/good.txt", "good")
	r.addAssistant("KB assistant", kb.DocID)
	// Remove the content from the instance's storage but keep the file
	// document: exactly what a lost storage write leaves behind.
	require.NoError(t, vfsafero.GetMemFS(r.inst.Domain).Remove("/KB/broken.txt"))

	err := rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{
		Doctype: consts.Files, ReconcileDirID: kb.DocID,
	})

	require.NoError(t, err, "content missing on disk is a definitive skip, not a retry")
	_, ok := r.fake.File(good.DocID)
	assert.True(t, ok, "the walk continued past the file with missing content")
	_, ok = r.fake.File(broken.DocID)
	assert.False(t, ok, "the file with missing content is not indexed")
	path := "/indexer/partition/" + r.inst.Domain + "/file/" + broken.DocID
	assert.Zero(t, r.fake.Rec.Count(http.MethodPost, path), "content that cannot be read is never uploaded")
	assert.Zero(t, r.fake.Rec.Count(http.MethodPut, path))
}

// TestIndexWorkspaceListingFailureIsRetryable covers the "openRAG workspace
// listing fails" edge case: the diff must not run on a partial view, so the
// job fails with nothing indexed, nothing detached and the checkpoint held.
// Every status but 404 (no partition yet) qualifies, a 4xx as much as a 5xx:
// an unreadable listing is never "the partition holds no workspace".
func TestIndexWorkspaceListingFailureIsRetryable(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			r := newRAGTest(t)
			kb := r.mkdir("/KB")
			doc := r.writeFile("/KB/a.txt", "alpha")
			r.addAssistant("KB assistant", kb.DocID)
			// A workspace and a file a run on a partial view would wrongly delete.
			r.fake.AddWorkspace("old-kb")
			r.fake.AddFile("ghost", "x", "old-kb")

			r.fake.Fail = func(method, path string) int {
				if method == http.MethodGet && path == "/partition/"+r.inst.Domain+"/workspaces" {
					return status
				}
				return 0
			}
			err := r.index()
			require.Error(t, err)

			lastSeq, _ := r.checkpoint()
			assert.Empty(t, lastSeq, "the checkpoint is held")
			assert.Equal(t, []string{"ghost"}, r.fake.FileIDs(), "nothing indexed, nothing deleted")
			assert.True(t, r.fake.HasWorkspace("old-kb"), "no workspace removed on a partial view")
			assert.False(t, r.fake.HasWorkspace(kb.DocID), "no workspace created either")
			assert.Equal(t, 0, r.uploads(doc))
		})
	}
}

// TestIndexWorkspaceListingNotFoundIsEmpty covers the cold start: openRAG
// has no partition for the instance yet and answers 404 on the workspace
// listing. That is "no workspace", not a failure: the diff creates them,
// the partition along the way, and the batch indexes.
func TestIndexWorkspaceListingNotFoundIsEmpty(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	// The fake knows no partition; a real openRAG answers 404 on the listing
	// until the partition is created with the first workspace.
	r.fake.Fail = func(method, path string) int {
		if method == http.MethodGet && path == "/partition/"+r.inst.Domain+"/workspaces" {
			return http.StatusNotFound
		}
		return 0
	}

	r.runUntilSettled()

	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the workspace is created on a cold partition")
	assert.Equal(t, 1, r.fake.Rec.Count(http.MethodPost, "/partition/"+r.inst.Domain), "the partition was created")
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "and the file indexed")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
	lastSeq, _ := r.checkpoint()
	assert.NotEmpty(t, lastSeq)
}

// TestIndexScopeLoadingFailureIsRetryable covers the "the scopes cannot be
// read" edge case: the job fails without touching the checkpoint nor
// openRAG, so a later run indexes on a complete view of the scopes.
func TestIndexScopeLoadingFailureIsRetryable(t *testing.T) {
	r := newRAGTest(t)
	r.mkdir("/KB")
	r.writeFile("/KB/a.txt", "alpha")
	// An id starting with "_" is rejected by couchdb before any HTTP
	// round-trip: the lookup fails deterministically, and it is not a
	// not-found.
	r.addAssistant("Broken", "_bogus")

	err := r.index()
	require.Error(t, err)
	lastSeq, _ := r.checkpoint()
	assert.Empty(t, lastSeq, "the checkpoint is held")
	assert.Empty(t, r.fake.FileIDs(), "nothing was indexed on a partial view")
}

func TestIndexMessageJSON(t *testing.T) {
	raw, err := json.Marshal(rag.IndexMessage{Doctype: consts.Files})
	require.NoError(t, err)
	assert.JSONEq(t, `{"doctype":"io.cozy.files"}`, string(raw))
	raw, err = json.Marshal(rag.IndexMessage{Doctype: consts.Files, ReconcileDirID: "x"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"doctype":"io.cozy.files","reconcile_dir_id":"x"}`, string(raw))
}
