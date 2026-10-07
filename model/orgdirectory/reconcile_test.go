package orgdirectory

import (
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestReconcileOrganizationContacts(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)

	const onCtx = "reconcile-on"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{onCtx: map[string]interface{}{"common_contacts": true}}
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
	sent := fromFeed("carol@acme.test")
	gone := fromFeed("dave@acme.test")
	notFromFeed, err := contact.Create(org, contact.CreateOptions{Email: "eve@acme.test", External: true})
	require.NoError(t, err)

	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, false)
	require.Error(t, err, "nothing sent since, so no republication reached the stack")
	require.True(t, exists(gone))

	MarkContactSeen(org, sent.M[contact.CardDAVPathKey].(string))

	report, err := ReconcileOrganizationContacts(testCtx(t), orgID, since, true)
	require.NoError(t, err)
	require.Equal(t, []string{"addressbooks/domain/dave@acme.test.vcf"}, report.Removed)
	require.True(t, exists(gone), "a dry run deletes nothing")

	report, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, false)
	require.NoError(t, err)
	require.Equal(t, 1, report.Kept)
	require.False(t, exists(gone))
	require.True(t, exists(sent))
	require.True(t, exists(notFromFeed))

	report, err = ReconcileOrganizationContacts(testCtx(t), orgID, since, false)
	require.NoError(t, err)
	require.Empty(t, report.Removed)

	_, err = ReconcileOrganizationContacts(testCtx(t), orgID, time.Now().Add(-8*24*time.Hour), false)
	require.Error(t, err, "the seen markers do not go back that far")
}
