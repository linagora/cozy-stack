package recents

import (
	"bytes"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const recentsFlag = "drive.recents.enabled"

// setupInstances returns one instance by name, with the recents enabled except
// for the one named noflag.
func setupInstances(t *testing.T, names ...string) map[string]*instance.Instance {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	insts := make(map[string]*instance.Instance, len(names))
	for _, name := range names {
		inst := testutils.NewSetup(t, t.Name()+name).GetTestInstance()
		testutils.WithFlag(t, inst, recentsFlag, name != "noflag")
		insts[name] = inst
	}
	return insts
}

func mkdir(t *testing.T, inst *instance.Instance, path string) *vfs.DirDoc {
	t.Helper()
	dir, err := vfs.MkdirAll(inst.VFS(), path)
	require.NoError(t, err)
	return dir
}

func newFile(t *testing.T, inst *instance.Instance, dir *vfs.DirDoc, name, content string) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, dir.ID(), int64(len(content)), nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, nil)
	require.NoError(t, err)
	_, err = bytes.NewBufferString(content).WriteTo(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got, err := inst.VFS().FileByID(doc.ID())
	require.NoError(t, err)
	return got
}

// patchFile renames a file, or moves it when a folder is given.
func patchFile(t *testing.T, inst *instance.Instance, file *vfs.FileDoc, name string, dir *vfs.DirDoc) *vfs.FileDoc {
	t.Helper()
	patch := &vfs.DocPatch{Name: &name}
	if dir != nil {
		id := dir.ID()
		patch = &vfs.DocPatch{DirID: &id}
	}
	got, err := vfs.ModifyFileMetadata(inst.VFS(), file, patch)
	require.NoError(t, err)
	return got
}

// putRow crafts the row of a user on an item, with metadata to be replaced.
func putRow(t *testing.T, inst *instance.Instance, id string) *recents.FileDirActivity {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          id,
		LastEditedAt:   "2026-01-01T00:00:00Z",
		LastViewedAt:   "2026-01-02T00:00:00Z",
		LastActivityAt: "2026-01-02T00:00:00Z",
		Name:           "old-name",
		Type:           consts.FileType,
		UpdatedAt:      "2020-01-01T00:00:00Z",
		DirID:          "old-dir",
		Path:           "/old-path",
		SchemaVersion:  recents.SchemaVersion,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, row))
	return row
}

func getRow(t *testing.T, inst *instance.Instance, id string) *recents.FileDirActivity {
	t.Helper()
	row := &recents.FileDirActivity{}
	require.NoError(t, couchdb.GetDoc(inst, consts.FilesRecents, id, row))
	return row
}

func assertNoRow(t *testing.T, inst *instance.Instance, id string) {
	t.Helper()
	err := couchdb.GetDoc(inst, consts.FilesRecents, id, &recents.FileDirActivity{})
	assert.True(t, couchdb.IsNotFoundError(err), "there should be no row: %v", err)
}

// assertUntouched checks that the row is still the given revision of the row.
func assertUntouched(t *testing.T, inst *instance.Instance, row *recents.FileDirActivity) {
	t.Helper()
	assert.Equal(t, row, getRow(t, inst, row.DocID))
}

// fileRow is the row that putRow has crafted, once it has the metadata of a
// file created by newFile.
func fileRow(file *vfs.FileDoc, path string) *recents.FileDirActivity {
	return &recents.FileDirActivity{
		DocID:          file.ID(),
		LastEditedAt:   "2026-01-01T00:00:00Z",
		LastViewedAt:   "2026-01-02T00:00:00Z",
		LastActivityAt: "2026-01-02T00:00:00Z",
		Name:           file.DocName,
		Type:           consts.FileType,
		Mime:           "text/plain",
		Class:          "text",
		Size:           "5",
		UpdatedAt:      file.UpdatedAt.Format(time.RFC3339Nano),
		DirID:          file.DirID,
		Path:           path,
		SchemaVersion:  recents.SchemaVersion,
	}
}

// assertRow compares a stored row with what is expected, whatever its revision.
func assertRow(t *testing.T, inst *instance.Instance, want *recents.FileDirActivity) {
	t.Helper()
	got := getRow(t, inst, want.DocID)
	want.DocRev = got.DocRev
	assert.Equal(t, want, got)
}

// runWorker runs a worker of this package on an instance, with a message.
func runWorker(t *testing.T, inst *instance.Instance, worker job.WorkerFunc, message any) {
	t.Helper()
	msg, err := job.NewMessage(message)
	require.NoError(t, err)
	j := &job.Job{JobID: "1", Domain: inst.Domain, Message: msg}
	ctx, cancel := job.NewTaskContext("0", j, inst)
	defer cancel()
	require.NoError(t, worker(ctx))
}

func runSync(t *testing.T, owner *instance.Instance, ids ...string) {
	t.Helper()
	runWorker(t, owner, WorkerSync, &job.RecentsSyncMessage{IDs: ids})
}

func TestWorkerSync(t *testing.T) {
	insts := setupInstances(t, "owner", "noflag")
	owner, noFlag := insts["owner"], insts["noflag"]

	t.Run("FileRowFollowsARenameAndAMove", func(t *testing.T) {
		dir := mkdir(t, owner, "/File/dir")
		other := mkdir(t, owner, "/File/other")
		file := newFile(t, owner, dir, "before.txt", "hello")
		bare := newFile(t, owner, dir, "bare.txt", "hello")
		putRow(t, owner, file.ID())

		file = patchFile(t, owner, file, "after.txt", nil)
		runSync(t, owner, file.ID(), bare.ID())
		// The dates of the activity are kept, and the row has no drive.
		assertRow(t, owner, fileRow(file, "/File/dir/after.txt"))
		assertNoRow(t, owner, bare.ID())

		file = patchFile(t, owner, file, "", other)
		runSync(t, owner, file.ID())
		require.Equal(t, other.ID(), file.DirID)
		assertRow(t, owner, fileRow(file, "/File/other/after.txt"))
	})

	t.Run("FolderRowFollowsARename", func(t *testing.T) {
		dir := mkdir(t, owner, "/Folder/before")
		putRow(t, owner, dir.ID())

		name := "after"
		dir, err := vfs.ModifyDirMetadata(owner.VFS(), dir, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)
		runSync(t, owner, dir.ID())

		got := getRow(t, owner, dir.ID())
		assert.Equal(t, "after", got.Name)
		assert.Equal(t, consts.DirType, got.Type)
		assert.Empty(t, got.Size)
		assert.Equal(t, "/Folder/after", got.Path)
	})

	t.Run("TrashKeepsTheRowsAndDestroyDeletesThem", func(t *testing.T) {
		root := mkdir(t, owner, "/Trash")
		dir := mkdir(t, owner, "/Trash/sub")
		inside := newFile(t, owner, dir, "a.txt", "hello")
		file := newFile(t, owner, root, "b.txt", "hello")
		putRow(t, owner, dir.ID())
		putRow(t, owner, inside.ID())
		putRow(t, owner, file.ID())

		// The row of the owner tells where the item is.
		trashed, err := vfs.TrashFile(owner.VFS(), file)
		require.NoError(t, err)
		runSync(t, owner, file.ID())
		require.Equal(t, consts.TrashDirID, trashed.DirID)
		assertRow(t, owner, fileRow(trashed, vfs.TrashDirName+"/"+trashed.DocName))

		restored, err := vfs.RestoreFile(owner.VFS(), trashed)
		require.NoError(t, err)
		runSync(t, owner, file.ID())
		assertRow(t, owner, fileRow(restored, "/Trash/b.txt"))

		trashedDir, err := vfs.TrashDir(owner.VFS(), dir)
		require.NoError(t, err)
		runSync(t, owner, dir.ID(), inside.ID())
		assert.Equal(t, trashedDir.Fullpath+"/a.txt", getRow(t, owner, inside.ID()).Path)

		require.NoError(t, owner.VFS().DestroyDirAndContent(trashedDir, owner.VFS().EnsureErased))
		require.NoError(t, owner.VFS().DestroyFile(restored))
		runSync(t, owner, dir.ID(), inside.ID(), file.ID())
		assertNoRow(t, owner, dir.ID())
		assertNoRow(t, owner, inside.ID())
		assertNoRow(t, owner, file.ID())
	})

	t.Run("FlagOffOnTheOwnerInstance", func(t *testing.T) {
		file := newFile(t, noFlag, mkdir(t, noFlag, "/FlagOff"), "a.txt", "hello")
		row := putRow(t, noFlag, file.ID())

		file = patchFile(t, noFlag, file, "b.txt", nil)
		runSync(t, noFlag, file.ID())
		assertUntouched(t, noFlag, row)
	})

	t.Run("FixedIDsAreSkipped", func(t *testing.T) {
		row := putRow(t, owner, consts.RootDirID)
		runSync(t, owner, consts.RootDirID)
		assertUntouched(t, owner, row)
	})
}
