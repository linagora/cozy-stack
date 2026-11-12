package recents

import (
	"bytes"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
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

// writeFile creates a file, or gives a new content to the given one.
func writeFile(t *testing.T, inst *instance.Instance, dir *vfs.DirDoc, name, content string, old *vfs.FileDoc) *vfs.FileDoc {
	t.Helper()
	doc, err := vfs.NewFileDoc(name, dir.ID(), int64(len(content)), nil, "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	f, err := inst.VFS().CreateFile(doc, old)
	require.NoError(t, err)
	_, err = bytes.NewBufferString(content).WriteTo(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	got, err := inst.VFS().FileByID(doc.ID())
	require.NoError(t, err)
	return got
}

func newFile(t *testing.T, inst *instance.Instance, dir *vfs.DirDoc, name, content string) *vfs.FileDoc {
	t.Helper()
	return writeFile(t, inst, dir, name, content, nil)
}

// newDrive creates a drive of the owner rooted at the given item.
func newDrive(t *testing.T, owner *instance.Instance, rootID string, members ...sharing.Member) *sharing.Sharing {
	t.Helper()
	now := time.Now()
	s := &sharing.Sharing{
		Active:        true,
		Owner:         true,
		Drive:         true,
		DriveRootType: sharing.DriveRootTypeDirectory,
		AppSlug:       "test",
		AccessMode:    sharing.AccessModeAdditive,
		Members: append([]sharing.Member{{
			Status:   sharing.MemberStatusOwner,
			Instance: "https://" + owner.Domain,
		}}, members...),
		Rules:     []sharing.Rule{{Title: "test", DocType: consts.Files, Values: []string{rootID}}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, couchdb.CreateDoc(owner, s))
	require.NoError(t, s.AddReferenceForSharing(owner, &s.Rules[0]))
	return s
}

// member is a member of a drive with the given status.
func member(inst *instance.Instance, status string) sharing.Member {
	return sharing.Member{Status: status, Instance: "https://" + inst.Domain}
}

func writer(inst *instance.Instance) sharing.Member {
	return member(inst, sharing.MemberStatusReady)
}

// putRow crafts the row of a user on an item.
func putRow(t *testing.T, inst *instance.Instance, id string) *recents.FileDirActivity {
	t.Helper()
	row := &recents.FileDirActivity{
		DocID:          id,
		LastEditedAt:   "2026-01-01T00:00:00Z",
		LastViewedAt:   "2026-01-02T00:00:00Z",
		LastActivityAt: "2026-01-02T00:00:00Z",
		Name:           "old-name",
		Type:           consts.FileType,
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
	insts := setupInstances(t, "owner", "bob", "carol", "noflag", "erin", "dave")
	owner, bob, carol, noFlag := insts["owner"], insts["bob"], insts["carol"], insts["noflag"]
	erin, dave := insts["erin"], insts["dave"]
	both := []*instance.Instance{owner, bob}

	t.Run("LivingItemsKeepTheirRowsAsTheyAre", func(t *testing.T) {
		root := mkdir(t, owner, "/Living")
		dir := mkdir(t, owner, "/Living/dir")
		newDrive(t, owner, root.ID(), writer(bob))
		file := newFile(t, owner, root, "before.txt", "hello")
		rows := make(map[*recents.FileDirActivity]*instance.Instance)
		for _, inst := range both {
			rows[putRow(t, inst, file.ID())] = inst
			rows[putRow(t, inst, dir.ID())] = inst
		}
		syncWritesNothing := func() {
			t.Helper()
			runSync(t, owner, file.ID(), dir.ID())
			for row, inst := range rows {
				assertUntouched(t, inst, row)
			}
		}

		// A rename, a move and a new content of the file, a rename of the folder.
		name, dirID := "after.txt", dir.ID()
		file, err := vfs.ModifyFileMetadata(owner.VFS(), file, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)
		syncWritesNothing()
		file, err = vfs.ModifyFileMetadata(owner.VFS(), file, &vfs.DocPatch{DirID: &dirID})
		require.NoError(t, err)
		syncWritesNothing()
		file = writeFile(t, owner, dir, name, "hello world", file)
		syncWritesNothing()
		name = "renamed"
		dir, err = vfs.ModifyDirMetadata(owner.VFS(), dir, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)
		syncWritesNothing()

		// The trash is not a destruction: the items can be restored.
		_, err = vfs.TrashDir(owner.VFS(), dir)
		require.NoError(t, err)
		syncWritesNothing()
	})

	t.Run("DestroyedItemsLoseTheirRows", func(t *testing.T) {
		root := mkdir(t, owner, "/Destroy")
		dir := mkdir(t, owner, "/Destroy/sub")
		newDrive(t, owner, root.ID(), writer(bob))
		inside := newFile(t, owner, dir, "a.txt", "hello")
		file := newFile(t, owner, root, "b.txt", "hello")
		bare := newFile(t, owner, root, "bare.txt", "hello")
		gone := []string{dir.ID(), inside.ID(), file.ID()}
		kept := make(map[*instance.Instance]*recents.FileDirActivity)
		for _, inst := range both {
			kept[inst] = putRow(t, inst, root.ID())
			for _, id := range gone {
				putRow(t, inst, id)
			}
		}

		// The folder is destroyed from the trash, the files directly.
		trashed, err := vfs.TrashDir(owner.VFS(), dir)
		require.NoError(t, err)
		require.NoError(t, owner.VFS().DestroyDirAndContent(trashed, owner.VFS().EnsureErased))
		require.NoError(t, owner.VFS().DestroyFile(file))
		require.NoError(t, owner.VFS().DestroyFile(bare))
		runSync(t, owner, append(gone, bare.ID(), root.ID())...)
		for _, inst := range both {
			for _, id := range append(gone, bare.ID()) {
				assertNoRow(t, inst, id)
			}
			assertUntouched(t, inst, kept[inst])
		}
	})

	t.Run("OnlyTheReadyMembersOfTheDrivesOfTheOwner", func(t *testing.T) {
		rootA := mkdir(t, owner, "/Who/A")
		rootB := mkdir(t, owner, "/Who/B")
		newDrive(t, owner, rootA.ID(),
			sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://not-on-this-stack.example.net"},
			writer(bob), writer(noFlag),
			member(erin, sharing.MemberStatusRevoked),
			member(dave, sharing.MemberStatusPendingInvitation))
		newDrive(t, owner, rootB.ID(), writer(carol))
		file := newFile(t, owner, rootA, "a.txt", "hello")
		rows := make(map[*instance.Instance]*recents.FileDirActivity)
		for _, inst := range []*instance.Instance{owner, bob, carol, noFlag, erin, dave} {
			rows[inst] = putRow(t, inst, file.ID())
		}

		// The drive of a destroyed item is out of reach: carol, who is a member
		// of another drive of the owner only, is found by it. The row of a
		// member who is not ready stays.
		require.NoError(t, owner.VFS().DestroyFile(file))
		runSync(t, owner, file.ID())
		for _, inst := range []*instance.Instance{owner, bob, carol} {
			assertNoRow(t, inst, file.ID())
			delete(rows, inst)
		}
		for inst, row := range rows {
			assertUntouched(t, inst, row)
		}
	})

	t.Run("FlagOffOnTheOwnerStillDeletesForTheMembers", func(t *testing.T) {
		root := mkdir(t, noFlag, "/FlagOffOwner")
		newDrive(t, noFlag, root.ID(), writer(bob))
		file := newFile(t, noFlag, root, "a.txt", "hello")
		mine := putRow(t, noFlag, file.ID())
		putRow(t, bob, file.ID())

		require.NoError(t, noFlag.VFS().DestroyFile(file))
		runSync(t, noFlag, file.ID())
		assertUntouched(t, noFlag, mine)
		assertNoRow(t, bob, file.ID())
	})

	t.Run("MembersOfADriveOfSomeoneElseAreNotRecipients", func(t *testing.T) {
		// On the instance of a member, the drive is held with the list of its
		// members, but the items of this instance are not theirs.
		dir := mkdir(t, bob, "/NotOwner")
		s := newDrive(t, bob, dir.ID(), writer(carol))
		s.Owner = false
		require.NoError(t, couchdb.UpdateDoc(bob, s))
		file := newFile(t, bob, dir, "a.txt", "hello")
		row := putRow(t, carol, file.ID())

		require.NoError(t, bob.VFS().DestroyFile(file))
		runSync(t, bob, file.ID())
		assertUntouched(t, carol, row)
	})

	t.Run("FixedIDsAreSkipped", func(t *testing.T) {
		row := putRow(t, owner, consts.RootDirID)
		runSync(t, owner, consts.RootDirID)
		assertUntouched(t, owner, row)
	})
}
