package orgdirectory

import (
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

func TestRemoveMemberCopies(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	const onCtx, offCtx = "member-copies-on", "member-copies-off"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		onCtx:  map[string]interface{}{"common_contacts": true},
		offCtx: map[string]interface{}{},
	}
	t.Cleanup(func() { conf.Contexts = previous })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "copies" + suffix
	orgDomain := "copies-" + suffix + ".example"
	newInstance := func(domain, contextName string) *instance.Instance {
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      domain,
			OrgDomain:   orgDomain,
			OrgID:       orgID,
			ContextName: contextName,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
		return inst
	}
	copyMember := func(inst *instance.Instance, email string) *contact.Contact {
		c, err := UpsertManagedContact(inst, ContactPatch{OrganizationID: orgID, Email: email, Name: email})
		require.NoError(t, err)
		return c
	}
	exists := func(inst *instance.Instance, c *contact.Contact) bool {
		_, err := contact.Find(inst, c.ID())
		return err == nil
	}

	org := newInstance(orgID+".local", onCtx)
	alice := newInstance("alice-copies-"+suffix+".local", onCtx)
	bob := newInstance("bob-copies-"+suffix+".local", offCtx)

	orgMember := copyMember(org, "carol@acme.test")
	aliceCopy := copyMember(alice, "carol@acme.test")
	bobCopy := copyMember(bob, "carol@acme.test")
	takenOver := copyMember(alice, "dave@acme.test")
	takenOver.M[contact.CardDAVPathKey] = "addressbooks/alice/collected/dave.vcf"
	require.NoError(t, couchdb.UpdateDoc(alice, takenOver))
	personal, err := contact.Create(alice, contact.CreateOptions{Email: "eve@example.test"})
	require.NoError(t, err)
	shared, err := contact.Create(alice, contact.CreateOptions{Email: "frank@example.test", External: true})
	require.NoError(t, err)

	report, err := RemoveMemberCopies(testCtx(t), orgID, true)
	require.NoError(t, err)
	require.Equal(t, map[string]int{alice.Domain: 1}, report.Removed)
	require.True(t, exists(alice, aliceCopy), "a dry run deletes nothing")

	report, err = RemoveMemberCopies(testCtx(t), orgID, false)
	require.NoError(t, err)
	require.Equal(t, map[string]int{alice.Domain: 1}, report.Removed)
	require.Empty(t, report.Errors)
	require.False(t, exists(alice, aliceCopy))
	require.True(t, exists(alice, takenOver))
	require.True(t, exists(alice, personal))
	require.True(t, exists(alice, shared))
	require.True(t, exists(org, orgMember))
	require.True(t, exists(bob, bobCopy))

	report, err = RemoveMemberCopies(testCtx(t), orgID, false)
	require.NoError(t, err)
	require.Empty(t, report.Removed)
}

func TestRemoveMemberCopiesNeedsTheOrgInstanceOnTheFeed(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "nofeed" + suffix
	orgDomain := "nofeed-" + suffix + ".example"
	createOrgDirectoryInstance(t, orgID+".local", orgDomain, orgID, "owner@acme.test", "Owner")

	_, err := RemoveMemberCopies(testCtx(t), orgID, false)
	require.Error(t, err)
}
