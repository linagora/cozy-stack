package orgdirectory

import (
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/cache"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestReconcileOrganizationContacts(t *testing.T) {
	for _, centralized := range []bool{false, true} {
		t.Run(fmt.Sprintf("centralized=%t", centralized), func(t *testing.T) { testReconcileOrganizationContacts(t, centralized) })
	}
}

func testReconcileOrganizationContacts(t *testing.T, centralized bool) {
	config.UseTestFile(t)
	needCouchDB(t)

	const onCtx = "reconcile-on"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{onCtx: map[string]interface{}{"common_contacts": true, "centralized_org_directory": centralized}}
	t.Cleanup(func() { conf.Contexts = previous })

	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	orgID := "reconcile" + suffix
	org, err := lifecycle.Create(&lifecycle.Options{
		Domain:      orgID + ".local",
		OrgDomain:   "reconcile-" + suffix + ".example",
		OrgID:       orgID,
		ContextName: onCtx,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(org.Domain) })

	fromFeed := func(email string) *contact.Contact {
		c, err := contact.Create(org, contact.CreateOptions{Email: email, External: true})
		require.NoError(t, err)
		c.M[contact.CardDAVPathKey] = "addressbooks/domain/" + email + ".vcf"
		require.NoError(t, couchdb.UpdateDoc(org, c))
		return c
	}
	exists := func(c *contact.Contact) bool {
		_, err := contact.Find(org, c.ID())
		return err == nil
	}

	since := time.Now().Add(-time.Second)
	paths := []string{"addressbooks/domain/carol@acme.test.vcf"}
	conf.Contexts[onCtx].(map[string]interface{})["common_contacts"] = false
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 1, paths, false)
	require.ErrorIs(t, err, ErrNoCommonContacts, "centralization alone must not enable service reconciliation")
	conf.Contexts[onCtx].(map[string]interface{})["common_contacts"] = true
	sent := fromFeed("carol@acme.test")
	gone := fromFeed("dave@acme.test")
	notFromFeed, err := contact.Create(org, contact.CreateOptions{Email: "eve@acme.test", External: true})
	require.NoError(t, err)

	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 1, paths, false)
	require.ErrorIs(t, err, ErrIncompleteRepublication, "nothing sent since, so no republication reached the stack")
	require.True(t, exists(gone))

	MarkContactSeen(sent)
	require.NoError(t, couchdb.UpdateDoc(org, sent))
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 2, append(paths, gone.M[contact.CardDAVPathKey].(string)), false)
	require.ErrorIs(t, err, ErrIncompleteRepublication, "consumer lag must not authorize deletion")
	require.True(t, exists(gone))
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 0, nil, false)
	require.ErrorIs(t, err, ErrIncompleteRepublication, "extra observations must also refuse deletion")
	unrelated := fromFeed("unrelated@acme.test")
	MarkContactSeen(unrelated)
	require.NoError(t, couchdb.UpdateDoc(org, unrelated))
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 2, append(paths, gone.M[contact.CardDAVPathKey].(string)), false)
	require.ErrorIs(t, err, ErrIncompleteRepublication, "an unrelated live update must not hide a missing snapshot path")
	require.True(t, exists(gone))
	require.NoError(t, couchdb.DeleteDoc(org, unrelated))
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 2, append(paths, paths...), false)
	require.ErrorIs(t, err, ErrIncompleteRepublication, "duplicate source paths are invalid")
	duplicate := fromFeed("duplicate@acme.test")
	duplicate.M[contact.CardDAVPathKey] = paths[0]
	require.NoError(t, couchdb.UpdateDoc(org, duplicate))
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 1, paths, false)
	require.ErrorIs(t, err, ErrIncompleteRepublication, "a stale duplicate of a snapshot path must not be deleted")
	require.True(t, exists(duplicate))
	require.NoError(t, couchdb.DeleteDoc(org, duplicate))
	previousCache := conf.CacheStorage
	conf.CacheStorage = cache.NewInMemory()
	t.Cleanup(func() { conf.CacheStorage = previousCache })

	report, err := ReconcileOrganizationContacts(testCtx(t), orgID, since, 1, paths, true)
	require.NoError(t, err)
	require.Equal(t, []string{"addressbooks/domain/dave@acme.test.vcf"}, report.Removed)
	require.True(t, exists(gone), "a dry run deletes nothing")

	report, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 1, paths, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Kept)
	require.False(t, exists(gone))
	require.True(t, exists(sent))
	require.True(t, exists(notFromFeed))

	report, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, 1, paths, false)
	require.NoError(t, err)
	require.Empty(t, report.Removed)

	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, time.Now().Add(-8*24*time.Hour), 1, paths, false)
	require.ErrorIs(t, err, ErrSinceTooOld)
	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, time.Now().Add(time.Hour), 0, nil, false)
	require.ErrorIs(t, err, ErrSinceInFuture)

	report, err = ReconcileOrganizationContacts(testCtx(t), orgID, time.Now(), 0, nil, false)
	require.NoError(t, err, "a completed empty republication has an explicit zero count")
	require.Len(t, report.Removed, 1)
	require.True(t, exists(notFromFeed))
}
