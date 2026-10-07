package space

import (
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestProvisionDrive(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	t.Run("CreatesTheSpaceDriveOnTheOrganizationInstance", func(t *testing.T) {
		org := newOrgInstance(t)
		spaceID := "space-" + utils.RandomString(8)

		s, err := ProvisionDrive(org, Space{
			ID:             spaceID,
			OrganizationID: org.OrgID,
			Name:           "Design Sprint",
			Timestamp:      time.Date(2026, 10, 5, 9, 12, 44, 0, time.UTC),
		})
		require.NoError(t, err)

		require.True(t, s.Drive)
		require.True(t, s.OrgDrive)
		require.True(t, s.Owner)
		require.Equal(t, "Design Sprint", s.Description)

		rootID, err := s.DriveRootID()
		require.NoError(t, err)
		dir, err := org.VFS().DirByID(rootID)
		require.NoError(t, err)
		require.Equal(t, "/Design Sprint", dir.Fullpath)
		require.Contains(t, dir.ReferencedBy, couchdb.DocReference{Type: consts.Spaces, ID: spaceID})

		var rec Record
		require.NoError(t, couchdb.GetDoc(org, consts.Spaces, spaceID, &rec))
		require.Equal(t, s.SID, rec.SharingID)
		require.Equal(t, rootID, rec.DirID)
		require.Equal(t, org.OrgID, rec.OrganizationID)
		require.Equal(t, "Design Sprint", rec.Name)
	})

	t.Run("RedeliveryReturnsTheSameDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		sp := Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: "Roadmap"}

		first, err := ProvisionDrive(org, sp)
		require.NoError(t, err)
		again, err := ProvisionDrive(org, sp)
		require.NoError(t, err)

		require.Equal(t, first.SID, again.SID)
		drives, err := sharing.ListDrives(org)
		require.NoError(t, err)
		require.Len(t, drives, 1)
	})

	t.Run("RecoversTheDriveWhenTheRecordIsMissing", func(t *testing.T) {
		org := newOrgInstance(t)
		sp := Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: "Hiring"}

		first, err := ProvisionDrive(org, sp)
		require.NoError(t, err)
		var rec Record
		require.NoError(t, couchdb.GetDoc(org, consts.Spaces, sp.ID, &rec))
		require.NoError(t, couchdb.DeleteDoc(org, &rec))

		again, err := ProvisionDrive(org, sp)
		require.NoError(t, err)

		require.Equal(t, first.SID, again.SID)
		var restored Record
		require.NoError(t, couchdb.GetDoc(org, consts.Spaces, sp.ID, &restored))
		require.Equal(t, first.SID, restored.SharingID)
	})

	t.Run("PutsANewDriveOnTheFolderWhenTheDriveIsGone", func(t *testing.T) {
		org := newOrgInstance(t)
		sp := Space{
			ID:             "space-" + utils.RandomString(8),
			OrganizationID: org.OrgID,
			Name:           "Legal",
			Timestamp:      time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC),
		}
		first, err := ProvisionDrive(org, sp)
		require.NoError(t, err)
		firstRoot, err := first.DriveRootID()
		require.NoError(t, err)
		require.NoError(t, first.Revoke(org))

		sp.Timestamp = sp.Timestamp.Add(time.Hour)
		again, err := ProvisionDrive(org, sp)
		require.NoError(t, err)

		require.NotEqual(t, first.SID, again.SID)
		root, err := again.DriveRootID()
		require.NoError(t, err)
		require.Equal(t, firstRoot, root)
		var rec Record
		require.NoError(t, couchdb.GetDoc(org, consts.Spaces, sp.ID, &rec))
		require.Equal(t, again.SID, rec.SharingID)
		require.True(t, rec.LastEventAt.Equal(sp.Timestamp))
	})

	t.Run("NamesTheFolderAfterTheSpaceWithoutClashing", func(t *testing.T) {
		org := newOrgInstance(t)
		_, err := vfs.Mkdir(org.VFS(), "/Design", nil)
		require.NoError(t, err)

		for name, want := range map[string]string{
			"Design":        "/Design (2)",
			"Q3 / Q4 plans": "/Q3 - Q4 plans",
			"  ":            "",
		} {
			sp := Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: name}
			s, err := ProvisionDrive(org, sp)
			require.NoError(t, err)
			rootID, err := s.DriveRootID()
			require.NoError(t, err)
			dir, err := org.VFS().DirByID(rootID)
			require.NoError(t, err)
			if want == "" {
				want = "/" + sp.ID
			}
			require.Equal(t, want, dir.Fullpath)
			require.Equal(t, name, s.Description)
		}
	})

	t.Run("SharesTheDriveWithTheMembersByRole", func(t *testing.T) {
		org := newOrgInstance(t)
		for _, name := range []string{"alice", "bob", "carol"} {
			_, err := orgdirectory.UpsertManagedContact(org, orgdirectory.ContactPatch{
				OrganizationID: org.OrgID,
				Email:          name + "@acme.test",
				Name:           name,
				CozyURL:        "https://" + name + ".cozy.local/",
			})
			require.NoError(t, err)
		}

		s, err := ProvisionDrive(org, Space{
			ID:             "space-" + utils.RandomString(8),
			OrganizationID: org.OrgID,
			Name:           "Launch",
			Members: []Member{
				{Email: "alice@acme.test", Role: RoleAdmin},
				{Email: "bob@acme.test", Role: RoleEditor},
				{Email: "carol@acme.test", Role: RoleViewer},
				{Email: "dave@acme.test", Role: RoleEditor},
			},
		})
		require.NoError(t, err)

		readOnly := map[string]bool{}
		for _, m := range s.Members[1:] {
			readOnly[m.Email] = m.ReadOnly
		}
		require.Equal(t, map[string]bool{
			"alice@acme.test": false,
			"bob@acme.test":   false,
			"carol@acme.test": true,
		}, readOnly)
	})

	t.Run("RedeliveryAddsOnlyTheMissingMembers", func(t *testing.T) {
		org := newOrgInstance(t)
		for _, name := range []string{"alice", "bob"} {
			_, err := orgdirectory.UpsertManagedContact(org, orgdirectory.ContactPatch{
				OrganizationID: org.OrgID,
				Email:          name + "@acme.test",
				Name:           name,
				CozyURL:        "https://" + name + ".cozy.local/",
			})
			require.NoError(t, err)
		}
		sp := Space{
			ID:             "space-" + utils.RandomString(8),
			OrganizationID: org.OrgID,
			Name:           "Ops",
			Members:        []Member{{Email: "alice@acme.test", Role: RoleEditor}},
		}
		_, err := ProvisionDrive(org, sp)
		require.NoError(t, err)

		sp.Members = []Member{
			{Email: "alice@acme.test", Role: RoleViewer},
			{Email: "bob@acme.test", Role: RoleViewer},
		}
		s, err := ProvisionDrive(org, sp)
		require.NoError(t, err)

		readOnly := map[string]bool{}
		for _, m := range s.Members[1:] {
			readOnly[m.Email] = m.ReadOnly
		}
		require.Equal(t, map[string]bool{
			"alice@acme.test": false,
			"bob@acme.test":   true,
		}, readOnly)
	})
}

func newOrgInstance(t *testing.T) *instance.Instance {
	t.Helper()
	orgID := strings.ToLower("org" + utils.RandomString(10))
	setup := testutils.NewSetup(t, t.Name())
	return setup.GetTestInstance(&lifecycle.Options{
		Domain:     orgID + ".cozy.local",
		OrgID:      orgID,
		Email:      "admin@" + orgID + ".example",
		PublicName: "Organization",
	})
}
