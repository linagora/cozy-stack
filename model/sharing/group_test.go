package sharing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/cozy/cozy-stack/worker/mails"
)

func TestGroupColorJSON(t *testing.T) {
	data, err := json.Marshal(Group{
		ID:    "group-id",
		Name:  "Marketing",
		Color: "#22AA55",
	})
	require.NoError(t, err)
	require.JSONEq(t, `{
		"id": "group-id",
		"name": "Marketing",
		"color": "#22AA55",
		"addedBy": 0,
		"read_only": false
	}`, string(data))
}

func TestGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{
		Email:      "alice@example.net",
		PublicName: "Alice",
	})

	t.Run("RevokeGroup", func(t *testing.T) {
		now := time.Now()
		friends := createGroup(t, inst, "Friends")
		friends.M["color"] = "#22AA55"
		require.NoError(t, couchdb.UpdateDoc(inst, friends))
		football := createGroup(t, inst, "Football")
		bob := createContactInGroups(t, inst, "Bob", []string{friends.ID()})
		_ = createContactInGroups(t, inst, "Charlie", []string{friends.ID(), football.ID()})
		_ = createContactInGroups(t, inst, "Dave", []string{football.ID()})

		s := &Sharing{
			Active:      true,
			Owner:       true,
			Description: "Just testing groups",
			Members: []Member{
				{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
			},
			Rules: []Rule{
				{
					Title:   "Just testing groups",
					DocType: "io.cozy.tests",
					Values:  []string{uuidv7()},
				},
			},
			CreatedAt: now,
			UpdatedAt: now,
		}
		require.NoError(t, couchdb.CreateDoc(inst, s))
		require.NoError(t, s.AddContact(inst, bob.ID(), false))
		require.NoError(t, s.AddGroup(inst, friends.ID(), false))
		require.NoError(t, s.AddGroup(inst, football.ID(), false))

		require.Len(t, s.Members, 4)
		require.Equal(t, s.Members[0].Name, "Alice")
		require.Equal(t, s.Members[1].Name, "Bob")
		assert.False(t, s.Members[1].OnlyInGroups)
		assert.Equal(t, s.Members[1].Groups, []int{0})
		require.Equal(t, s.Members[2].Name, "Charlie")
		assert.True(t, s.Members[2].OnlyInGroups)
		assert.Equal(t, s.Members[2].Groups, []int{0, 1})
		require.Equal(t, s.Members[3].Name, "Dave")
		assert.True(t, s.Members[3].OnlyInGroups)
		assert.Equal(t, s.Members[3].Groups, []int{1})

		require.Len(t, s.Groups, 2)
		require.Equal(t, s.Groups[0].Name, "Friends")
		require.Equal(t, "#22AA55", s.Groups[0].Color)
		assert.False(t, s.Groups[0].Revoked)
		require.Equal(t, s.Groups[1].Name, "Football")
		assert.False(t, s.Groups[1].Revoked)

		require.NoError(t, s.RevokeGroup(inst, 1)) // Revoke the football group

		require.Len(t, s.Members, 4)
		assert.NotEqual(t, s.Members[1].Status, MemberStatusRevoked)
		assert.Equal(t, s.Members[1].Groups, []int{0})
		assert.NotEqual(t, s.Members[2].Status, MemberStatusRevoked)
		assert.Equal(t, s.Members[2].Groups, []int{0})
		assert.Equal(t, s.Members[3].Status, MemberStatusRevoked)
		assert.Empty(t, s.Members[3].Groups)

		require.Len(t, s.Groups, 2)
		assert.False(t, s.Groups[0].Revoked)
		assert.True(t, s.Groups[1].Revoked)

		require.NoError(t, s.RevokeGroup(inst, 0)) // Revoke the fiends group

		require.Len(t, s.Members, 4)
		assert.NotEqual(t, s.Members[1].Status, MemberStatusRevoked)
		assert.Empty(t, s.Members[1].Groups)
		assert.Equal(t, s.Members[2].Status, MemberStatusRevoked)
		assert.Empty(t, s.Members[2].Groups)
		assert.Equal(t, s.Members[3].Status, MemberStatusRevoked)
		assert.Empty(t, s.Members[3].Groups)

		require.Len(t, s.Groups, 2)
		assert.True(t, s.Groups[0].Revoked)
		assert.True(t, s.Groups[1].Revoked)
	})

	t.Run("UpdateGroupMetadata", func(t *testing.T) {
		group := createGroup(t, inst, "Marketing")
		group.M["color"] = "#3367D6"
		require.NoError(t, couchdb.UpdateDoc(inst, group))

		s := &Sharing{
			Active: true,
			Groups: []Group{{
				ID:    group.ID(),
				Name:  group.Name(),
				Color: group.Color(),
			}},
		}
		require.NoError(t, couchdb.CreateDoc(inst, s))

		updated := group.JSONDoc.Clone().(*couchdb.JSONDoc)
		updated.M["name"] = "Brand marketing"
		updated.M["color"] = "#A142F4"
		require.NoError(t, UpdateGroups(inst, job.ShareGroupMessage{RenamedGroup: updated}))

		stored, err := FindSharing(inst, s.ID())
		require.NoError(t, err)
		require.Len(t, stored.Groups, 1)
		require.Equal(t, "Brand marketing", stored.Groups[0].Name)
		require.Equal(t, "#A142F4", stored.Groups[0].Color)

		updated.M["name"] = 42
		updated.M["color"] = "#0F9D58"
		require.NoError(t, UpdateGroups(inst, job.ShareGroupMessage{RenamedGroup: updated}))

		stored, err = FindSharing(inst, s.ID())
		require.NoError(t, err)
		require.Equal(t, "Brand marketing", stored.Groups[0].Name)
		require.Equal(t, "#0F9D58", stored.Groups[0].Color)

		delete(updated.M, "name")
		delete(updated.M, "color")
		require.NoError(t, UpdateGroups(inst, job.ShareGroupMessage{RenamedGroup: updated}))

		stored, err = FindSharing(inst, s.ID())
		require.NoError(t, err)
		require.Equal(t, "Brand marketing", stored.Groups[0].Name)
		require.Empty(t, stored.Groups[0].Color)
	})

	t.Run("UpdateGroups", func(t *testing.T) {
		now := time.Now()
		friends := createGroup(t, inst, "Friends")
		football := createGroup(t, inst, "Football")
		_ = createContactInGroups(t, inst, "Bob", []string{friends.ID()})
		charlie := createContactInGroups(t, inst, "Charlie", []string{football.ID()})
		dave := createContactWithoutEmail(t, inst, "Dave", []string{football.ID()})

		s := &Sharing{
			Active:      true,
			Owner:       true,
			Description: "Just testing groups",
			Members: []Member{
				{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
			},
			Rules: []Rule{
				{
					Title:   "Just testing groups",
					DocType: "io.cozy.tests",
					Values:  []string{uuidv7()},
				},
			},
			CreatedAt: now,
			UpdatedAt: now,
		}
		require.NoError(t, s.AddGroup(inst, friends.ID(), false))
		require.NoError(t, s.AddGroup(inst, football.ID(), false))
		perms, err := s.Create(inst)
		require.NoError(t, err)
		require.NoError(t, s.SendInvitations(inst, perms))
		sid := s.ID()

		require.Len(t, s.Members, 4)
		require.Equal(t, s.Members[0].Name, "Alice")
		require.Equal(t, s.Members[1].Name, "Bob")
		assert.True(t, s.Members[1].OnlyInGroups)
		assert.Equal(t, s.Members[1].Groups, []int{0})
		assert.Equal(t, s.Members[1].Status, MemberStatusPendingInvitation)
		require.Equal(t, s.Members[2].Name, "Charlie")
		assert.True(t, s.Members[2].OnlyInGroups)
		assert.Equal(t, s.Members[2].Groups, []int{1})
		assert.Equal(t, s.Members[2].Status, MemberStatusPendingInvitation)
		require.Equal(t, s.Members[3].Name, "Dave")
		assert.True(t, s.Members[3].OnlyInGroups)
		assert.Equal(t, s.Members[3].Groups, []int{1})
		assert.Equal(t, s.Members[3].Status, MemberStatusMailNotSent)

		require.Len(t, s.Groups, 2)
		require.Equal(t, s.Groups[0].Name, "Friends")
		assert.False(t, s.Groups[0].Revoked)
		require.Equal(t, s.Groups[1].Name, "Football")
		assert.False(t, s.Groups[1].Revoked)

		// Charlie is added to the friends group
		msg1 := job.ShareGroupMessage{
			ContactID:   charlie.ID(),
			GroupsAdded: []string{friends.ID()},
		}
		require.NoError(t, UpdateGroups(inst, msg1))

		s = &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, s))

		require.Len(t, s.Members, 4)
		require.Equal(t, s.Members[0].Name, "Alice")
		require.Equal(t, s.Members[1].Name, "Bob")
		assert.True(t, s.Members[1].OnlyInGroups)
		assert.Equal(t, s.Members[1].Groups, []int{0})
		require.Equal(t, s.Members[2].Name, "Charlie")
		assert.True(t, s.Members[2].OnlyInGroups)
		assert.Equal(t, s.Members[2].Groups, []int{0, 1})
		require.Equal(t, s.Members[3].Name, "Dave")
		assert.True(t, s.Members[3].OnlyInGroups)
		assert.Equal(t, s.Members[3].Groups, []int{1})

		// Replaying the same group addition must not duplicate the group index,
		// rotate credentials, resend invitations, or update the sharing.
		revAfterFirstAddition := s.Rev()
		credentialsAfterFirstAddition := append([]Credentials(nil), s.Credentials...)
		require.NoError(t, UpdateGroups(inst, msg1))

		s = &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, s))
		require.Equal(t, revAfterFirstAddition, s.Rev())
		require.Equal(t, credentialsAfterFirstAddition, s.Credentials)
		assert.Equal(t, []int{0, 1}, s.Members[2].Groups)

		// Charlie is removed of the football group
		msg2 := job.ShareGroupMessage{
			ContactID:     charlie.ID(),
			GroupsRemoved: []string{football.ID()},
		}
		require.NoError(t, UpdateGroups(inst, msg2))

		s = &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, s))

		require.Len(t, s.Members, 4)
		require.Equal(t, s.Members[0].Name, "Alice")
		require.Equal(t, s.Members[1].Name, "Bob")
		assert.True(t, s.Members[1].OnlyInGroups)
		assert.Equal(t, s.Members[1].Groups, []int{0})
		require.Equal(t, s.Members[2].Name, "Charlie")
		assert.True(t, s.Members[2].OnlyInGroups)
		assert.Equal(t, s.Members[2].Groups, []int{0})
		require.Equal(t, s.Members[3].Name, "Dave")
		assert.True(t, s.Members[3].OnlyInGroups)
		assert.Equal(t, s.Members[3].Groups, []int{1})

		// Email address is added for Dave, and an invitation can now be sent
		addEmailToContact(t, inst, dave)
		msg3 := job.ShareGroupMessage{
			ContactID:       dave.ID(),
			BecomeInvitable: true,
		}
		require.NoError(t, UpdateGroups(inst, msg3))

		s = &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, s))

		require.Len(t, s.Members, 4)
		require.Equal(t, s.Members[0].Name, "Alice")
		require.Equal(t, s.Members[1].Name, "Bob")
		assert.True(t, s.Members[1].OnlyInGroups)
		assert.Equal(t, s.Members[1].Groups, []int{0})
		assert.Equal(t, s.Members[1].Status, MemberStatusPendingInvitation)
		require.Equal(t, s.Members[2].Name, "Charlie")
		assert.True(t, s.Members[2].OnlyInGroups)
		assert.Equal(t, s.Members[2].Groups, []int{0})
		assert.Equal(t, s.Members[1].Status, MemberStatusPendingInvitation)
		require.Equal(t, s.Members[3].Name, "Dave")
		assert.True(t, s.Members[3].OnlyInGroups)
		assert.Equal(t, s.Members[3].Groups, []int{1})
		assert.Equal(t, s.Members[3].Status, MemberStatusPendingInvitation)
	})

	t.Run("RemoveLastDriveGroupMemberKeepsSharing", func(t *testing.T) {
		now := time.Now()
		team := createGroup(t, inst, "Drive Team")
		alice := createContactInGroups(t, inst, "DriveAlice", []string{team.ID()})

		s := &Sharing{
			Active:      true,
			Owner:       true,
			Drive:       true,
			Description: "Just testing drive groups",
			Members: []Member{
				{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
			},
			Rules: []Rule{
				{
					Title:   "Just testing drive groups",
					DocType: consts.Files,
					Values:  []string{uuidv7()},
				},
			},
			CreatedAt: now,
			UpdatedAt: now,
		}
		require.NoError(t, couchdb.CreateDoc(inst, s))
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		require.Len(t, s.Members, 2)
		require.Equal(t, s.Members[1].Name, "DriveAlice")
		assert.True(t, s.Members[1].OnlyInGroups)
		assert.Equal(t, []int{0}, s.Members[1].Groups)

		msg := job.ShareGroupMessage{
			ContactID:     alice.ID(),
			GroupsRemoved: []string{team.ID()},
		}
		require.NoError(t, UpdateGroups(inst, msg))

		s = &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, s))

		require.Len(t, s.Members, 2)
		assert.True(t, s.Active)
		assert.Equal(t, MemberStatusRevoked, s.Members[1].Status)
		assert.Empty(t, s.Members[1].Groups)
		require.Len(t, s.Groups, 1)
		assert.False(t, s.Groups[0].Revoked)
	})

	t.Run("RemoveMemberFromGroupPreservesRemovalAfterConflict", func(t *testing.T) {
		team := createGroup(t, inst, "Conflicting Drive Team")
		otherTeam := createGroup(t, inst, "Remaining Drive Team")
		alice := createContactInGroups(t, inst, "ConflictAlice", []string{team.ID()})

		s := createDriveSharingForGroupTest(t, inst, "Conflicting group removal")
		s.OrgDrive = true
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, s.AddGroup(inst, otherTeam.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		concurrent := &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, concurrent))
		concurrent.Description = "Updated concurrently"
		require.NoError(t, couchdb.UpdateDoc(inst, concurrent))

		require.NoError(t, s.RemoveMemberFromGroup(inst, 0, alice))

		stored := &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, stored))
		require.Len(t, stored.Members, 2)
		assert.Equal(t, MemberStatusRevoked, stored.Members[1].Status)
		assert.Empty(t, stored.Members[1].Groups)
		assert.Equal(t, "Updated concurrently", stored.Description)
	})

	t.Run("RevokeLastDriveGroupDeletesSharing", func(t *testing.T) {
		team := createGroup(t, inst, "Drive Group")
		_ = createContactInGroups(t, inst, "DriveGroupAlice", []string{team.ID()})
		s := createDriveSharingForGroupTest(t, inst, "Drive group revoke")
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		require.Len(t, s.Members, 2)
		require.Len(t, s.Groups, 1)
		require.NoError(t, s.RevokeGroup(inst, 0))

		deleted := &Sharing{}
		err := couchdb.GetDoc(inst, consts.Sharings, sid, deleted)
		require.Error(t, err)
		assert.True(t, couchdb.IsNotFoundError(err), "expected deleted sharing, got %v", err)
	})

	t.Run("RevokeLastOrgDriveGroupKeepsInactiveSharing", func(t *testing.T) {
		team := createGroup(t, inst, "Org Drive Group")
		_ = createContactInGroups(t, inst, "OrgDriveGroupAlice", []string{team.ID()})
		s := createDriveSharingForGroupTest(t, inst, "Org drive group revoke")
		s.OrgDrive = true
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		require.Len(t, s.Members, 2)
		require.Len(t, s.Groups, 1)
		require.NoError(t, s.RevokeGroup(inst, 0))

		stored := &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, stored))
		assert.False(t, stored.Active)
		assert.True(t, stored.OrgDrive)
		require.Len(t, stored.Members, 2)
		assert.Equal(t, MemberStatusRevoked, stored.Members[1].Status)
		require.Len(t, stored.Groups, 1)
		assert.True(t, stored.Groups[0].Revoked)
	})

	t.Run("RevokeEmptyLastDriveGroupDeletesSharing", func(t *testing.T) {
		team := createGroup(t, inst, "Empty Drive Group")
		s := createDriveSharingForGroupTest(t, inst, "Empty drive group revoke")
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		require.Len(t, s.Members, 1)
		require.Len(t, s.Groups, 1)
		require.NoError(t, s.RevokeGroup(inst, 0))

		deleted := &Sharing{}
		err := couchdb.GetDoc(inst, consts.Sharings, sid, deleted)
		require.Error(t, err)
		assert.True(t, couchdb.IsNotFoundError(err), "expected deleted sharing, got %v", err)
	})

	t.Run("RevokeDriveGroupKeepsSharingWhenAnotherGroupRemains", func(t *testing.T) {
		team := createGroup(t, inst, "First Empty Drive Group")
		otherTeam := createGroup(t, inst, "Second Empty Drive Group")
		s := createDriveSharingForGroupTest(t, inst, "Remaining drive group")
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, s.AddGroup(inst, otherTeam.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		require.Len(t, s.Members, 1)
		require.Len(t, s.Groups, 2)
		require.NoError(t, s.RevokeGroup(inst, 0))

		stored := &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, stored))
		assert.True(t, stored.Active)
		require.Len(t, stored.Groups, 2)
		assert.True(t, stored.Groups[0].Revoked)
		assert.False(t, stored.Groups[1].Revoked)
	})

	t.Run("DeleteGroupRevokesMatchingSharingGroup", func(t *testing.T) {
		team := createGroup(t, inst, "Deleted Drive Group")
		otherTeam := createGroup(t, inst, "Active Drive Group")
		_ = createContactInGroups(t, inst, "DeletedGroupAlice", []string{team.ID()})
		s := createDriveSharingForGroupTest(t, inst, "Deleted sharing group")
		s.OrgDrive = true
		sid := s.SID
		require.NoError(t, s.AddGroup(inst, team.ID(), false))
		require.NoError(t, s.AddGroup(inst, otherTeam.ID(), false))
		require.NoError(t, couchdb.UpdateDoc(inst, s))

		require.NoError(t, UpdateGroups(inst, job.ShareGroupMessage{
			DeletedGroupID: team.ID(),
		}))

		stored := &Sharing{}
		require.NoError(t, couchdb.GetDoc(inst, consts.Sharings, sid, stored))
		assert.True(t, stored.Active)
		require.Len(t, stored.Members, 2)
		assert.Equal(t, MemberStatusRevoked, stored.Members[1].Status)
		assert.Empty(t, stored.Members[1].Groups)
		require.Len(t, stored.Groups, 2)
		assert.True(t, stored.Groups[0].Revoked)
		assert.False(t, stored.Groups[1].Revoked)
	})
}

func createDriveSharingForGroupTest(t *testing.T, inst *instance.Instance, description string) *Sharing {
	t.Helper()
	now := time.Now()
	s := &Sharing{
		Active:      true,
		Owner:       true,
		Drive:       true,
		Description: description,
		Members: []Member{
			{Status: MemberStatusOwner, Name: "Alice", Email: "alice@cozy.tools"},
		},
		Rules: []Rule{
			{
				Title:   description,
				DocType: consts.Files,
				Values:  []string{uuidv7()},
			},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, couchdb.CreateDoc(inst, s))
	return s
}

func createGroup(t *testing.T, inst *instance.Instance, name string) *contact.Group {
	t.Helper()
	g := contact.NewGroup()
	g.M["name"] = name
	require.NoError(t, couchdb.CreateDoc(inst, g))
	return g
}

func createContactInGroups(t *testing.T, inst *instance.Instance, contactName string, groupIDs []string) *contact.Contact {
	t.Helper()
	email := strings.ToLower(contactName) + "@cozy.tools"
	mail := map[string]interface{}{"address": email}

	var groups []interface{}
	for _, id := range groupIDs {
		groups = append(groups, map[string]interface{}{
			"_id":   id,
			"_type": consts.Groups,
		})
	}

	c := contact.New()
	c.M["fullname"] = contactName
	c.M["email"] = []interface{}{mail}
	c.M["relationships"] = map[string]interface{}{
		"groups": map[string]interface{}{"data": groups},
	}
	require.NoError(t, couchdb.CreateDoc(inst, c))
	return c
}

func createContactWithoutEmail(t *testing.T, inst *instance.Instance, contactName string, groupIDs []string) *contact.Contact {
	t.Helper()

	var groups []interface{}
	for _, id := range groupIDs {
		groups = append(groups, map[string]interface{}{
			"_id":   id,
			"_type": consts.Groups,
		})
	}

	c := contact.New()
	c.M["fullname"] = contactName
	c.M["relationships"] = map[string]interface{}{
		"groups": map[string]interface{}{"data": groups},
	}
	require.NoError(t, couchdb.CreateDoc(inst, c))
	return c
}

func addEmailToContact(t *testing.T, inst *instance.Instance, c *contact.Contact) {
	t.Helper()

	email := strings.ToLower(c.PrimaryName()) + "@cozy.tools"
	mail := map[string]interface{}{"address": email}
	c.M["email"] = []interface{}{mail}
	require.NoError(t, couchdb.UpdateDoc(inst, c))
}
