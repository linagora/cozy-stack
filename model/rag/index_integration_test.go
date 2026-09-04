package rag_test

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/model/vfs"
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

// ragTest wires a test instance to a fake openRAG whose upload hook plays
// the indexer's status callback.
type ragTest struct {
	t    *testing.T
	inst *instance.Instance
	fake *rag.FakeOpenRAG
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
	fake.OnUpload = func(fileID, docRev string) {
		_ = rag.SetIndexStatus(inst, fileID, rag.StatusSuccess, docRev)
	}
	previous := config.GetConfig().RAGServers
	config.GetConfig().RAGServers = map[string]config.RAGServer{config.DefaultInstanceContext: fake.Server}
	t.Cleanup(func() { config.GetConfig().RAGServers = previous })
	return &ragTest{t: t, inst: inst, fake: fake}
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

func (r *ragTest) addTrigger(msg rag.IndexMessage) job.Trigger {
	r.t.Helper()
	tr, err := job.NewTrigger(r.inst, job.TriggerInfos{
		Type:       "@event",
		WorkerType: "rag-index",
		Arguments:  consts.Files,
	}, msg)
	require.NoError(r.t, err)
	require.NoError(r.t, job.System().AddTrigger(tr))
	return tr
}

func (r *ragTest) index(msg rag.IndexMessage) error {
	return rag.Index(r.inst, rag.TestingLogger(), msg)
}

func (r *ragTest) uploads(doc *vfs.FileDoc) int {
	return r.fake.Rec.Count(http.MethodPost, "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID) +
		r.fake.Rec.Count(http.MethodPut, "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID)
}

func sortedIDs(ids []string) []string {
	out := append([]string{}, ids...)
	slices.Sort(out)
	return out
}

func TestIndexScoped(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	sub := r.mkdir("/KB/sub")
	r.mkdir("/Other")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	inSub := r.writeFile("/KB/sub/b.txt", "beta")
	outside := r.writeFile("/Other/c.txt", "gamma")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)

	require.NoError(t, r.index(msg))

	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the workspace is created by the indexer")
	assert.False(t, r.fake.HasWorkspace(sub.DocID))
	assert.Equal(t, sortedIDs([]string{inKB.DocID, inSub.DocID}), r.fake.FileIDs())
	ff, _ := r.fake.File(inKB.DocID)
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
	assert.Equal(t, hex.EncodeToString(inKB.MD5Sum), ff.MD5)
	_, ok := r.fake.File(outside.DocID)
	assert.False(t, ok, "out-of-scope files never reach openRAG")

	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.NotEmpty(t, cp.LastSeq)

	// Second run: nothing changed, no upload.
	uploads := r.uploads(inKB)
	require.NoError(t, r.index(msg))
	assert.Equal(t, uploads, r.uploads(inKB))
}

func TestIndexGlobalAndScopedCoexist(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	outside := r.writeFile("/Other/c.txt", "gamma")
	global := rag.IndexMessage{Doctype: consts.Files}
	scoped := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(global)
	r.addTrigger(scoped)

	require.NoError(t, r.index(global))
	require.NoError(t, r.index(scoped))

	ff, ok := r.fake.File(outside.DocID)
	require.True(t, ok, "the global trigger indexes everything")
	assert.Empty(t, ff.Workspaces)
	ff, ok = r.fake.File(inKB.DocID)
	require.True(t, ok)
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces, "the scoped run attached the file the global run uploaded")
	assert.Equal(t, 1, r.uploads(inKB), "uploaded once, by the global run")

	// Move the KB file out: with a global trigger it stays indexed, detached.
	other, err := r.inst.VFS().DirByPath("/Other")
	require.NoError(t, err)
	_, err = vfs.ModifyFileMetadata(r.inst.VFS(), inKB, &vfs.DocPatch{DirID: &other.DocID})
	require.NoError(t, err)
	require.NoError(t, r.index(scoped))
	ff, ok = r.fake.File(inKB.DocID)
	require.True(t, ok)
	assert.Empty(t, ff.Workspaces)
}

func TestIndexMoveOutOfScope(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	other := r.mkdir("/Other")
	doc := r.writeFile("/KB/a.txt", "alpha")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))
	_, ok := r.fake.File(doc.DocID)
	require.True(t, ok)

	_, err := vfs.ModifyFileMetadata(r.inst.VFS(), doc, &vfs.DocPatch{DirID: &other.DocID})
	require.NoError(t, err)
	require.NoError(t, r.index(msg))

	_, ok = r.fake.File(doc.DocID)
	assert.False(t, ok, "unclaimed after leaving the scope: deleted")
	var status rag.IndexStatus
	err = couchdbGet(r.inst, doc.DocID, &status)
	assert.True(t, couchdbNotFound(err), "its index status is gone too")
}

func TestIndexNewFileOutOfScopeSkipsOpenRAG(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))

	doc := r.writeFile("/Other/c.txt", "gamma")
	require.NoError(t, r.index(msg))

	assert.Equal(t, "1-", doc.DocRev[:2])
	assert.Equal(t, 0, r.uploads(doc))
	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodGet, "/partition/"+r.inst.Domain+"/files/"+doc.DocID+"/workspaces"), "a rev 1- file out of scope costs no openRAG call")
}

func TestIndexSubtreeMove(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	moving := r.mkdir("/Moving")
	doc := r.writeFile("/Moving/d.txt", "delta")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))
	_, ok := r.fake.File(doc.DocID)
	require.False(t, ok)

	// Drag /Moving into /KB: only the dir doc changes.
	_, err := vfs.ModifyDirMetadata(r.inst.VFS(), moving, &vfs.DocPatch{DirID: &kb.DocID})
	require.NoError(t, err)
	require.NoError(t, r.index(msg))
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "files of a subtree dragged into the scope are indexed")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)

	// Drag it back out.
	moved, err := r.inst.VFS().DirByID(moving.DocID)
	require.NoError(t, err)
	root := consts.RootDirID
	_, err = vfs.ModifyDirMetadata(r.inst.VFS(), moved, &vfs.DocPatch{DirID: &root})
	require.NoError(t, err)
	require.NoError(t, r.index(msg))
	_, ok = r.fake.File(doc.DocID)
	assert.False(t, ok, "files of a subtree dragged out of the scope are deleted")
}

func TestIndexTrashAndRestoreKBFolder(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))

	trashed, err := vfs.TrashDir(r.inst.VFS(), kb)
	require.NoError(t, err)
	require.NoError(t, r.index(msg))
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok, "a trashed KB folder claims nothing")
	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the workspace is kept")

	_, err = vfs.RestoreDir(r.inst.VFS(), trashed)
	require.NoError(t, err)
	require.NoError(t, r.index(msg))
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok, "restored: indexed again")
	assert.Equal(t, []string{kb.DocID}, ff.Workspaces)
}

func TestIndexKeepsCheckpointOnRetryableError(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)

	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID {
			return 503
		}
		return 0
	}
	err := r.index(msg)
	require.Error(t, err)
	assert.True(t, rag.IsRetryable(err))
	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.Empty(t, cp.LastSeq, "checkpoint not advanced")
	assert.Equal(t, 1, cp.Retries)

	r.fake.Fail = nil
	require.NoError(t, r.index(msg))
	_, ok := r.fake.File(doc.DocID)
	assert.True(t, ok)
	cp, err = rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.NotEmpty(t, cp.LastSeq)
	assert.Equal(t, 0, cp.Retries)
}

func TestIndexGivesUpAfterMaxRetries(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, rag.SaveCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID, rag.Checkpoint{Retries: rag.MaxBatchRetries}))

	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+doc.DocID {
			return 503
		}
		return 0
	}
	err := r.index(msg)
	require.Error(t, err)
	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.NotEmpty(t, cp.LastSeq, "past the cap the batch advances anyway")
	assert.Equal(t, 0, cp.Retries)
}

func TestIndexNonRetryableErrorIsSkipped(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	bad := r.writeFile("/KB/bad.txt", "bad")
	good := r.writeFile("/KB/good.txt", "good")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	r.fake.Fail = func(method, path string) int {
		if method == http.MethodPost && path == "/indexer/partition/"+r.inst.Domain+"/file/"+bad.DocID {
			return 415
		}
		return 0
	}

	err := r.index(msg)
	require.Error(t, err)
	assert.False(t, rag.IsRetryable(err))
	_, ok := r.fake.File(good.DocID)
	assert.True(t, ok, "the batch continued past the 4xx")
	cp, err := rag.LoadCheckpoint(r.inst, consts.Files, "rag-index-"+kb.DocID)
	require.NoError(t, err)
	assert.NotEmpty(t, cp.LastSeq, "4xx does not hold the checkpoint")
}

func TestIndexPerTriggerCheckpoints(t *testing.T) {
	r := newRAGTest(t)
	a := r.mkdir("/A")
	b := r.mkdir("/B")
	inA := r.writeFile("/A/a.txt", "alpha")
	inB := r.writeFile("/B/b.txt", "beta")
	msgA := rag.IndexMessage{Doctype: consts.Files, DirID: a.DocID}
	msgB := rag.IndexMessage{Doctype: consts.Files, DirID: b.DocID}
	r.addTrigger(msgA)
	r.addTrigger(msgB)

	require.NoError(t, r.index(msgA))
	_, ok := r.fake.File(inB.DocID)
	assert.False(t, ok)
	require.NoError(t, r.index(msgB))
	ff, ok := r.fake.File(inB.DocID)
	require.True(t, ok, "B's run is not starved by A's checkpoint")
	assert.Equal(t, []string{b.DocID}, ff.Workspaces)
	ff, _ = r.fake.File(inA.DocID)
	assert.Equal(t, []string{a.DocID}, ff.Workspaces)
}

func TestIndexNestedFolders(t *testing.T) {
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
	ff, ok := r.fake.File(doc.DocID)
	require.True(t, ok)
	assert.ElementsMatch(t, []string{outer.DocID, inner.DocID}, ff.Workspaces)
	assert.Equal(t, 1, r.uploads(doc), "uploaded once")
}

func TestIndexMissingFolderIsNoop(t *testing.T) {
	r := newRAGTest(t)
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: "does-not-exist"}
	require.NoError(t, r.index(msg))
	assert.Empty(t, r.fake.Rec.All())
}

func TestIndexDeletedFileIsRemoved(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))

	_, err := vfs.TrashFile(r.inst.VFS(), doc)
	require.NoError(t, err)
	require.NoError(t, r.index(msg))
	_, ok := r.fake.File(doc.DocID)
	assert.False(t, ok)
}

func TestIndexNotSupportedClassStatus(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	img := r.writeFile("/KB/pic.jpg", "not really a jpeg")
	msg := rag.IndexMessage{Doctype: consts.Files, DirID: kb.DocID}
	r.addTrigger(msg)
	require.NoError(t, r.index(msg))

	assert.Equal(t, 0, r.uploads(img), "images are not indexed without the flag")
	var status rag.IndexStatus
	require.NoError(t, couchdbGet(r.inst, img.DocID, &status))
	assert.Equal(t, rag.StatusNotSupported, status.Status)
}

func TestIndexMessageJSON(t *testing.T) {
	raw, err := json.Marshal(rag.IndexMessage{Doctype: consts.Files})
	require.NoError(t, err)
	assert.JSONEq(t, `{"doctype":"io.cozy.files"}`, string(raw))
	raw, err = json.Marshal(rag.IndexMessage{Doctype: consts.Files, DirID: "kb", Action: rag.ActionRemove})
	require.NoError(t, err)
	assert.JSONEq(t, `{"doctype":"io.cozy.files","dir_id":"kb","action":"remove"}`, string(raw))
}

func couchdbGet(inst *instance.Instance, id string, doc *rag.IndexStatus) error {
	return couchdb.GetDoc(inst, consts.ChatRAG, id, doc)
}

func couchdbNotFound(err error) bool {
	return couchdb.IsNotFoundError(err)
}
