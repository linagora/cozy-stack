package search

import (
	"testing"

	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVisiblePaths(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	owner := testutils.NewSetup(t, t.Name()+"_owner").GetTestInstance()
	caller := testutils.NewSetup(t, t.Name()+"_caller").GetTestInstance()

	// Files of the caller
	work, err := vfs.MkdirAll(caller.VFS(), "/Work/2026")
	require.NoError(t, err)
	trashed, err := vfs.MkdirAll(caller.VFS(), "/Old")
	require.NoError(t, err)
	trashed, err = vfs.TrashDir(caller.VFS(), trashed)
	require.NoError(t, err)

	// A shared drive of the owner, under a private folder
	root, err := vfs.MkdirAll(owner.VFS(), "/Private/Team")
	require.NoError(t, err)
	nested, err := vfs.MkdirAll(owner.VFS(), "/Private/Team/a/b")
	require.NoError(t, err)
	outside, err := vfs.MkdirAll(owner.VFS(), "/Private/Elsewhere")
	require.NoError(t, err)

	// Parents of the files of single-file drives
	private, err := owner.VFS().DirByPath("/Private")
	require.NoError(t, err)
	gone, err := vfs.MkdirAll(owner.VFS(), "/Gone")
	require.NoError(t, err)
	gone, err = vfs.TrashDir(owner.VFS(), gone)
	require.NoError(t, err)

	// The drives of the caller (a recipient), as listed by the scope docs
	drives := map[string]Drive{
		"drive-1":    {OwnerInstanceID: owner.Domain, RootDirID: root.ID()},
		"drive-file": {OwnerInstanceID: owner.Domain, RootFileID: "f1"},
	}

	hits := []Hit{
		{OwnerInstanceID: caller.Domain, DirID: work.ID()},
		{OwnerInstanceID: caller.Domain, DirID: consts.RootDirID},
		{OwnerInstanceID: caller.Domain, DirID: trashed.ID()},
		{OwnerInstanceID: owner.Domain, DocID: root.ID(), DirID: root.DirID, DriveID: "drive-1"}, // the root folder itself
		// A sibling of the root, in a stale scope doc: moved out of the drive
		{OwnerInstanceID: owner.Domain, DocID: outside.ID(), DirID: root.DirID, DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: root.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: nested.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: outside.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: nested.ID(), DriveID: "unknown-drive"},
		{DirID: nested.ID(), DriveID: "unknown-drive"}, // no owner either
		{OwnerInstanceID: "other.example.net", DirID: nested.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: nested.ID()}, // not the caller's file
		{OwnerInstanceID: caller.Domain, DirID: "does-not-exist"},
		{OwnerInstanceID: owner.Domain, DirID: private.ID(), DriveID: "drive-file"},
		{OwnerInstanceID: owner.Domain, DirID: gone.ID(), DriveID: "drive-file"},
	}
	paths, err := VisiblePaths(caller, hits, drives)
	require.NoError(t, err)
	assert.Equal(t, []VisiblePath{
		{Path: "/Work/2026", OK: true},
		{Path: "/", OK: true},
		{},                        // parent in the trash
		{OK: true},                // the root folder, without a path
		{},                        // a sibling of the root
		{Path: "/Team", OK: true}, // the name of the root folder
		{Path: "/Team/a/b", OK: true},
		{},         // moved out of the drive
		{},         // unknown drive
		{},         // unknown drive, without owner
		{},         // owner does not match the drive
		{},         // file of another instance without drive
		{},         // missing parent
		{OK: true}, // the file of a single-file drive, without a path
		{},         // the file of a single-file drive, in a trashed folder
	}, paths)

	t.Run("drive of an instance missing from the stack cluster", func(t *testing.T) {
		deleted := map[string]Drive{"drive-deleted": {OwnerInstanceID: "alice.deleted.example", RootDirID: "r1"}}
		paths, err := VisiblePaths(caller, []Hit{
			{OwnerInstanceID: "alice.deleted.example", DirID: "d1", DriveID: "drive-deleted"},
			{OwnerInstanceID: caller.Domain, DirID: work.ID()},
		}, deleted)
		require.NoError(t, err)
		assert.Equal(t, []VisiblePath{
			{}, // the leftovers of a deleted instance
			{Path: "/Work/2026", OK: true},
		}, paths)
	})
}
