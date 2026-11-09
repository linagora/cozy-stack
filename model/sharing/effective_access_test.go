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

func TestAccessResolver_DriveForMember(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	daveInstance := "https://dave.example.net"
	otherInstance := "https://other.example.net"
	dave := &Member{Email: "dave@example.net", Instance: daveInstance}
	resolver := NewAccessResolver(inst)

	// Every subtest has its own tree: /<name>/child/nested.txt and
	// /<name>/top.txt, where the sharings of X are on /<name> and those of Y
	// on /<name>/child.
	newTree := func(t *testing.T, name string) (x, y *vfs.DirDoc, nested, top *vfs.FileDoc) {
		t.Helper()
		tree := H{name + "/": H{"child/": H{"nested.txt": H{}}, "top.txt": H{}}}
		x = createTree(t, fs, tree, consts.RootDirID)
		var err error
		y, err = fs.DirByPath(x.Fullpath + "/child")
		require.NoError(t, err)
		nested, err = fs.FileByPath(y.Fullpath + "/nested.txt")
		require.NoError(t, err)
		top, err = fs.FileByPath(x.Fullpath + "/top.txt")
		require.NoError(t, err)
		return x, y, nested, top
	}
	// A drive where Dave is not a member has another ready member instead.
	shareWith := func(t *testing.T, rootID, role string) *Sharing {
		t.Helper()
		switch role {
		case "writer":
			return createActiveRecipientSharing(t, inst, rootID, false, daveInstance)
		case "reader":
			return createActiveRecipientSharing(t, inst, rootID, true, daveInstance)
		default:
			return createActiveRecipientSharing(t, inst, rootID, false, otherInstance)
		}
	}
	makeClassic := func(t *testing.T, s *Sharing) {
		t.Helper()
		s.Drive = false
		require.NoError(t, couchdb.UpdateDoc(inst, s))
	}

	rules := []struct {
		name, x, y, want string
	}{
		{"WriterOnBothGivesOuter", "writer", "writer", "x"},
		{"ReaderOnOuterWriterOnNestedGivesNested", "reader", "writer", "y"},
		{"WriterOnOuterReaderOnNestedGivesOuter", "writer", "reader", "x"},
		{"ReaderOnBothGivesOuter", "reader", "reader", "x"},
		{"MemberOfNestedOnlyGivesNested", "none", "writer", "y"},
		{"MemberOfOuterOnlyGivesOuter", "writer", "none", "x"},
	}
	for _, tt := range rules {
		t.Run(tt.name, func(t *testing.T) {
			x, y, nested, _ := newTree(t, tt.name)
			sX := shareWith(t, x.ID(), tt.x)
			sY := shareWith(t, y.ID(), tt.y)

			wantSharing, wantRoot := sX, x
			if tt.want == "y" {
				wantSharing, wantRoot = sY, y
			}
			sc, err := resolver.DriveForMember(nested.ID(), dave)
			require.NoError(t, err)
			require.NotNil(t, sc)
			assert.Equal(t, wantSharing.SID, sc.Sharing.SID)
			assert.Equal(t, wantRoot.ID(), sc.RootID)
			assert.Equal(t, wantRoot.Fullpath, sc.RootPath)
			// The member entry is the one inside the returned sharing.
			assert.Same(t, &sc.Sharing.Members[1], sc.Member)
		})
	}

	t.Run("OutsideTheOnlyDriveOfTheMember", func(t *testing.T) {
		x, y, _, top := newTree(t, "OutsideNested")
		shareWith(t, x.ID(), "none")
		shareWith(t, y.ID(), "writer")

		sc, err := resolver.DriveForMember(top.ID(), dave)
		require.NoError(t, err)
		assert.Nil(t, sc)
	})

	t.Run("NoSharing", func(t *testing.T) {
		_, _, nested, _ := newTree(t, "NoSharing")

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		assert.Nil(t, sc)
	})

	t.Run("ClassicSharingAlone", func(t *testing.T) {
		x, _, nested, _ := newTree(t, "ClassicAlone")
		makeClassic(t, shareWith(t, x.ID(), "writer"))

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		assert.Nil(t, sc)
	})

	t.Run("ClassicWritableNestedInDriveWhereMemberIsReader", func(t *testing.T) {
		x, y, nested, _ := newTree(t, "ClassicNested")
		sDrive := shareWith(t, x.ID(), "reader")
		makeClassic(t, shareWith(t, y.ID(), "writer"))

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, sDrive.SID, sc.Sharing.SID)
		assert.True(t, sc.Member.ReadOnly)
	})

	for _, status := range []string{MemberStatusPendingInvitation, MemberStatusRevoked} {
		t.Run("MemberNotReady/"+status, func(t *testing.T) {
			x, _, nested, _ := newTree(t, "NotReady-"+status)
			s := shareWith(t, x.ID(), "writer")
			s.Members[1].Status = status
			require.NoError(t, couchdb.UpdateDoc(inst, s))

			sc, err := resolver.DriveForMember(nested.ID(), dave)
			require.NoError(t, err)
			assert.Nil(t, sc)
		})
	}

	t.Run("NilMember", func(t *testing.T) {
		x, _, nested, _ := newTree(t, "NilMember")
		shareWith(t, x.ID(), "writer")

		sc, err := resolver.DriveForMember(nested.ID(), nil)
		require.NoError(t, err)
		assert.Nil(t, sc)
	})

	t.Run("ResolverError", func(t *testing.T) {
		sc, err := resolver.DriveForMember("no-such-id", dave)
		assert.True(t, os.IsNotExist(err), "unexpected error: %v", err)
		assert.Nil(t, sc)
	})

	t.Run("SameRootGivesSmallestSharingID", func(t *testing.T) {
		x, _, nested, _ := newTree(t, "SameRoot")
		s1 := shareWith(t, x.ID(), "writer")
		s2 := shareWith(t, x.ID(), "writer")
		want := min(s1.SID, s2.SID)

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, want, sc.Sharing.SID)
	})

	t.Run("PathOfDirectoryRootDrive", func(t *testing.T) {
		x, y, nested, top := newTree(t, "PathDirRoot")
		shareWith(t, x.ID(), "writer")
		nestedPath, err := nested.Path(fs)
		require.NoError(t, err)
		topPath, err := top.Path(fs)
		require.NoError(t, err)

		targets := []struct {
			id, fullpath, want string
		}{
			{x.ID(), x.Fullpath, ""},
			{y.ID(), y.Fullpath, "/child"},
			{top.ID(), topPath, "/top.txt"},
			{nested.ID(), nestedPath, "/child/nested.txt"},
		}
		for _, target := range targets {
			sc, err := resolver.DriveForMember(target.id, dave)
			require.NoError(t, err)
			require.NotNil(t, sc)
			got, err := sc.PathFromRoot(target.fullpath)
			require.NoError(t, err)
			assert.Equal(t, target.want, got)
		}
	})

	t.Run("PathIsRelativeToTheReturnedDrive", func(t *testing.T) {
		x, y, nested, _ := newTree(t, "PathNested")
		shareWith(t, x.ID(), "reader")
		shareWith(t, y.ID(), "writer")
		nestedPath, err := nested.Path(fs)
		require.NoError(t, err)

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		got, err := sc.PathFromRoot(nestedPath)
		require.NoError(t, err)
		assert.Equal(t, "/nested.txt", got)
	})

	t.Run("FileRootDrive", func(t *testing.T) {
		_, _, nested, _ := newTree(t, "FileRoot")
		s := createSharing(t, inst, nested.ID(), false, false, DriveRootTypeFile, daveInstance)
		nestedPath, err := nested.Path(fs)
		require.NoError(t, err)

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, s.SID, sc.Sharing.SID)
		assert.Equal(t, nested.ID(), sc.RootID)
		assert.Equal(t, nestedPath, sc.RootPath)
		got, err := sc.PathFromRoot(nestedPath)
		require.NoError(t, err)
		assert.Empty(t, got)
	})

	t.Run("FileRootDriveDoesNotCoverItsSiblings", func(t *testing.T) {
		_, _, nested, top := newTree(t, "FileRootSiblings")
		createSharing(t, inst, nested.ID(), false, false, DriveRootTypeFile, daveInstance)

		sc, err := resolver.DriveForMember(top.ID(), dave)
		require.NoError(t, err)
		assert.Nil(t, sc)
	})

	t.Run("FileRootDriveNestedInDriveWhereMemberIsReader", func(t *testing.T) {
		x, _, nested, _ := newTree(t, "FileRootInReader")
		shareWith(t, x.ID(), "reader")
		s := createSharing(t, inst, nested.ID(), false, false, DriveRootTypeFile, daveInstance)

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, s.SID, sc.Sharing.SID)
	})

	t.Run("FileRootDriveNestedInDriveWhereMemberIsWriter", func(t *testing.T) {
		x, _, nested, _ := newTree(t, "FileRootInWriter")
		sDrive := shareWith(t, x.ID(), "writer")
		createSharing(t, inst, nested.ID(), false, false, DriveRootTypeFile, daveInstance)
		nestedPath, err := nested.Path(fs)
		require.NoError(t, err)

		sc, err := resolver.DriveForMember(nested.ID(), dave)
		require.NoError(t, err)
		require.NotNil(t, sc)
		assert.Equal(t, sDrive.SID, sc.Sharing.SID)
		got, err := sc.PathFromRoot(nestedPath)
		require.NoError(t, err)
		assert.Equal(t, "/child/nested.txt", got)
	})
}

// compareDriveScopes is tested on its own because the sharings come sorted by
// ID from the resolver: the tie-break cannot fail through DriveForMember.
func TestAccessResolver_CompareDriveScopes(t *testing.T) {
	scope := func(sid, rootPath string, readOnly bool) SharingScope {
		return SharingScope{
			Sharing:  &Sharing{SID: sid},
			Member:   &Member{ReadOnly: readOnly},
			RootPath: rootPath,
		}
	}
	tests := []struct {
		name          string
		better, worse SharingScope
	}{
		{"WriterBeforeOuterReader", scope("b", "/x/y", false), scope("a", "/x", true)},
		{"OuterBeforeNestedSameRights", scope("b", "/x", false), scope("a", "/x/y", false)},
		{"OuterBeforeNestedAsReader", scope("b", "/x", true), scope("a", "/x/y", true)},
		{"SmallestIDOnSameRoot", scope("a", "/x", false), scope("b", "/x", false)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Negative(t, compareDriveScopes(tt.better, tt.worse))
			assert.Positive(t, compareDriveScopes(tt.worse, tt.better))
		})
	}
}

func TestAccessResolver_PathFromRoot(t *testing.T) {
	tests := []struct {
		name, rootPath, fullpath, want string
		wantErr                        bool
	}{
		{name: "RootItself", rootPath: "/Team", fullpath: "/Team", want: ""},
		{name: "FileAtRoot", rootPath: "/Team", fullpath: "/Team/a.txt", want: "/a.txt"},
		{name: "FileInSubfolder", rootPath: "/Team", fullpath: "/Team/sub/a.txt", want: "/sub/a.txt"},
		{name: "Subfolder", rootPath: "/Team", fullpath: "/Team/sub", want: "/sub"},
		{name: "RootNotAtTopLevel", rootPath: "/Perso/Team", fullpath: "/Perso/Team/sub/a.txt", want: "/sub/a.txt"},
		{name: "FileRoot", rootPath: "/Perso/report.pdf", fullpath: "/Perso/report.pdf", want: ""},
		{name: "SiblingWithSamePrefix", rootPath: "/Team", fullpath: "/Team2/a.txt", wantErr: true},
		{name: "ParentOfRoot", rootPath: "/Perso/Team", fullpath: "/Perso", wantErr: true},
		{name: "Unrelated", rootPath: "/Team", fullpath: "/Other/a.txt", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := &SharingScope{RootPath: tt.rootPath}
			got, err := sc.PathFromRoot(tt.fullpath)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
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
