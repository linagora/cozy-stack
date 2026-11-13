package recents

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

func writer(inst *instance.Instance) sharing.Member {
	return sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://" + inst.Domain}
}

func reader(inst *instance.Instance) sharing.Member {
	m := writer(inst)
	m.ReadOnly = true
	return m
}

func withStatus(m sharing.Member, status string) sharing.Member {
	m.Status = status
	return m
}

// memberRow is fileRow for a member of a drive of the owner.
func memberRow(file *vfs.FileDoc, path string, drive *sharing.Sharing, owner *instance.Instance) *recents.FileDirActivity {
	row := fileRow(file, path)
	row.DriveID = drive.SID
	row.OwnerDomain = owner.Domain
	return row
}

func TestWorkerSyncMembers(t *testing.T) {
	insts := setupInstances(t, "owner", "bob", "carol", "noflag", "erin", "dave")
	owner, bob, carol, noFlag := insts["owner"], insts["bob"], insts["carol"], insts["noflag"]
	erin, dave := insts["erin"], insts["dave"]

	t.Run("FileRowTakesTheDriveOfTheMemberAndItsPath", func(t *testing.T) {
		rootA := mkdir(t, owner, "/What/A")
		subA := mkdir(t, owner, "/What/A/sub")
		rootB := mkdir(t, owner, "/What/B")
		outside := mkdir(t, owner, "/What/Private")
		driveA := newDrive(t, owner, rootA.ID(), writer(bob))
		driveB := newDrive(t, owner, rootB.ID(), writer(bob))
		file := newFile(t, owner, subA, "before.txt", "hello")
		putRow(t, bob, file.ID())

		// The dates of the activity are kept, and the path starts at the drive.
		file = patchFile(t, owner, file, "after.txt", nil)
		runSync(t, owner, file.ID())
		assertRow(t, bob, memberRow(file, "/sub/after.txt", driveA, owner))

		file = patchFile(t, owner, file, "", rootB)
		runSync(t, owner, file.ID())
		assertRow(t, bob, memberRow(file, "/after.txt", driveB, owner))

		// Out of the drives of the member, the row stays as it is.
		row := getRow(t, bob, file.ID())
		file = patchFile(t, owner, file, "", outside)
		runSync(t, owner, file.ID())
		assertUntouched(t, bob, row)
	})

	t.Run("FolderRowAndRootOfTheDrive", func(t *testing.T) {
		root := mkdir(t, owner, "/Folder/Drive")
		dir := mkdir(t, owner, "/Folder/Drive/before")
		drive := newDrive(t, owner, root.ID(), writer(bob))
		putRow(t, bob, dir.ID())
		putRow(t, bob, root.ID())

		name := "after"
		dir, err := vfs.ModifyDirMetadata(owner.VFS(), dir, &vfs.DocPatch{Name: &name})
		require.NoError(t, err)
		runSync(t, owner, dir.ID(), root.ID())

		got := getRow(t, bob, dir.ID())
		assert.Equal(t, "after", got.Name)
		assert.Equal(t, consts.DirType, got.Type)
		assert.Equal(t, "/after", got.Path)
		assert.Equal(t, drive.SID, got.DriveID)
		assert.Empty(t, getRow(t, bob, root.ID()).Path, "nothing of the tree above the drive")
	})

	t.Run("NestedDriveWinsForItsWriter", func(t *testing.T) {
		outer := mkdir(t, owner, "/Nested/outer")
		inner := mkdir(t, owner, "/Nested/outer/inner")
		newDrive(t, owner, outer.ID(), reader(bob))
		innerDrive := newDrive(t, owner, inner.ID(), writer(bob))
		file := newFile(t, owner, inner, "a.txt", "hello")
		putRow(t, bob, file.ID())

		runSync(t, owner, file.ID())
		assertRow(t, bob, memberRow(file, "/a.txt", innerDrive, owner))
	})

	t.Run("OnlyTheReadyMembersOfTheDrivesOfTheOwner", func(t *testing.T) {
		rootA := mkdir(t, owner, "/Who/A")
		rootB := mkdir(t, owner, "/Who/B")
		newDrive(t, owner, rootA.ID(),
			sharing.Member{Status: sharing.MemberStatusReady, Instance: "https://not-on-this-stack.example.net"},
			writer(bob), writer(noFlag),
			withStatus(writer(erin), sharing.MemberStatusRevoked),
			withStatus(writer(dave), sharing.MemberStatusPendingInvitation))
		newDrive(t, owner, rootB.ID(), writer(carol))
		file := newFile(t, owner, rootA, "a.txt", "hello")
		bare := newFile(t, owner, rootA, "bare.txt", "hello")
		putRow(t, owner, file.ID())
		putRow(t, bob, file.ID())
		// Carol is a member of another drive of the owner only.
		left := make(map[*instance.Instance]*recents.FileDirActivity)
		for _, inst := range []*instance.Instance{noFlag, erin, dave, carol} {
			left[inst] = putRow(t, inst, file.ID())
		}

		file = patchFile(t, owner, file, "b.txt", nil)
		runSync(t, owner, file.ID(), bare.ID())
		assert.Equal(t, "b.txt", getRow(t, owner, file.ID()).Name)
		assert.Equal(t, "b.txt", getRow(t, bob, file.ID()).Name)
		assertNoRow(t, bob, bare.ID())
		for inst, row := range left {
			assertUntouched(t, inst, row)
		}

		// The drive of a destroyed item is out of reach: carol is found by her
		// other drive, and the row of a member who is not ready stays, as an
		// orphan.
		require.NoError(t, owner.VFS().DestroyFile(file))
		runSync(t, owner, file.ID())
		assertNoRow(t, owner, file.ID())
		assertNoRow(t, bob, file.ID())
		assertNoRow(t, carol, file.ID())
		delete(left, carol)
		for inst, row := range left {
			assertUntouched(t, inst, row)
		}
	})

	t.Run("FlagOffOnTheOwnerStillUpdatesTheMembers", func(t *testing.T) {
		root := mkdir(t, noFlag, "/FlagOffOwner")
		newDrive(t, noFlag, root.ID(), writer(bob))
		file := newFile(t, noFlag, root, "a.txt", "hello")
		mine := putRow(t, noFlag, file.ID())
		putRow(t, bob, file.ID())

		file = patchFile(t, noFlag, file, "b.txt", nil)
		runSync(t, noFlag, file.ID())
		assertUntouched(t, noFlag, mine)
		assert.Equal(t, "b.txt", getRow(t, bob, file.ID()).Name)
	})

	t.Run("MembersOfADriveOfSomeoneElseAreNotRecipients", func(t *testing.T) {
		// On the instance of a member, the drive is held with the list of its
		// members, but the changes of the items of this instance are not theirs.
		dir := mkdir(t, bob, "/NotOwner")
		s := newDrive(t, bob, dir.ID(), writer(carol))
		s.Owner = false
		require.NoError(t, couchdb.UpdateDoc(bob, s))
		file := newFile(t, bob, dir, "a.txt", "hello")
		row := putRow(t, carol, file.ID())

		runSync(t, bob, file.ID())
		assertUntouched(t, carol, row)
	})

	t.Run("TrashedItemsAreLeftUntilRestoredOrDestroyed", func(t *testing.T) {
		root := mkdir(t, owner, "/Bin/Drive")
		dir := mkdir(t, owner, "/Bin/Drive/sub")
		drive := newDrive(t, owner, root.ID(), writer(bob))
		inside := newFile(t, owner, dir, "a.txt", "hello")
		file := newFile(t, owner, root, "b.txt", "hello")
		rows := []*recents.FileDirActivity{
			putRow(t, bob, dir.ID()), putRow(t, bob, inside.ID()), putRow(t, bob, file.ID()),
		}

		trashed, err := vfs.TrashFile(owner.VFS(), file)
		require.NoError(t, err)
		trashedDir, err := vfs.TrashDir(owner.VFS(), dir)
		require.NoError(t, err)
		runSync(t, owner, dir.ID(), inside.ID(), file.ID())
		for _, row := range rows {
			assertUntouched(t, bob, row)
		}

		restored, err := vfs.RestoreFile(owner.VFS(), trashed)
		require.NoError(t, err)
		runSync(t, owner, file.ID())
		assertRow(t, bob, memberRow(restored, "/b.txt", drive, owner))

		// The root of the drive goes to the trash, with the restored file. The
		// reference to the drive has changed the revision of the root.
		rows = []*recents.FileDirActivity{putRow(t, bob, root.ID()), getRow(t, bob, file.ID())}
		root, err = owner.VFS().DirByID(root.ID())
		require.NoError(t, err)
		_, err = vfs.TrashDir(owner.VFS(), root)
		require.NoError(t, err)
		runSync(t, owner, root.ID(), file.ID())
		for _, row := range rows {
			assertUntouched(t, bob, row)
		}

		// A destruction from the trash still finds the members.
		require.NoError(t, owner.VFS().DestroyDirAndContent(trashedDir, owner.VFS().EnsureErased))
		runSync(t, owner, dir.ID(), inside.ID())
		assertNoRow(t, bob, dir.ID())
		assertNoRow(t, bob, inside.ID())
	})

	t.Run("PathNotUnderTheRootIsNeverWritten", func(t *testing.T) {
		scope := &sharing.SharingScope{
			Sharing:  &sharing.Sharing{SID: "drive"},
			RootPath: "/elsewhere",
		}
		_, err := memberMetadata(owner, scope, &item{fullpath: "/Private/a.txt"})
		assert.Error(t, err)
	})

	t.Run("AChangeOfAFileEndsUpInTheRowsThroughTheTrigger", func(t *testing.T) {
		// The real trigger of the stack, with its real delay.
		root := mkdir(t, owner, "/Trigger/Drive")
		newDrive(t, owner, root.ID(), writer(bob))
		file := newFile(t, owner, root, "a.txt", "a")
		putRow(t, owner, file.ID())
		putRow(t, bob, file.ID())

		patchFile(t, owner, file, "b.txt", nil)
		named := func(inst *instance.Instance) bool {
			row := &recents.FileDirActivity{}
			err := couchdb.GetDoc(inst, consts.FilesRecents, file.ID(), row)
			return err == nil && row.Name == "b.txt"
		}
		assert.Eventually(t, func() bool {
			return named(owner) && named(bob)
		}, 60*time.Second, 200*time.Millisecond)
	})
}
