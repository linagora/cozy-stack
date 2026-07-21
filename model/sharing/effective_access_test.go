package sharing

import (
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccessResolver_AncestorsOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	s := createActiveDirSharing(t, inst, parent.ID())

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(child.ID())
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, s.SID, scopes[0].SharingID)
	assert.Equal(t, parent.ID(), scopes[0].RootID)
	assert.Equal(t, parent.Fullpath, scopes[0].RootPath)

	ea, err := resolver.Resolve(child.ID())
	require.NoError(t, err)
	assert.True(t, ea.CanRead)
	assert.True(t, ea.CanWrite)
	assert.True(t, ea.Can(permission.GET))
	assert.True(t, ea.Can(permission.PUT))
}

func TestAccessResolver_SelfAndAncestor(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	sParent := createActiveDirSharing(t, inst, parent.ID())
	sChild := createActiveDirSharing(t, inst, child.ID())

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(child.ID())
	require.NoError(t, err)
	require.Len(t, scopes, 2)
	got := map[string]SharingScope{}
	for _, sc := range scopes {
		got[sc.SharingID] = sc
	}
	assert.Contains(t, got, sParent.SID)
	assert.Contains(t, got, sChild.SID)
	assert.Equal(t, child.ID(), got[sChild.SID].RootID)
}

func TestAccessResolver_SkipsInactive(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	s := createActiveDirSharing(t, inst, parent.ID())
	s.Active = false
	require.NoError(t, couchdb.UpdateDoc(inst, s))

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(child.ID())
	require.NoError(t, err)
	assert.Empty(t, scopes)
}

func TestAccessResolver_SkipsLimitedAccess(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	s := createActiveDirSharing(t, inst, parent.ID())
	s.AccessMode = AccessModeLimitedAccess
	require.NoError(t, couchdb.UpdateDoc(inst, s))

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(child.ID())
	require.NoError(t, err)
	assert.Empty(t, scopes)
}

func TestAccessResolver_FileTargetWithOwnShare(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"file.txt": nil}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	file, err := fs.FileByPath(parent.Fullpath + "/file.txt")
	require.NoError(t, err)

	s := createActiveFileSharing(t, inst, file.ID())

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(file.ID())
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, s.SID, scopes[0].SharingID)
	assert.Equal(t, file.ID(), scopes[0].RootID)
}

func TestAccessResolver_FileTargetInSharedDir(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"file.txt": nil}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	file, err := fs.FileByPath(parent.Fullpath + "/file.txt")
	require.NoError(t, err)

	s := createActiveDirSharing(t, inst, parent.ID())

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(file.ID())
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, s.SID, scopes[0].SharingID)
	assert.Equal(t, parent.ID(), scopes[0].RootID)
}

func TestAccessResolver_FileTargetAdditive(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"file.txt": nil}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	file, err := fs.FileByPath(parent.Fullpath + "/file.txt")
	require.NoError(t, err)

	sParent := createActiveDirSharing(t, inst, parent.ID())
	sFile := createActiveFileSharing(t, inst, file.ID())

	resolver := NewAccessResolver(inst)
	scopes, err := resolver.scopesFor(file.ID())
	require.NoError(t, err)
	require.Len(t, scopes, 2)
	got := map[string]SharingScope{}
	for _, sc := range scopes {
		got[sc.SharingID] = sc
	}
	assert.Contains(t, got, sParent.SID)
	assert.Contains(t, got, sFile.SID)
	assert.Equal(t, file.ID(), got[sFile.SID].RootID)
	assert.Equal(t, parent.ID(), got[sParent.SID].RootID)
}

func TestAccessResolver_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	resolver := NewAccessResolver(inst)
	ea, err := resolver.Resolve("does-not-exist")
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
	assert.Nil(t, ea)
}

func TestAccessResolver_NearestRestrictiveBoundaryStub(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	resolver := NewAccessResolver(inst)
	b, err := resolver.NearestRestrictiveBoundary("whatever")
	require.NoError(t, err)
	assert.Nil(t, b)

	roots, err := resolver.ChildSharedRootsUnder("whatever")
	require.NoError(t, err)
	assert.Nil(t, roots)
}

func TestAccessResolver_ReadOnlyRecipient(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	createActiveRecipientSharing(t, inst, parent.ID(), true)

	resolver := NewAccessResolver(inst)
	ea, err := resolver.Resolve(child.ID())
	require.NoError(t, err)
	assert.True(t, ea.CanRead)
	assert.False(t, ea.CanWrite)
	assert.True(t, ea.Can(permission.GET))
	assert.False(t, ea.Can(permission.PUT))
}

func TestAccessResolver_HighestChildScopeWins(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	createActiveRecipientSharing(t, inst, parent.ID(), true)
	createActiveRecipientSharing(t, inst, child.ID(), false)

	resolver := NewAccessResolver(inst)
	ea, err := resolver.Resolve(child.ID())
	require.NoError(t, err)
	assert.True(t, ea.CanRead)
	assert.True(t, ea.CanWrite)
	assert.True(t, ea.Can(permission.GET))
	assert.True(t, ea.Can(permission.PUT))
	assert.Len(t, ea.SourceSharingIDs, 2)
}

func TestAccessResolver_HighestParentScopeWins(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	createActiveRecipientSharing(t, inst, parent.ID(), false)
	createActiveRecipientSharing(t, inst, child.ID(), true)

	resolver := NewAccessResolver(inst)
	ea, err := resolver.Resolve(child.ID())
	require.NoError(t, err)
	assert.True(t, ea.CanRead)
	assert.True(t, ea.CanWrite)
	assert.True(t, ea.Can(permission.GET))
	assert.True(t, ea.Can(permission.PUT))
	assert.Len(t, ea.SourceSharingIDs, 2)
}

func TestAccessResolver_NotAMember(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	createActiveRecipientSharing(t, inst, parent.ID(), false, "https://someone-else.cozy.tools")

	resolver := NewAccessResolver(inst)
	ea, err := resolver.Resolve(child.ID())
	require.NoError(t, err)
	assert.False(t, ea.CanRead)
	assert.False(t, ea.CanWrite)
	assert.False(t, ea.Can(permission.GET))
	assert.Empty(t, ea.SourceSharingIDs)
}

func TestAccessResolver_RevokedMemberSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)

	s := createActiveRecipientSharing(t, inst, parent.ID(), false)
	s.Members[1].Status = MemberStatusRevoked
	require.NoError(t, couchdb.UpdateDoc(inst, s))

	resolver := NewAccessResolver(inst)
	ea, err := resolver.Resolve(child.ID())
	require.NoError(t, err)
	assert.False(t, ea.CanRead)
	assert.False(t, ea.CanWrite)
	assert.Empty(t, ea.SourceSharingIDs)
}

// createActiveDirSharing creates and persists an active additive sharing owned
// by the current instance (owner → write access), with a directory root.
func createActiveDirSharing(t *testing.T, inst *instance.Instance, rootID string) *Sharing {
	t.Helper()
	return createSharing(t, inst, rootID, true, false, DriveRootTypeDirectory, "")
}

// createActiveFileSharing creates and persists an active additive sharing
// owned by the current instance, with a single-file root.
func createActiveFileSharing(t *testing.T, inst *instance.Instance, rootID string) *Sharing {
	t.Helper()
	return createSharing(t, inst, rootID, true, false, DriveRootTypeFile, "")
}

// createActiveRecipientSharing creates and persists an active additive sharing
// where the current instance is a recipient (not the owner). readOnly sets the
// recipient's ReadOnly flag. When memberInstance is empty, the member's
// Instance is set to the current instance's domain; otherwise the given
// memberInstance is used (to simulate a non-member).
func createActiveRecipientSharing(t *testing.T, inst *instance.Instance, rootID string, readOnly bool, memberInstance ...string) *Sharing {
	t.Helper()
	mi := ""
	if len(memberInstance) > 0 {
		mi = memberInstance[0]
	}
	return createSharing(t, inst, rootID, false, readOnly, DriveRootTypeDirectory, mi)
}

func createSharing(t *testing.T, inst *instance.Instance, rootID string, owner bool, readOnly bool, rootType string, recipientInstance string) *Sharing {
	t.Helper()
	now := time.Now()

	var members []Member
	if owner {
		name, err := inst.SettingsPublicName()
		require.NoError(t, err)
		email, err := inst.SettingsEMail()
		require.NoError(t, err)

		members = []Member{
			{
				Status:   MemberStatusOwner,
				Name:     name,
				Email:    email,
				Instance: "https://" + inst.Domain,
			},
		}
	} else {
		instVal := recipientInstance
		if instVal == "" {
			instVal = "https://" + inst.Domain
		}

		members = []Member{
			{
				Status:   MemberStatusOwner,
				Name:     "Alice",
				Email:    "alice@cozy.tools",
				Instance: "https://owner.cozy.tools",
			},
			{
				Status:   MemberStatusReady,
				Name:     inst.Domain,
				Instance: instVal,
				ReadOnly: readOnly,
			},
		}
	}

	s := &Sharing{
		Active:        true,
		Owner:         owner,
		Drive:         true,
		DriveRootType: rootType,
		AppSlug:       "test",
		AccessMode:    AccessModeAdditive,
		Members:       members,
		Rules: []Rule{
			{
				Title:   "test",
				DocType: consts.Files,
				Values:  []string{rootID},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, couchdb.CreateDoc(inst, s))
	require.NoError(t, s.AddReferenceForSharing(inst, &s.Rules[0]))
	return s
}
