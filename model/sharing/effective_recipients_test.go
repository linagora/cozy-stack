package sharing

import (
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEffectiveRecipients_Self(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	s := createActiveDirSharing(t, inst, parent.ID())
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)
	require.Len(t, recipients, 2)

	byEmail := map[string]EffectiveRecipient{}
	for _, r := range recipients {
		byEmail[r.Email] = r
	}

	bob := byEmail["bob@cozy.tools"]
	assert.Equal(t, "Bob", bob.Name)
	assert.Equal(t, "https://bob.cozy.tools", bob.Instance)
	assert.Equal(t, MemberStatusReady, bob.Status)
	assert.False(t, bob.ReadOnly)
	assert.True(t, bob.CanEditHere)
	require.Len(t, bob.Sources, 1)
	assert.Equal(t, s.SID, bob.Sources[0].SharingID)
	assert.Equal(t, parent.ID(), bob.Sources[0].RootID)
	assert.Equal(t, parent.DocName, bob.Sources[0].RootName)
	assert.Equal(t, "self", bob.Sources[0].Kind)
	assert.Equal(t, 1, bob.Sources[0].MemberIndex)
	assert.True(t, bob.Sources[0].Manageable)

	email, err := inst.SettingsEMail()
	require.NoError(t, err)
	owner := byEmail[email]
	assert.Equal(t, MemberStatusOwner, owner.Status)
	assert.True(t, owner.CanEditHere)
	// The owner cannot revoke themselves (RevokeRecipient rejects index 0).
	assert.False(t, owner.Sources[0].Manageable)
}

func TestEffectiveRecipients_Ancestor(t *testing.T) {
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
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
		ReadOnly: true,
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(child.ID(), nil)
	require.NoError(t, err)
	require.Len(t, recipients, 2)

	var bob *EffectiveRecipient
	for i := range recipients {
		if recipients[i].Email == "bob@cozy.tools" {
			bob = &recipients[i]
		}
	}
	require.NotNil(t, bob)
	assert.True(t, bob.ReadOnly)
	assert.False(t, bob.CanEditHere)
	require.Len(t, bob.Sources, 1)
	assert.Equal(t, s.SID, bob.Sources[0].SharingID)
	assert.Equal(t, parent.ID(), bob.Sources[0].RootID)
	assert.Equal(t, parent.DocName, bob.Sources[0].RootName)
	assert.Equal(t, "ancestor", bob.Sources[0].Kind)
	assert.False(t, bob.Sources[0].Manageable)
}

func TestEffectiveRecipients_DedupReadWriteWins(t *testing.T) {
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
	addMemberToSharing(t, inst, sParent, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
		ReadOnly: true,
	})
	sChild := createActiveDirSharing(t, inst, child.ID())
	addMemberToSharing(t, inst, sChild, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
		ReadOnly: false,
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(child.ID(), nil)
	require.NoError(t, err)

	var bobs []EffectiveRecipient
	for _, r := range recipients {
		if r.Instance == "https://bob.cozy.tools" {
			bobs = append(bobs, r)
		}
	}
	require.Len(t, bobs, 1)
	bob := bobs[0]
	assert.False(t, bob.ReadOnly) // read-write wins over read-only
	assert.True(t, bob.CanEditHere)
	assert.Len(t, bob.Sources, 2)
}

func TestEffectiveRecipients_DedupByEmailFallback(t *testing.T) {
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
	addMemberToSharing(t, inst, sParent, Member{
		Status: MemberStatusMailNotSent,
		Name:   "Bob",
		Email:  "Bob@Cozy.Tools",
	})
	sChild := createActiveDirSharing(t, inst, child.ID())
	addMemberToSharing(t, inst, sChild, Member{
		Status: MemberStatusMailNotSent,
		Name:   "Bob",
		Email:  "bob@cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(child.ID(), nil)
	require.NoError(t, err)

	var bobs []EffectiveRecipient
	for _, r := range recipients {
		if r.Name == "Bob" {
			bobs = append(bobs, r)
		}
	}
	require.Len(t, bobs, 1)
	assert.Len(t, bobs[0].Sources, 2)
}

func TestEffectiveRecipients_DedupInstanceThenEmail(t *testing.T) {
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

	// Bob has accepted the parent share (instance known)…
	sParent := createActiveDirSharing(t, inst, parent.ID())
	addMemberToSharing(t, inst, sParent, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})
	// …but was only invited by email to the child share (no instance yet).
	sChild := createActiveDirSharing(t, inst, child.ID())
	addMemberToSharing(t, inst, sChild, Member{
		Status: MemberStatusMailNotSent,
		Name:   "Bob",
		Email:  "bob@cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(child.ID(), nil)
	require.NoError(t, err)

	var bobs []EffectiveRecipient
	for _, r := range recipients {
		if r.Email == "bob@cozy.tools" {
			bobs = append(bobs, r)
		}
	}
	require.Len(t, bobs, 1)
	assert.Equal(t, "https://bob.cozy.tools", bobs[0].Instance)
	assert.Len(t, bobs[0].Sources, 2)
}

func TestEffectiveRecipients_DedupTransitiveBridge(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	// The same person appears with instance only, then email only, then both:
	// the third occurrence bridges the first two and must merge all three
	// into a single recipient instead of orphaning the email-only one.
	// Members of one sharing are processed in order and sharings are
	// processed in sorted ID order, so this is deterministic.
	s := createActiveDirSharing(t, inst, parent.ID())
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Instance: "https://bob.cozy.tools",
	})
	addMemberToSharing(t, inst, s, Member{
		Status: MemberStatusMailNotSent,
		Name:   "Bob",
		Email:  "bob@cozy.tools",
	})
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)

	var bobs []EffectiveRecipient
	for _, r := range recipients {
		if r.Name == "Bob" || r.Email == "bob@cozy.tools" {
			bobs = append(bobs, r)
		}
	}
	require.Len(t, bobs, 1)
	assert.Equal(t, "bob@cozy.tools", bobs[0].Email)
	assert.Equal(t, "https://bob.cozy.tools", bobs[0].Instance)
	assert.Len(t, bobs[0].Sources, 3)
}

func TestEffectiveRecipients_MergePrefersAdvancedStatus(t *testing.T) {
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
	addMemberToSharing(t, inst, sParent, Member{
		Status:   MemberStatusMailNotSent,
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})
	sChild := createActiveDirSharing(t, inst, child.ID())
	addMemberToSharing(t, inst, sChild, Member{
		Status:   MemberStatusReady,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(child.ID(), nil)
	require.NoError(t, err)

	var bobs []EffectiveRecipient
	for _, r := range recipients {
		if r.Instance == "https://bob.cozy.tools" {
			bobs = append(bobs, r)
		}
	}
	require.Len(t, bobs, 1)
	assert.Equal(t, MemberStatusReady, bobs[0].Status)
	assert.Equal(t, "Bob", bobs[0].Name) // filled from the occurrence that has it
}

func TestEffectiveRecipients_AbsorbKeepsEmailFallbackName(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	// Bob is known by email+instance (no name: PrimaryName falls back to
	// the email), then by instance only. The second occurrence merges into
	// the first via the instance key and must not erase the email fallback
	// name with its own empty name. Members of one sharing are processed
	// in order, so this is deterministic.
	s := createActiveDirSharing(t, inst, parent.ID())
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusReady,
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusReady,
		Instance: "https://bob.cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)

	var bobs []EffectiveRecipient
	for _, r := range recipients {
		if r.Instance == "https://bob.cozy.tools" {
			bobs = append(bobs, r)
		}
	}
	require.Len(t, bobs, 1)
	assert.Equal(t, "bob@cozy.tools", bobs[0].Name) // email fallback preserved
	assert.Equal(t, "bob@cozy.tools", bobs[0].Email)
	assert.Len(t, bobs[0].Sources, 2)
}

func TestEffectiveRecipients_RevokedExcluded(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	s := createActiveDirSharing(t, inst, parent.ID())
	addMemberToSharing(t, inst, s, Member{
		Status:   MemberStatusRevoked,
		Name:     "Bob",
		Email:    "bob@cozy.tools",
		Instance: "https://bob.cozy.tools",
	})

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)
	for _, r := range recipients {
		assert.NotEqual(t, "bob@cozy.tools", r.Email)
	}
}

func TestEffectiveRecipients_NotShared(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)
	assert.Empty(t, recipients)
}

func TestEffectiveRecipients_NotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	recipients, err := NewAccessResolver(inst).EffectiveRecipients("does-not-exist", nil)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
	assert.Nil(t, recipients)
}

// On a recipient instance of a drive, member management is delegated to the
// owner (AddRecipients, RevokeRecipient), so a "self" source stays
// manageable — same condition as authorizeRevokeRecipient.
func TestEffectiveRecipients_ManageableDriveRecipient(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	createActiveRecipientSharing(t, inst, parent.ID(), false)

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, recipients)

	for _, r := range recipients {
		require.Len(t, r.Sources, 1)
		assert.Equal(t, "self", r.Sources[0].Kind)
		if r.Sources[0].MemberIndex == 0 {
			// The owner cannot revoke themselves (RevokeRecipient
			// rejects index 0).
			assert.False(t, r.Sources[0].Manageable)
		} else {
			assert.True(t, r.Sources[0].Manageable)
		}
		assert.True(t, r.CanEditHere)
	}
}

// A read-only drive recipient cannot manage members: authorizeRevokeRecipient
// requires write access on the sharing for the delegated revoke path, so the
// "self" sources must not be flagged as manageable.
func TestEffectiveRecipients_NotManageableDriveRecipientReadOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()
	fs := inst.VFS()

	tree := H{"parent/": H{}}
	parent := createTree(t, fs, tree, consts.RootDirID)

	createActiveRecipientSharing(t, inst, parent.ID(), true)

	recipients, err := NewAccessResolver(inst).EffectiveRecipients(parent.ID(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, recipients)

	for _, r := range recipients {
		require.Len(t, r.Sources, 1)
		assert.Equal(t, "self", r.Sources[0].Kind)
		assert.False(t, r.Sources[0].Manageable)
		assert.True(t, r.CanEditHere)
	}
}

// addMemberToSharing appends a member to a persisted sharing document.
func addMemberToSharing(t *testing.T, inst *instance.Instance, s *Sharing, m Member) {
	t.Helper()
	s.Members = append(s.Members, m)
	require.NoError(t, couchdb.UpdateDoc(inst, s))
}

func TestMemberReadsRootUsesTheLoadedScopes(t *testing.T) {
	ready := func(email string) Member { return Member{Email: email, Status: MemberStatusReady} }
	parent := &Sharing{SID: "parent", Members: []Member{ready("owner@example.net"), ready("dave@example.net")}}
	nested := &Sharing{SID: "nested", Members: []Member{ready("owner@example.net"), ready("betty@example.net")}}
	sharings := []*Sharing{parent, nested}
	roots := map[string]rootInfo{
		"parent": {RootID: "p", RootPath: "/Team"},
		"nested": {RootID: "n", RootPath: "/Team/Clients"},
	}
	betty := &Member{Email: "betty@example.net"}
	dave := &Member{Email: "dave@example.net"}

	assert.True(t, memberReadsRoot(sharings, roots, "/Team/Clients", betty))
	assert.False(t, memberReadsRoot(sharings, roots, "/Team", betty))
	assert.True(t, memberReadsRoot(sharings, roots, "/Team", dave))
	assert.True(t, memberReadsRoot(sharings, roots, "/Team/Clients", dave))
	assert.False(t, memberReadsRoot(sharings, roots, "/Team", &Member{Email: "eve@example.net"}))
}
