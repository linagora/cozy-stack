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

	// The drives of the caller (a recipient), as listed by the scope docs
	drives := map[string]Drive{"drive-1": {OwnerInstanceID: owner.Domain, RootDirID: root.ID()}}

	hits := []Hit{
		{OwnerInstanceID: caller.Domain, DirID: work.ID()},
		{OwnerInstanceID: caller.Domain, DirID: consts.RootDirID},
		{OwnerInstanceID: caller.Domain, DirID: trashed.ID()},
		{OwnerInstanceID: owner.Domain, DirID: root.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: nested.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: outside.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: nested.ID(), DriveID: "unknown-drive"},
		{OwnerInstanceID: "other.example.net", DirID: nested.ID(), DriveID: "drive-1"},
		{OwnerInstanceID: owner.Domain, DirID: nested.ID()}, // not the caller's file
		{OwnerInstanceID: caller.Domain, DirID: "does-not-exist"},
	}
	paths, err := VisiblePaths(caller, hits, drives)
	require.NoError(t, err)
	assert.Equal(t, []VisiblePath{
		{Path: "/Work/2026", OK: true},
		{Path: "/", OK: true},
		{},                        // parent in the trash
		{Path: "/Team", OK: true}, // the name of the root folder
		{Path: "/Team/a/b", OK: true},
		{}, // moved out of the drive
		{}, // unknown drive
		{}, // owner does not match the drive
		{}, // file of another instance without drive
		{}, // missing parent
	}, paths)

	t.Run("drive owned outside of the stack cluster", func(t *testing.T) {
		remote := map[string]Drive{"drive-remote": {OwnerInstanceID: "alice.remote.example", RootDirID: "r1"}}
		paths, err := VisiblePaths(caller, []Hit{
			{OwnerInstanceID: "alice.remote.example", DirID: "d1", DriveID: "drive-remote"},
			{OwnerInstanceID: caller.Domain, DirID: work.ID()},
			{OwnerInstanceID: "mallory.remote.example", DirID: "d1", DriveID: "drive-remote"},
		}, remote)
		require.NoError(t, err)
		assert.Equal(t, []VisiblePath{
			{OK: true}, // kept, without a path
			{Path: "/Work/2026", OK: true},
			{}, // owner does not match the drive
		}, paths)
	})
}
