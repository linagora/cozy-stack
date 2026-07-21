package rabbitmq_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestSyncCreatedOrgContact(t *testing.T) {
	t.Run("CreatesExternalContactsForOtherMembers", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-" + suffix + ".example"
		orgID := "org-sync-created-" + suffix
		target := createInstanceInOrg(t, "sync-created-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-created-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob", "+33987654321")
		carol := createInstanceInOrg(t, "sync-created-carol-"+suffix+".local", orgDomain, orgID, "carol@example.com", "Carol")
		targetURL := target.PageURL("", nil)
		bobURL := bob.PageURL("", nil)
		carolURL := carol.PageURL("", nil)

		preexisting := createContact(t, bob, "alice@example.com", "https://manual.example", false, "Existing Alice")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:  "alice@example.com",
			Mobile:         "+33123456789",
			WorkplaceFqdn:  target.Domain,
			OrganizationID: orgID,
		})
		require.NoError(t, err)

		bobContacts, err := contact.FindAllByEmail(bob, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, bobContacts, 2)
		var bobExternalCount int
		var bobFoundManual bool
		for _, doc := range bobContacts {
			if doc.ID() == preexisting.ID() {
				bobFoundManual = true
				require.Equal(t, "Existing Alice", doc.PrimaryName())
				require.False(t, doc.IsExternal())
				require.False(t, doc.IsTrusted())
				continue
			}
			if doc.IsExternal() {
				bobExternalCount++
				require.Equal(t, "Alice", doc.PrimaryName())
				require.Equal(t, "+33123456789", doc.PrimaryPhoneNumber())
				require.Equal(t, targetURL, doc.PrimaryCozyURL())
				require.True(t, doc.IsTrusted())
			}
		}
		require.True(t, bobFoundManual)
		require.Equal(t, 1, bobExternalCount)

		carolContacts, err := contact.FindAllByEmail(carol, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, carolContacts, 1)
		require.True(t, carolContacts[0].IsExternal())
		require.Equal(t, "Alice", carolContacts[0].PrimaryName())
		require.Equal(t, "+33123456789", carolContacts[0].PrimaryPhoneNumber())
		require.Equal(t, targetURL, carolContacts[0].PrimaryCozyURL())
		require.True(t, carolContacts[0].IsTrusted())

		targetContacts, err := contact.FindAllByEmail(target, "alice@example.com")
		if err == nil {
			for _, doc := range targetContacts {
				require.False(t, doc.IsExternal())
			}
		} else {
			require.True(t, errors.Is(err, contact.ErrNotFound))
		}

		targetBobContacts, err := contact.FindAllByEmail(target, "bob@example.com")
		require.NoError(t, err)
		require.Len(t, targetBobContacts, 1)
		require.True(t, targetBobContacts[0].IsExternal())
		require.Equal(t, "Bob", targetBobContacts[0].PrimaryName())
		require.Equal(t, "+33987654321", targetBobContacts[0].PrimaryPhoneNumber())
		require.Equal(t, bobURL, targetBobContacts[0].PrimaryCozyURL())
		require.True(t, targetBobContacts[0].IsTrusted())

		targetCarolContacts, err := contact.FindAllByEmail(target, "carol@example.com")
		require.NoError(t, err)
		require.Len(t, targetCarolContacts, 1)
		require.True(t, targetCarolContacts[0].IsExternal())
		require.Equal(t, "Carol", targetCarolContacts[0].PrimaryName())
		require.Equal(t, carolURL, targetCarolContacts[0].PrimaryCozyURL())
		require.True(t, targetCarolContacts[0].IsTrusted())
	})

	t.Run("CreatesExternalContactsWithOrganizationDomainOnly", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-domain-" + suffix + ".example"
		orgID := "org-sync-created-domain-" + suffix
		target := createInstanceInOrg(t, "sync-created-domain-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-created-domain-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")
		targetURL := target.PageURL("", nil)

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:      "alice@example.com",
			WorkplaceFqdn:      target.Domain,
			OrganizationDomain: orgDomain,
		})
		require.NoError(t, err)

		bobContacts, err := contact.FindAllByEmail(bob, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, bobContacts, 1)
		require.True(t, bobContacts[0].IsExternal())
		require.True(t, bobContacts[0].IsTrusted())
		require.Equal(t, "Alice", bobContacts[0].PrimaryName())
		require.Equal(t, targetURL, bobContacts[0].PrimaryCozyURL())
		require.True(t, orgdirectory.IsManagedDirectoryDoc(&bobContacts[0].JSONDoc))
	})

	t.Run("AdoptsExistingExternalContact", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-existing-" + suffix + ".example"
		orgID := "org-sync-created-existing-" + suffix
		target := createInstanceInOrg(t, "sync-created-existing-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-created-existing-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")
		carol := createInstanceInOrg(t, "sync-created-existing-carol-"+suffix+".local", orgDomain, orgID, "carol@example.com", "Carol")
		targetURL := target.PageURL("", nil)

		existing := createContact(t, bob, "alice@example.com", "https://old.example", true, "Old Alice")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:  "alice@example.com",
			Mobile:         "+33123456789",
			WorkplaceFqdn:  target.Domain,
			OrganizationID: orgID,
		})
		require.NoError(t, err)

		bobContacts, err := contact.FindAllByEmail(bob, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, bobContacts, 1)
		require.Equal(t, existing.ID(), bobContacts[0].ID())
		require.True(t, bobContacts[0].IsExternal())
		require.Equal(t, "Alice", bobContacts[0].PrimaryName())
		require.Equal(t, targetURL, bobContacts[0].PrimaryCozyURL())
		require.True(t, bobContacts[0].IsTrusted())
		require.True(t, orgdirectory.IsManagedDirectoryDoc(&bobContacts[0].JSONDoc))

		carolContacts, err := contact.FindAllByEmail(carol, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, carolContacts, 1)
		require.True(t, carolContacts[0].IsExternal())
		require.Equal(t, targetURL, carolContacts[0].PrimaryCozyURL())
		require.True(t, carolContacts[0].IsTrusted())
	})

	t.Run("FailsOnMultipleExternalContactsForEmail", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-dup-" + suffix + ".example"
		orgID := "org-sync-created-dup-" + suffix
		target := createInstanceInOrg(t, "sync-created-dup-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-created-dup-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")

		createContact(t, bob, "alice@example.com", "https://old-a.example", true, "Alice External A")
		createContact(t, bob, "alice@example.com", "https://old-b.example", true, "Alice External B")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:  "alice@example.com",
			WorkplaceFqdn:  target.Domain,
			OrganizationID: orgID,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "multiple managed contacts found for email alice@example.com")
	})

	t.Run("ContinuesAfterInstanceError", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-continue-" + suffix + ".example"
		orgID := "org-sync-created-continue-" + suffix
		target := createInstanceInOrg(t, "sync-created-continue-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-created-continue-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")
		carol := createInstanceInOrg(t, "sync-created-continue-carol-"+suffix+".local", orgDomain, orgID, "carol@example.com", "Carol")

		createContact(t, bob, "alice@example.com", "https://old-a.example", true, "Alice External A")
		createContact(t, bob, "alice@example.com", "https://old-b.example", true, "Alice External B")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:  "alice@example.com",
			WorkplaceFqdn:  target.Domain,
			OrganizationID: orgID,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "multiple managed contacts found for email alice@example.com")

		carolContacts, err := contact.FindAllByEmail(carol, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, carolContacts, 1)
		require.True(t, carolContacts[0].IsExternal())
		require.Equal(t, target.PageURL("", nil), carolContacts[0].PrimaryCozyURL())
		require.True(t, carolContacts[0].IsTrusted())
	})

	t.Run("MissingInternalEmail", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-missing-email-" + suffix + ".example"
		orgID := "org-sync-created-missing-email-" + suffix
		target := createInstanceInOrg(t, "sync-created-missing-email-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			WorkplaceFqdn:  target.Domain,
			OrganizationID: orgID,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing internalEmail")
	})

	t.Run("MissingPublicName", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-missing-name-" + suffix + ".example"
		orgID := "org-sync-created-missing-name-" + suffix
		target := createInstanceInOrg(t, "sync-created-missing-name-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		clearInstancePublicName(t, target)

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:  "alice@example.com",
			WorkplaceFqdn:  target.Domain,
			OrganizationID: orgID,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing public_name in settings")
	})

	t.Run("MissingOrganizationIDAndDomain", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-missing-org-" + suffix + ".example"
		orgID := "org-sync-created-missing-org-" + suffix
		target := createInstanceInOrg(t, "sync-created-missing-org-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail: "alice@example.com",
			WorkplaceFqdn: target.Domain,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing organizationId or organization domain")
	})

	t.Run("OrganizationHasNoInstances", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-created-zero-" + suffix + ".example"
		orgID := "org-sync-created-zero-" + suffix
		target := createInstanceInOrg(t, "sync-created-zero-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")

		err := rabbitmq.SyncCreatedOrgContact(testCtx(t), target, rabbitmq.UserCreatedMessage{
			InternalEmail:  "alice@example.com",
			WorkplaceFqdn:  target.Domain,
			OrganizationID: "missing-sync-created-" + suffix,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "organization has no instances")
	})
}

func TestSyncDeletedOrgContact(t *testing.T) {
	t.Run("DeletesExternalContactsFromOtherMembers", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-deleted-" + suffix + ".example"
		orgID := "org-sync-deleted-" + suffix
		target := createInstanceInOrg(t, "sync-deleted-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-deleted-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")
		carol := createInstanceInOrg(t, "sync-deleted-carol-"+suffix+".local", orgDomain, orgID, "carol@example.com", "Carol")
		targetURL := target.PageURL("", nil)

		createContact(t, bob, "alice@example.com", targetURL, true, "Alice External")
		carolManual := createContact(t, carol, "alice@example.com", "https://manual.example", false, "Alice Manual")
		targetExternal := createContact(t, target, "alice@example.com", targetURL, true, "Alice Own External")

		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: target.Domain,
			InternalEmail: "alice@example.com",
			Domain:        orgDomain,
		})
		require.NoError(t, err)

		_, err = contact.FindAllByEmail(bob, "alice@example.com")
		require.True(t, errors.Is(err, contact.ErrNotFound))

		carolContacts, err := contact.FindAllByEmail(carol, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, carolContacts, 1)
		require.Equal(t, carolManual.ID(), carolContacts[0].ID())
		require.False(t, carolContacts[0].IsExternal())

		targetContacts, err := contact.FindAllByEmail(target, "alice@example.com")
		require.NoError(t, err)
		var foundTargetExternal bool
		for _, doc := range targetContacts {
			if doc.ID() == targetExternal.ID() {
				foundTargetExternal = true
			}
		}
		require.True(t, foundTargetExternal)
	})

	t.Run("NoExternalContactIsNoOp", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-deleted-noop-" + suffix + ".example"
		orgID := "org-sync-deleted-noop-" + suffix
		target := createInstanceInOrg(t, "sync-deleted-noop-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-deleted-noop-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")

		manual := createContact(t, bob, "alice@example.com", "https://manual.example", false, "Alice Manual")

		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: target.Domain,
			InternalEmail: "alice@example.com",
			Domain:        orgDomain,
		})
		require.NoError(t, err)

		bobContacts, err := contact.FindAllByEmail(bob, "alice@example.com")
		require.NoError(t, err)
		require.Len(t, bobContacts, 1)
		require.Equal(t, manual.ID(), bobContacts[0].ID())
		require.False(t, bobContacts[0].IsExternal())
	})

	t.Run("FailsOnMultipleExternalContactsForEmail", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-deleted-dup-" + suffix + ".example"
		orgID := "org-sync-deleted-dup-" + suffix
		target := createInstanceInOrg(t, "sync-deleted-dup-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-deleted-dup-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")

		createContact(t, bob, "alice@example.com", target.PageURL("", nil), true, "Alice External A")
		createContact(t, bob, "alice@example.com", "https://old-b.example", true, "Alice External B")

		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: target.Domain,
			InternalEmail: "alice@example.com",
			Domain:        orgDomain,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "multiple external contacts found for email alice@example.com")
	})

	t.Run("ContinuesAfterInstanceError", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-deleted-continue-" + suffix + ".example"
		orgID := "org-sync-deleted-continue-" + suffix
		target := createInstanceInOrg(t, "sync-deleted-continue-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "sync-deleted-continue-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")
		carol := createInstanceInOrg(t, "sync-deleted-continue-carol-"+suffix+".local", orgDomain, orgID, "carol@example.com", "Carol")

		createContact(t, bob, "alice@example.com", target.PageURL("", nil), true, "Alice External A")
		createContact(t, bob, "alice@example.com", "https://old-b.example", true, "Alice External B")
		createContact(t, carol, "alice@example.com", target.PageURL("", nil), true, "Alice External")

		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: target.Domain,
			InternalEmail: "alice@example.com",
			Domain:        orgDomain,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "multiple external contacts found for email alice@example.com")

		_, err = contact.FindAllByEmail(carol, "alice@example.com")
		require.True(t, errors.Is(err, contact.ErrNotFound))
	})

	t.Run("MissingWorkplaceFqdn", func(t *testing.T) {
		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			InternalEmail: "alice@example.com",
			Domain:        "example.com",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing workplaceFqdn")
	})

	t.Run("MissingInternalEmail", func(t *testing.T) {
		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: "alice.example.com",
			Domain:        "example.com",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing internalEmail")
	})

	t.Run("MissingOrganizationDomain", func(t *testing.T) {
		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: "alice.example.com",
			InternalEmail: "alice@example.com",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing organizationId or organization domain")
	})

	t.Run("OrganizationHasNoInstances", func(t *testing.T) {
		config.UseTestFile(t)
		testutils.NeedCouchdb(t)

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "sync-deleted-zero-" + suffix + ".example"
		orgID := "org-sync-deleted-zero-" + suffix
		target := createInstanceInOrg(t, "sync-deleted-zero-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")

		err := rabbitmq.SyncDeletedOrgContact(testCtx(t), rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: target.Domain,
			InternalEmail: "alice@example.com",
			Domain:        "missing-sync-deleted-" + suffix + ".example",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "organization has no instances")
	})
}

func clearInstancePublicName(t *testing.T, inst *instance.Instance) {
	t.Helper()
	doc, err := inst.SettingsDocument()
	require.NoError(t, err)
	doc.M["public_name"] = ""
	require.NoError(t, couchdb.UpdateDoc(inst, doc))
}
