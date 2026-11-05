package sharing

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	openBobEmail    = "bob@example.net"
	openBobInstance = "https://bob.example.net"
)

// createOwnedDriveWithBob creates a drive sharing as seen on the owner's
// instance, with Bob as a ready member.
func createOwnedDriveWithBob(t *testing.T, inst *instance.Instance, rootID string, readOnly bool) *Sharing {
	t.Helper()
	s := createActiveRecipientSharing(t, inst, rootID, readOnly, openBobInstance)
	s.Owner = true
	s.Members[1].Email = openBobEmail
	require.NoError(t, couchdb.UpdateDoc(inst, s))
	return s
}

// openerWithInteractCode builds the opener like the web open handlers do for a
// request carrying a share-interact token of the given sharing.
func openerWithInteractCode(t *testing.T, inst *instance.Instance, file *vfs.FileDoc, s *Sharing, code string) *FileOpener {
	t.Helper()
	opener, err := NewFileOpener(inst, file)
	require.NoError(t, err)
	opener.AddShareByLinkCode(code)
	pdoc, err := permission.GetForShareInteract(inst, s.ID())
	require.NoError(t, err)
	require.NoError(t, opener.CheckPermission(pdoc, ""))
	require.True(t, opener.InteractToken)
	return opener
}

// Bob is read-only on the drive X, and a writer on the drive Y nested in X.
// He opens a file of Y through the route of X, which is correctly not read-only
// as his effective access on the file comes from Y. When the resolver cannot
// tell which sharing grants the write, Bob must not get the code of X, as it
// carries all the verbs on the whole X.
func TestGetSharecodeInteractTokenResolverFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	x := createTree(t, fs, H{"x/": H{"y/": H{"nested.txt": H{}}}}, consts.RootDirID)
	y, err := fs.DirByPath(x.Fullpath + "/y")
	require.NoError(t, err)
	file, err := fs.FileByPath(y.Fullpath + "/nested.txt")
	require.NoError(t, err)

	sX := createOwnedDriveWithBob(t, inst, x.ID(), true)
	sY := createOwnedDriveWithBob(t, inst, y.ID(), false)
	xCode, err := sX.GetInteractCode(inst, &sX.Members[1], 1)
	require.NoError(t, err)
	yCode, err := sY.GetInteractCode(inst, &sY.Members[1], 1)
	require.NoError(t, err)
	require.NotEqual(t, xCode, yCode)

	// What driveOpenReadOnly computes for the open route of X.
	ea, err := NewAccessResolver(inst).ResolveForMember(file.ID(), &sX.Members[1])
	require.NoError(t, err)
	require.True(t, ea.CanWrite, "readOnly must be false for this scenario")

	t.Run("NominalCaseGivesTheCodeOfTheNestedDrive", func(t *testing.T) {
		opener := openerWithInteractCode(t, inst, file, sX, xCode)
		code, err := opener.GetSharecode(0, false)
		require.NoError(t, err)
		assert.Equal(t, yCode, code)
	})

	t.Run("ResolverErrorMustNotGiveTheCodeOfTheOuterDrive", func(t *testing.T) {
		opener := openerWithInteractCode(t, inst, file, sX, xCode)

		// The resolver needs io.cozy.sharings, whereas GetInteractCode only
		// needs the sharing already loaded in the opener and
		// io.cozy.permissions: it simulates a CouchDB failure hitting only the
		// resolver.
		require.NoError(t, couchdb.DeleteDB(inst, consts.Sharings))
		sc, resolverErr := NewAccessResolver(inst).NearestWritableSharing(file.ID(), &sX.Members[1])
		require.Error(t, resolverErr, "the resolver must fail for this scenario")
		require.Nil(t, sc)

		code, err := opener.GetSharecode(0, false)
		if err != nil {
			return // Failing closed is an acceptable outcome
		}
		interact, err := permission.GetForShareInteract(inst, sX.ID())
		require.NoError(t, err)
		for member, xc := range interact.Codes {
			assert.NotEqualf(t, xc, code,
				"Read-only member got the share-interact code of the outer drive (member key %s), with verbs %v on %v",
				member, interact.Permissions[0].Verbs, interact.Permissions[0].Values)
		}
	})
}

// Bob is read-only on the drive and on no other sharing. Even if the caller does
// not ask for the read-only mode, the code must not be wider than his rights.
func TestGetSharecodeInteractTokenWithoutWritableSharing(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	x := createTree(t, fs, H{"x/": H{"file.txt": H{}}}, consts.RootDirID)
	file, err := fs.FileByPath(x.Fullpath + "/file.txt")
	require.NoError(t, err)

	sX := createOwnedDriveWithBob(t, inst, x.ID(), true)
	xCode, err := sX.GetInteractCode(inst, &sX.Members[1], 1)
	require.NoError(t, err)

	// The index given by the caller is not the one of Bob: it must be ignored.
	opener := openerWithInteractCode(t, inst, file, sX, xCode)
	code, err := opener.GetSharecode(0, false)
	require.NoError(t, err)

	preview, err := permission.GetForSharePreview(inst, sX.ID())
	require.NoError(t, err)
	assert.Equal(t, preview.ShortCodes[openBobEmail], code)
}
