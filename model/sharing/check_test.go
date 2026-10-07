package sharing

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTwoTestInstances(t *testing.T) (*instance.Instance, *instance.Instance) {
	t.Helper()
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	setupOwner := testutils.NewSetup(t, t.Name()+"-owner")
	ownerInst := setupOwner.GetTestInstance()
	setupRecipient := testutils.NewSetup(t, t.Name()+"-recipient")
	recipientInst := setupRecipient.GetTestInstance()

	require.NoError(t, couchdb.ResetDB(ownerInst, consts.Sharings))
	require.NoError(t, couchdb.ResetDB(recipientInst, consts.Sharings))
	require.NoError(t, couchdb.ResetDB(ownerInst, consts.Permissions))
	require.NoError(t, couchdb.ResetDB(recipientInst, consts.Permissions))

	return ownerInst, recipientInst
}

func TestCheckDriveOwnerRootInvalidRoot(t *testing.T) {
	s := &Sharing{SID: "sharing-id", Drive: true}

	checks := s.checkDriveOwnerRoot(nil)

	require.Len(t, checks, 1)
	assert.Equal(t, s.SID, checks[0]["id"])
	assert.Equal(t, "invalid_drive_root", checks[0]["type"])
	assert.Equal(t, ErrDriveRootNotFound.Error(), checks[0]["error"])
}

func TestCheckSharingsSharedDrive_Success(t *testing.T) {
	ownerInst, recipientInst := setupTwoTestInstances(t)

	// Create root directory on owner
	rootDir, err := vfs.Mkdir(ownerInst.VFS(), "/Projects", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	sharingID := uuidv7()

	ownerSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{rootDir.DocID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-1",
				XorKey: MakeXorKey(),
			},
		},
	}

	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, ownerSharing))
	require.NoError(t, ownerSharing.AddReferenceForSharing(ownerInst, &ownerSharing.Rules[0]))

	driveToken, err := ownerSharing.GetInteractCode(ownerInst, &ownerSharing.Members[1], 1)
	require.NoError(t, err)
	require.NotEmpty(t, driveToken)

	recipientSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{XorID(rootDir.DocID, ownerSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: driveToken,
				State:      "state-1",
			},
		},
	}
	require.NoError(t, recipientSharing.CreateDriveShortcut(recipientInst, true))
	require.NotEmpty(t, recipientSharing.ShortcutID)
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, recipientSharing))

	checks, err := CheckSharings(ownerInst, false)
	require.NoError(t, err)
	assert.Empty(t, checks, "expected zero consistency errors for healthy shared drive")

	checksFast, err := CheckSharings(ownerInst, true)
	require.NoError(t, err)
	assert.Empty(t, checksFast)
}

func TestCheckSharingsSharedDrive_MissingRootOnOwner(t *testing.T) {
	ownerInst, recipientInst := setupTwoTestInstances(t)

	now := time.Now().UTC()
	sharingID := uuidv7()
	missingRootDirID := uuidv7()

	ownerSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{missingRootDirID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-1",
				XorKey: MakeXorKey(),
			},
		},
	}

	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, ownerSharing))

	recipientSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{XorID(missingRootDirID, ownerSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: "dummy-token",
				State:      "state-1",
			},
		},
	}
	require.NoError(t, recipientSharing.CreateDriveShortcut(recipientInst, true))
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, recipientSharing))

	checks, err := CheckSharings(ownerInst, false)
	require.NoError(t, err)
	require.Len(t, checks, 1)
	assert.Equal(t, "missing_matching_root_doc", checks[0]["type"])
}

func TestCheckSharingsSharedDrive_TrashedRootOnOwner(t *testing.T) {
	ownerInst, recipientInst := setupTwoTestInstances(t)

	rootDir, err := vfs.Mkdir(ownerInst.VFS(), "/Projects", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	sharingID := uuidv7()

	ownerSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{rootDir.DocID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-1",
				XorKey: MakeXorKey(),
			},
		},
	}

	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, ownerSharing))

	// Trash the root directory
	_, err = vfs.TrashDir(ownerInst.VFS(), rootDir)
	require.NoError(t, err)

	recipientSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{XorID(rootDir.DocID, ownerSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: "dummy-token",
				State:      "state-1",
			},
		},
	}
	require.NoError(t, recipientSharing.CreateDriveShortcut(recipientInst, true))
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, recipientSharing))

	checks, err := CheckSharings(ownerInst, false)
	require.NoError(t, err)
	require.Len(t, checks, 1)
	assert.Equal(t, "trashed_root_doc", checks[0]["type"])
}

func TestCheckSharingsSharedDrive_MissingMemberShortcut(t *testing.T) {
	ownerInst, recipientInst := setupTwoTestInstances(t)

	rootDir, err := vfs.Mkdir(ownerInst.VFS(), "/Projects", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	sharingID := uuidv7()

	ownerSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{rootDir.DocID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-1",
				XorKey: MakeXorKey(),
			},
		},
	}

	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, ownerSharing))
	require.NoError(t, ownerSharing.AddReferenceForSharing(ownerInst, &ownerSharing.Rules[0]))

	driveToken, err := ownerSharing.GetInteractCode(ownerInst, &ownerSharing.Members[1], 1)
	require.NoError(t, err)

	// Member sharing points to non-existent shortcut
	recipientSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		ShortcutID:    uuidv7(),
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{XorID(rootDir.DocID, ownerSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: driveToken,
				State:      "state-1",
			},
		},
	}
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, recipientSharing))

	// In full mode, missing shortcut is reported
	checks, err := CheckSharings(ownerInst, false)
	require.NoError(t, err)
	require.Len(t, checks, 1)
	assert.Equal(t, "missing_matching_docs_for_member", checks[0]["type"])
	assert.Equal(t, recipientInst.Domain, checks[0]["member"])

	// In fast mode, FS consistency check is skipped
	checksFast, err := CheckSharings(ownerInst, true)
	require.NoError(t, err)
	assert.Empty(t, checksFast)
}

func TestCheckSharingsSharedDrive_InvalidMemberToken(t *testing.T) {
	ownerInst, recipientInst := setupTwoTestInstances(t)

	rootDir, err := vfs.Mkdir(ownerInst.VFS(), "/Projects", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	sharingID := uuidv7()

	ownerSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{rootDir.DocID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-1",
				XorKey: MakeXorKey(),
			},
		},
	}

	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, ownerSharing))
	require.NoError(t, ownerSharing.AddReferenceForSharing(ownerInst, &ownerSharing.Rules[0]))

	recipientSharing := &Sharing{
		SID:           sharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "Projects",
				DocType: consts.Files,
				Values:  []string{XorID(rootDir.DocID, ownerSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: "invalid-unrecognized-token",
				State:      "state-1",
			},
		},
	}
	require.NoError(t, recipientSharing.CreateDriveShortcut(recipientInst, true))
	require.NotEmpty(t, recipientSharing.ShortcutID)
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, recipientSharing))

	checks, err := CheckSharings(ownerInst, false)
	require.NoError(t, err)
	require.Len(t, checks, 1)
	assert.Equal(t, "invalid_access_token", checks[0]["type"])
	assert.Equal(t, recipientInst.Domain, checks[0]["member"])
}

func TestCheckSharingsSharedDrive_NestedDriveDoesNotThrowSharingInSharing(t *testing.T) {
	ownerInst, recipientInst := setupTwoTestInstances(t)

	// Create parent and child directory on owner
	parentDir, err := vfs.Mkdir(ownerInst.VFS(), "/ParentDrive", nil)
	require.NoError(t, err)
	childDir, err := vfs.Mkdir(ownerInst.VFS(), "/ParentDrive/ChildDrive", nil)
	require.NoError(t, err)

	now := time.Now().UTC()
	parentSharingID := uuidv7()
	childSharingID := uuidv7()

	parentSharing := &Sharing{
		SID:           parentSharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "ParentDrive",
				DocType: consts.Files,
				Values:  []string{parentDir.DocID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-parent",
				XorKey: MakeXorKey(),
			},
		},
	}
	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, parentSharing))
	require.NoError(t, parentSharing.AddReferenceForSharing(ownerInst, &parentSharing.Rules[0]))

	parentDriveToken, err := parentSharing.GetInteractCode(ownerInst, &parentSharing.Members[1], 1)
	require.NoError(t, err)

	parentRecipientSharing := &Sharing{
		SID:           parentSharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "ParentDrive",
				DocType: consts.Files,
				Values:  []string{XorID(parentDir.DocID, parentSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: parentDriveToken,
				State:      "state-parent",
			},
		},
	}
	require.NoError(t, parentRecipientSharing.CreateDriveShortcut(recipientInst, true))
	require.NotEmpty(t, parentRecipientSharing.ShortcutID)
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, parentRecipientSharing))

	// Child drive nested inside parent drive
	childSharing := &Sharing{
		SID:           childSharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         true,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "ChildDrive",
				DocType: consts.Files,
				Values:  []string{childDir.DocID},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				State:  "state-child",
				XorKey: MakeXorKey(),
			},
		},
	}
	require.NoError(t, couchdb.CreateNamedDoc(ownerInst, childSharing))
	require.NoError(t, childSharing.AddReferenceForSharing(ownerInst, &childSharing.Rules[0]))

	childDriveToken, err := childSharing.GetInteractCode(ownerInst, &childSharing.Members[1], 1)
	require.NoError(t, err)

	childRecipientSharing := &Sharing{
		SID:           childSharingID,
		Drive:         true,
		DriveRootType: DriveRootTypeDirectory,
		Owner:         false,
		Active:        true,
		CreatedAt:     now,
		UpdatedAt:     now,
		Rules: []Rule{
			{
				Title:   "ChildDrive",
				DocType: consts.Files,
				Values:  []string{XorID(childDir.DocID, childSharing.Credentials[0].XorKey)},
			},
		},
		Members: []Member{
			{
				Status:   MemberStatusOwner,
				Instance: "https://" + ownerInst.Domain,
			},
			{
				Status:   MemberStatusReady,
				Instance: "https://" + recipientInst.Domain,
			},
		},
		Credentials: []Credentials{
			{
				DriveToken: childDriveToken,
				State:      "state-child",
			},
		},
	}
	require.NoError(t, childRecipientSharing.CreateDriveShortcut(recipientInst, true))
	require.NotEmpty(t, childRecipientSharing.ShortcutID)
	require.NoError(t, couchdb.CreateNamedDoc(recipientInst, childRecipientSharing))

	checks, err := CheckSharings(ownerInst, false)
	require.NoError(t, err)
	assert.Empty(t, checks, "nested shared drives must not trigger sharing_in_sharing or missing_matching_docs errors")
}
