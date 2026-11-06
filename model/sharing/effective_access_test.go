package sharing

import (
	"os"
	"testing"
	"time"

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
	scopes, err := resolver.scopesMatching(child.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, s.SID, scopes[0].Sharing.SID)
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
	scopes, err := resolver.scopesMatching(child.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
	require.NoError(t, err)
	require.Len(t, scopes, 2)
	got := map[string]SharingScope{}
	for _, sc := range scopes {
		got[sc.Sharing.SID] = sc
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
	scopes, err := resolver.scopesMatching(child.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
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
	scopes, err := resolver.scopesMatching(child.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
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
	scopes, err := resolver.scopesMatching(file.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, s.SID, scopes[0].Sharing.SID)
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
	scopes, err := resolver.scopesMatching(file.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
	require.NoError(t, err)
	require.Len(t, scopes, 1)
	assert.Equal(t, s.SID, scopes[0].Sharing.SID)
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
	scopes, err := resolver.scopesMatching(file.ID(), func(s *Sharing) *Member {
		return s.MemberFor(inst)
	})
	require.NoError(t, err)
	require.Len(t, scopes, 2)
	got := map[string]SharingScope{}
	for _, sc := range scopes {
		got[sc.Sharing.SID] = sc
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

func TestAccessResolver_ResolveForMember(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{"nested.txt": H{}}, "top.txt": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)
	nestedFile, err := fs.FileByPath(child.Fullpath + "/nested.txt")
	require.NoError(t, err)
	topFile, err := fs.FileByPath(parent.Fullpath + "/top.txt")
	require.NoError(t, err)

	daveInstance := "https://dave.example.net"
	createActiveRecipientSharing(t, inst, parent.ID(), true, daveInstance)
	createActiveRecipientSharing(t, inst, child.ID(), false, daveInstance)

	resolver := NewAccessResolver(inst)
	dave := &Member{Email: "dave@example.net", Instance: daveInstance}

	t.Run("WriteViaNestedScope", func(t *testing.T) {
		ea, err := resolver.ResolveForMember(nestedFile.ID(), dave)
		require.NoError(t, err)
		assert.True(t, ea.CanRead)
		assert.True(t, ea.CanWrite)
		assert.True(t, ea.Can(permission.GET))
		assert.True(t, ea.Can(permission.PATCH))
		assert.Len(t, ea.SourceSharingIDs, 2)
	})

	t.Run("ReadOnlyViaRootScope", func(t *testing.T) {
		ea, err := resolver.ResolveForMember(topFile.ID(), dave)
		require.NoError(t, err)
		assert.True(t, ea.CanRead)
		assert.False(t, ea.CanWrite)
		assert.False(t, ea.Can(permission.PATCH))
	})

	t.Run("MatchingByInstanceHost", func(t *testing.T) {
		ea, err := resolver.ResolveForMember(topFile.ID(), &Member{Instance: daveInstance})
		require.NoError(t, err)
		assert.True(t, ea.CanRead)
	})

	t.Run("NotAMember", func(t *testing.T) {
		stranger := &Member{Email: "stranger@example.net", Instance: "https://stranger.example.net"}
		ea, err := resolver.ResolveForMember(topFile.ID(), stranger)
		require.NoError(t, err)
		assert.False(t, ea.CanRead)
		assert.False(t, ea.CanWrite)
	})

	t.Run("NilMember", func(t *testing.T) {
		ea, err := resolver.ResolveForMember(topFile.ID(), nil)
		require.NoError(t, err)
		assert.False(t, ea.CanRead)
	})
}

func TestAccessResolver_NearestWritableSharing(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{"nested.txt": H{}}, "top.txt": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)
	nestedFile, err := fs.FileByPath(child.Fullpath + "/nested.txt")
	require.NoError(t, err)
	topFile, err := fs.FileByPath(parent.Fullpath + "/top.txt")
	require.NoError(t, err)

	daveInstance := "https://dave.example.net"
	dave := &Member{Email: "dave@example.net", Instance: daveInstance}
	sParent := createActiveRecipientSharing(t, inst, parent.ID(), false, daveInstance)
	sChild := createActiveRecipientSharing(t, inst, child.ID(), false, daveInstance)

	resolver := NewAccessResolver(inst)

	t.Run("DeepestWritableScopeWins", func(t *testing.T) {
		sc, err := resolver.NearestWritableSharing(nestedFile.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, sChild.SID, sc.Sharing.SID)
		assert.Equal(t, child.ID(), sc.RootID)
		// The member entry is the one inside the returned sharing.
		assert.Same(t, &sc.Sharing.Members[1], sc.Member)
	})

	t.Run("OuterScopeWhenNoNestedOneApplies", func(t *testing.T) {
		sc, err := resolver.NearestWritableSharing(topFile.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, sParent.SID, sc.Sharing.SID)
		assert.Equal(t, parent.ID(), sc.RootID)
	})
}

func TestAccessResolver_NearestWritableSharingSkipsReadOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{"child/": H{"nested.txt": H{}}, "top.txt": H{}}}
	parent := createTree(t, fs, tree, consts.RootDirID)
	child, err := fs.DirByPath(parent.Fullpath + "/child")
	require.NoError(t, err)
	nestedFile, err := fs.FileByPath(child.Fullpath + "/nested.txt")
	require.NoError(t, err)
	topFile, err := fs.FileByPath(parent.Fullpath + "/top.txt")
	require.NoError(t, err)

	daveInstance := "https://dave.example.net"
	dave := &Member{Email: "dave@example.net", Instance: daveInstance}
	createActiveRecipientSharing(t, inst, parent.ID(), true, daveInstance)
	sChild := createActiveRecipientSharing(t, inst, child.ID(), false, daveInstance)

	resolver := NewAccessResolver(inst)

	t.Run("NestedScopeSkipsReadOnlyRoot", func(t *testing.T) {
		sc, err := resolver.NearestWritableSharing(nestedFile.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, sChild.SID, sc.Sharing.SID)
	})

	t.Run("NilWhenOnlyReadOnlyApplies", func(t *testing.T) {
		sc, err := resolver.NearestWritableSharing(topFile.ID(), dave)
		require.NoError(t, err)
		assert.Nil(t, sc)
	})
}

func TestAccessResolver_CrossesDriveBoundary(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	drive := createTree(t, fs, H{"drive/": H{"sub/": H{}, "nested/": H{}}}, consts.RootDirID)
	other := createTree(t, fs, H{"other/": H{}}, consts.RootDirID)
	plain := createTree(t, fs, H{"plain/": H{}}, consts.RootDirID)
	plain2 := createTree(t, fs, H{"plain2/": H{}}, consts.RootDirID)
	classic := createTree(t, fs, H{"classic/": H{}}, consts.RootDirID)
	foreign := createTree(t, fs, H{"foreign/": H{}}, consts.RootDirID)
	sub, err := fs.DirByPath("/drive/sub")
	require.NoError(t, err)
	nested, err := fs.DirByPath("/drive/nested")
	require.NoError(t, err)

	createActiveDirSharing(t, inst, drive.ID())
	createActiveDirSharing(t, inst, nested.ID())
	createActiveDirSharing(t, inst, other.ID())
	sClassic := createActiveDirSharing(t, inst, classic.ID())
	sClassic.Drive = false
	require.NoError(t, couchdb.UpdateDoc(inst, sClassic))
	// A drive the instance is not a member of does not apply to it.
	createActiveRecipientSharing(t, inst, foreign.ID(), false, "https://someone.else.example")

	tests := []struct {
		name     string
		from, to *vfs.DirDoc
		want     bool
	}{
		{"InsideDrive", drive, sub, false},
		{"BetweenPlainFolders", plain, plain2, false},
		{"IntoClassicSharing", plain, classic, false},
		{"IntoForeignDrive", plain, foreign, false},
		{"OutOfDrive", sub, plain, true},
		{"IntoDrive", plain, drive, true},
		{"IntoNestedDrive", sub, nested, true},
		{"OutOfNestedDrive", nested, drive, true},
		{"IntoAnotherDrive", drive, other, true},
	}

	resolver := NewAccessResolver(inst)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crosses, err := resolver.CrossesDriveBoundary(tt.from, tt.to)
			require.NoError(t, err)
			assert.Equal(t, tt.want, crosses)
		})
	}
}

func TestAccessResolver_ResolveDocsForInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	createTree(t, fs, H{"batch/": H{
		"shared/":   H{"sub/": H{"deep.txt": nil}, "own.txt": nil},
		"readonly/": H{"ro.txt": nil},
		"nested/":   H{"inner/": H{}},
		"inactive/": H{"inactive.txt": nil},
		"limited/":  H{"limited.txt": nil},
		"foreign/":  H{"foreign.txt": nil},
		"revoked/":  H{"revoked.txt": nil},
		"owned/":    H{"owned.txt": nil},
		"plain/":    H{"plain.txt": nil},
		"rwparent/": H{"rochild/": H{"rw.txt": nil}},
		"pending/":  H{"pending.txt": nil},
		"seen/":     H{"seen.txt": nil},
	}}, consts.RootDirID)
	dirID := func(p string) string {
		dir, err := fs.DirByPath("/batch/" + p)
		require.NoError(t, err)
		return dir.ID()
	}
	fileID := func(p string) string {
		file, err := fs.FileByPath("/batch/" + p)
		require.NoError(t, err)
		return file.ID()
	}

	daveInstance := "https://dave.example.net"
	dave := &instance.Instance{Domain: "dave.example.net"}
	createActiveRecipientSharing(t, inst, dirID("shared"), false, daveInstance)
	createSharing(t, inst, fileID("shared/own.txt"), false, true, DriveRootTypeFile, daveInstance)
	createActiveRecipientSharing(t, inst, dirID("readonly"), true, daveInstance)
	createActiveRecipientSharing(t, inst, dirID("nested"), true, daveInstance)
	createActiveRecipientSharing(t, inst, dirID("nested/inner"), false, daveInstance)
	sInactive := createActiveRecipientSharing(t, inst, dirID("inactive"), false, daveInstance)
	sInactive.Active = false
	require.NoError(t, couchdb.UpdateDoc(inst, sInactive))
	sLimited := createActiveRecipientSharing(t, inst, dirID("limited"), false, daveInstance)
	sLimited.AccessMode = AccessModeLimitedAccess
	require.NoError(t, couchdb.UpdateDoc(inst, sLimited))
	createActiveRecipientSharing(t, inst, dirID("foreign"), false, "https://someone-else.cozy.tools")
	sRevoked := createActiveRecipientSharing(t, inst, dirID("revoked"), false, daveInstance)
	sRevoked.Members[1].Status = MemberStatusRevoked
	require.NoError(t, couchdb.UpdateDoc(inst, sRevoked))
	createActiveDirSharing(t, inst, dirID("owned"))
	createActiveRecipientSharing(t, inst, dirID("rwparent"), false, daveInstance)
	createActiveRecipientSharing(t, inst, dirID("rwparent/rochild"), true, daveInstance)
	for _, status := range []string{MemberStatusPendingInvitation, MemberStatusSeen} {
		s := createActiveRecipientSharing(t, inst, dirID(status), false, daveInstance)
		s.Members[1].Status = status
		require.NoError(t, couchdb.UpdateDoc(inst, s))
	}

	// The targets are loaded after the sharings, which add references to
	// their roots, and by ID, so that their paths are not already known. Each
	// subtest loads them again, as a computed path is kept on the doc.
	ids := []string{
		dirID("shared/sub"),
		fileID("shared/sub/deep.txt"),
		fileID("shared/own.txt"),
		fileID("readonly/ro.txt"),
		dirID("nested/inner"),
		fileID("inactive/inactive.txt"),
		fileID("limited/limited.txt"),
		fileID("foreign/foreign.txt"),
		fileID("revoked/revoked.txt"),
		fileID("owned/owned.txt"),
		dirID("plain"),
		fileID("plain/plain.txt"),
		fileID("rwparent/rochild/rw.txt"),
		fileID("pending/pending.txt"),
		fileID("seen/seen.txt"),
	}
	loadTargets := func(t *testing.T, ids []string) []AccessTarget {
		t.Helper()
		targets := make([]AccessTarget, len(ids))
		for i, id := range ids {
			dir, file, err := fs.DirOrFileByID(id)
			require.NoError(t, err)
			targets[i] = AccessTarget{Dir: dir, File: file}
		}
		return targets
	}

	resolver := NewAccessResolver(inst)

	t.Run("SameAsResolveForMember", func(t *testing.T) {
		results, err := resolver.ResolveDocsForInstance(loadTargets(t, ids), dave)
		require.NoError(t, err)
		require.Len(t, results, len(ids))
		for i, id := range ids {
			want, err := resolver.ResolveForMember(id, &Member{Instance: daveInstance})
			require.NoError(t, err)
			assert.NoError(t, results[i].Err, id)
			assert.Equal(t, want, results[i].Access, id)
		}

		deep := results[1].Access
		assert.True(t, deep.CanRead)
		assert.True(t, deep.CanWrite)
		own := results[2].Access
		assert.True(t, own.CanWrite)
		assert.Len(t, own.SourceSharingIDs, 2)
		ro := results[3].Access
		assert.True(t, ro.CanRead)
		assert.False(t, ro.CanWrite)
		inner := results[4].Access
		assert.True(t, inner.CanWrite)
		assert.Len(t, inner.SourceSharingIDs, 2)
		rw := results[12].Access
		assert.True(t, rw.CanWrite, "the highest scope of the parents wins")
		assert.Len(t, rw.SourceSharingIDs, 2)
		for _, i := range []int{5, 6, 7, 8, 9, 10, 11, 13, 14} {
			assert.False(t, results[i].Access.CanRead, ids[i])
		}
	})

	t.Run("MissingParent", func(t *testing.T) {
		orphan := &vfs.FileDoc{Type: consts.FileType, DocID: "orphan", DocName: "orphan.txt", DirID: "no-such-dir"}
		underFile := &vfs.FileDoc{Type: consts.FileType, DocID: "under-file", DocName: "child.txt", DirID: fileID("plain/plain.txt")}
		results, err := resolver.ResolveDocsForInstance([]AccessTarget{
			{File: orphan},
			loadTargets(t, ids[1:2])[0],
			{File: underFile},
		}, dave)
		require.NoError(t, err)
		require.Len(t, results, 3)
		assert.ErrorIs(t, results[0].Err, vfs.ErrParentDoesNotExist)
		assert.Nil(t, results[0].Access)
		assert.NoError(t, results[1].Err)
		assert.True(t, results[1].Access.CanWrite)
		assert.ErrorIs(t, results[2].Err, vfs.ErrParentDoesNotExist)
	})

	t.Run("MatchingByDomainAlias", func(t *testing.T) {
		moved := &instance.Instance{Domain: "dave.example.org", DomainAliases: []string{"dave.example.net"}}
		results, err := resolver.ResolveDocsForInstance(loadTargets(t, ids[:2]), moved)
		require.NoError(t, err)
		assert.True(t, results[0].Access.CanRead)
		assert.True(t, results[1].Access.CanWrite)
	})

	t.Run("NoMatchingByEmail", func(t *testing.T) {
		emailDir := createTree(t, fs, H{"email/": H{"email.txt": nil}}, consts.RootDirID)
		s := createActiveRecipientSharing(t, inst, emailDir.ID(), false, "https://other.example.net")
		s.Members[1].Email = "dave@example.net"
		require.NoError(t, couchdb.UpdateDoc(inst, s))
		file, err := fs.FileByPath("/email/email.txt")
		require.NoError(t, err)

		ea, err := resolver.ResolveForMember(file.ID(), &Member{Email: "dave@example.net", Instance: daveInstance})
		require.NoError(t, err)
		assert.True(t, ea.CanRead)

		results, err := resolver.ResolveDocsForInstance([]AccessTarget{{File: file}}, dave)
		require.NoError(t, err)
		assert.False(t, results[0].Access.CanRead)
	})
}
