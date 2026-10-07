package orgdirectory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestGroupDocID(t *testing.T) {
	id1 := GroupDocID("org123", "engineering")
	id2 := GroupDocID("org123", "engineering")
	id3 := GroupDocID("org456", "engineering")

	require.Equal(t, id1, id2)
	require.NotEqual(t, id1, id3)
	require.Contains(t, id1, "b2b-group-")
}

func TestSyncGroupCreatedAdoptsExistingExternalContact(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "org-groups-" + suffix
	orgDomain := "groups-" + suffix + ".example"
	alice := createOrgDirectoryInstance(t, "alice-groups-"+suffix+".local", orgDomain, orgID, "alice@acme.test", "Alice")
	bob := createOrgDirectoryInstance(t, "bob-groups-"+suffix+".local", orgDomain, orgID, "bob@acme.test", "Bob")

	existing := createExternalContact(t, bob, "alice@acme.test", "Old Alice")

	err := SyncGroupCreated(testCtx(t), GroupCreatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Name:           "Engineering",
		Description:    "Engineering team",
		Color:          "#3366FF",
		Members: []GroupMember{
			{
				Username:      "alice",
				Email:         "alice@acme.test",
				FirstName:     "Alice",
				WorkplaceFQDN: alice.Domain,
			},
		},
	})
	require.NoError(t, err)

	groupID := GroupDocID(orgID, "engineering")
	group, err := contact.FindGroup(bob, groupID)
	require.NoError(t, err)
	require.Equal(t, "Engineering", group.Name())
	require.True(t, IsManagedDirectoryDoc(&group.JSONDoc))

	stored, err := contact.Find(bob, existing.ID())
	require.NoError(t, err)
	require.Equal(t, existing.ID(), stored.ID())
	require.True(t, stored.IsExternal())
	require.True(t, stored.IsTrusted())
	require.True(t, IsManagedDirectoryDoc(&stored.JSONDoc))
	require.Equal(t, alice.PageURL("", nil), stored.PrimaryCozyURL())
	require.Contains(t, stored.GroupIDs(), groupID)
}

func TestSyncGroupDeletedRemovesRelationshipsBeforeDeletingGroup(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "org-delete-group-" + suffix
	orgDomain := "delete-group-" + suffix + ".example"
	alice := createOrgDirectoryInstance(t, "alice-delete-group-"+suffix+".local", orgDomain, orgID, "alice@acme.test", "Alice")
	bob := createOrgDirectoryInstance(t, "bob-delete-group-"+suffix+".local", orgDomain, orgID, "bob@acme.test", "Bob")

	err := SyncGroupCreated(testCtx(t), GroupCreatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Name:           "Engineering",
		Members: []GroupMember{
			{
				Username:      "alice",
				Email:         "alice@acme.test",
				FirstName:     "Alice",
				WorkplaceFQDN: alice.Domain,
			},
		},
	})
	require.NoError(t, err)

	groupID := GroupDocID(orgID, "engineering")
	bobContacts, err := contact.FindAllByEmail(bob, "alice@acme.test")
	require.NoError(t, err)
	require.Len(t, bobContacts, 1)
	require.Contains(t, bobContacts[0].GroupIDs(), groupID)

	err = SyncGroupDeleted(testCtx(t), GroupDeletedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
	})
	require.NoError(t, err)

	_, err = contact.FindGroup(bob, groupID)
	require.True(t, couchdb.IsNotFoundError(err))

	stored, err := contact.Find(bob, bobContacts[0].ID())
	require.NoError(t, err)
	require.NotContains(t, stored.GroupIDs(), groupID)
}

func TestSyncGroupOptionalFieldsOmitPreserveAndClear(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "org-optional-group-" + suffix
	orgDomain := "optional-group-" + suffix + ".example"
	inst := createOrgDirectoryInstance(t, "optional-group-"+suffix+".local", orgDomain, orgID, "owner@acme.test", "Owner")

	err := SyncGroupCreated(testCtx(t), GroupCreatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Name:           "Engineering",
	})
	require.NoError(t, err)

	groupID := GroupDocID(orgID, "engineering")
	group, err := contact.FindGroup(inst, groupID)
	require.NoError(t, err)
	_, hasDescription := group.M["description"]
	_, hasColor := group.M["color"]
	require.False(t, hasDescription)
	require.False(t, hasColor)

	description := "Platform engineering"
	color := "#22AA55"
	err = SyncGroupUpdated(testCtx(t), GroupUpdatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Description:    &description,
		Color:          &color,
	})
	require.NoError(t, err)

	group, err = contact.FindGroup(inst, groupID)
	require.NoError(t, err)
	require.Equal(t, "Platform engineering", group.M["description"])
	require.Equal(t, "#22AA55", group.M["color"])

	err = SyncGroupUpdated(testCtx(t), GroupUpdatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
	})
	require.NoError(t, err)

	group, err = contact.FindGroup(inst, groupID)
	require.NoError(t, err)
	require.Equal(t, "Platform engineering", group.M["description"])
	require.Equal(t, "#22AA55", group.M["color"])

	empty := ""
	err = SyncGroupUpdated(testCtx(t), GroupUpdatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Description:    &empty,
	})
	require.NoError(t, err)

	group, err = contact.FindGroup(inst, groupID)
	require.NoError(t, err)
	_, hasDescription = group.M["description"]
	require.False(t, hasDescription)
	require.Equal(t, "#22AA55", group.M["color"])

	err = SyncGroupUpdated(testCtx(t), GroupUpdatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Color:          &empty,
	})
	require.NoError(t, err)

	group, err = contact.FindGroup(inst, groupID)
	require.NoError(t, err)
	_, hasColor = group.M["color"]
	require.False(t, hasColor)
}

func TestCopyOrgDirectoryFromOrgInstanceUsesGeneratedContactIDAndReusesIt(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "orgcopy" + suffix
	orgDomain := "copy-" + suffix + ".example"
	orgInst := createOrgDirectoryInstance(t, orgID+".local", orgDomain, orgID, "owner@acme.test", "Owner")

	err := SyncGroupCreated(testCtx(t), GroupCreatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Name:           "Engineering",
		Members: []GroupMember{
			{
				Username:      "alice",
				Email:         "alice@acme.test",
				FirstName:     "Alice",
				WorkplaceFQDN: "alice-" + suffix + ".local",
			},
		},
	})
	require.NoError(t, err)

	sourceContacts, err := contact.FindAllByEmail(orgInst, "alice@acme.test")
	require.NoError(t, err)
	require.Len(t, sourceContacts, 1)
	require.NotContains(t, sourceContacts[0].ID(), "b2b-contact-", "source contact keeps generated id")

	member := createOrgDirectoryInstance(t, "bob-copy-"+suffix+".local", orgDomain, orgID, "bob@acme.test", "Bob")
	require.NoError(t, CopyOrgDirectoryFromOrgInstance(testCtx(t), member, orgID))
	require.NoError(t, CopyOrgDirectoryFromOrgInstance(testCtx(t), member, orgID))

	targetContacts, err := contact.FindAllByEmail(member, "alice@acme.test")
	require.NoError(t, err)
	require.Len(t, targetContacts, 1)
	require.NotContains(t, targetContacts[0].ID(), "b2b-contact-")
	require.True(t, IsManagedDirectoryDoc(&targetContacts[0].JSONDoc))
	require.Contains(t, targetContacts[0].GroupIDs(), GroupDocID(orgID, "engineering"))
}

func TestUpsertManagedContactRequiresOnlyEmail(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "org-contact-patch-" + suffix
	orgDomain := "contact-patch-" + suffix + ".example"
	inst := createOrgDirectoryInstance(t, "contact-patch-"+suffix+".local", orgDomain, orgID, "owner@acme.test", "Owner")

	stored, err := UpsertManagedContact(inst, ContactPatch{
		OrganizationID: orgID,
		Email:          "alice@acme.test",
		Name:           "Alice",
	})
	require.NoError(t, err)
	require.Equal(t, "Alice", stored.PrimaryName())

	matches, err := contact.FindAllByEmail(inst, "alice@acme.test")
	require.NoError(t, err)
	require.Len(t, matches, 1)
	require.Equal(t, stored.ID(), matches[0].ID())

	_, err = UpsertManagedContact(inst, ContactPatch{
		OrganizationID: orgID,
		WorkplaceFQDN:  "alice-" + suffix + ".local",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "contact missing email")
}

func createOrgDirectoryInstance(t *testing.T, domain, orgDomain, orgID, email, publicName string) *instance.Instance {
	t.Helper()
	inst, err := lifecycle.Create(&lifecycle.Options{
		Domain:     domain,
		OrgDomain:  orgDomain,
		OrgID:      orgID,
		Email:      email,
		PublicName: publicName,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
	return inst
}

func createExternalContact(t *testing.T, inst *instance.Instance, email, name string) *contact.Contact {
	t.Helper()
	c, err := contact.Create(inst, contact.CreateOptions{
		Email:             email,
		Name:              name,
		External:          true,
		TrustedForSharing: true,
	})
	require.NoError(t, err)
	return c
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func needCouchDB(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := couchdb.CheckStatus(ctx); err != nil {
		t.Skipf("couchdb is required for this test: %v", err)
	}
}

func TestSyncGroupWithCommonContacts(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	const feedCtx = "groups-common-contacts"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{feedCtx: map[string]interface{}{"common_contacts": true}}
	t.Cleanup(func() { conf.Contexts = previous })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "feedgroups" + suffix
	orgDomain := "feed-groups-" + suffix + ".example"
	newInstance := func(domain string) *instance.Instance {
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      domain,
			OrgDomain:   orgDomain,
			OrgID:       orgID,
			ContextName: feedCtx,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		return inst
	}
	org := newInstance(orgID + ".local")
	bob := newInstance("bob-feed-groups-" + suffix + ".local")

	fromFeed := contact.New()
	fromFeed.M["fullname"] = "Alice Liddell"
	fromFeed.M["email"] = []interface{}{
		map[string]interface{}{"address": "alice@acme.test", "primary": true},
		map[string]interface{}{"address": "alice@home.test", "primary": false},
	}
	fromFeed.M["metadata"] = map[string]interface{}{"external": true}
	fromFeed.M[contact.CardDAVPathKey] = "addressbooks/domain/alice.vcf"
	require.NoError(t, couchdb.CreateDoc(org, fromFeed))

	err := SyncGroupCreated(testCtx(t), GroupCreatedMessage{
		OrganizationID: orgID,
		ID:             "engineering",
		Name:           "Engineering",
		Members: []GroupMember{
			{Username: "alice", Email: "alice@acme.test", FirstName: "Alice", WorkplaceFQDN: "alice-" + suffix + ".local"},
			{Username: "carol", Email: "carol@acme.test", FirstName: "Carol"},
		},
	})
	require.NoError(t, err)

	groupID := GroupDocID(orgID, "engineering")
	_, err = contact.FindGroup(org, groupID)
	require.NoError(t, err)
	_, err = contact.FindGroup(bob, groupID)
	require.True(t, couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err), "member instances get no copy: %v", err)

	stored, err := contact.Find(org, fromFeed.ID())
	require.NoError(t, err)
	require.Contains(t, stored.GroupIDs(), groupID)
	require.True(t, stored.IsTrusted())
	require.Equal(t, "Alice Liddell", stored.M["fullname"], "the feed fields are kept")
	require.Len(t, stored.M["email"], 2)
	require.Empty(t, stored.PrimaryCozyURL())

	carol, err := findManagedContactByEmail(org, "carol@acme.test")
	require.NoError(t, err, "a member the feed has not sent yet is written right away")
	require.Contains(t, carol.GroupIDs(), groupID)

	require.NoError(t, SyncGroupDeleted(testCtx(t), GroupDeletedMessage{OrganizationID: orgID, ID: "engineering"}))
	stored, err = contact.Find(org, fromFeed.ID())
	require.NoError(t, err)
	require.NotContains(t, stored.GroupIDs(), groupID)
}
