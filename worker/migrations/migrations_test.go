package migrations

import (
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackfillInternalEmails(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	orgID := fmt.Sprintf("org%d", time.Now().UnixNano())
	create := func(slug, email string) *instance.Instance {
		t.Helper()
		domain := slug + ".example"
		inst, err := lifecycle.Create(&lifecycle.Options{Domain: domain, Email: email, OrgID: orgID})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(domain) })
		return inst
	}
	internalEmail := func(inst *instance.Instance) string {
		t.Helper()
		got, err := lifecycle.GetInstance(inst.Domain)
		require.NoError(t, err)
		return got.InternalEmail
	}

	org := create(orgID, "admin@"+orgID+".example")
	alice := create("alice-"+orgID, " Alice@"+orgID+".example")
	bob := create("bob-"+orgID, "bob@"+orgID+".example")
	bobAgain := create("bob2-"+orgID, "bob@"+orgID+".example")
	carol := create("carol-"+orgID, "carol.old@"+orgID+".example")
	require.NoError(t, lifecycle.SetInternalEmail(carol, "carol@"+orgID+".example"))

	require.NoError(t, backfillInternalEmails(org))

	assert.Equal(t, "alice@"+orgID+".example", internalEmail(alice))
	assert.Empty(t, internalEmail(org))
	assert.Empty(t, internalEmail(bob))
	assert.Empty(t, internalEmail(bobAgain))
	assert.Equal(t, "carol@"+orgID+".example", internalEmail(carol))

	found, err := lifecycle.GetInstanceByInternalEmail("ALICE@" + orgID + ".example")
	require.NoError(t, err)
	assert.Equal(t, alice.Domain, found.Domain)
}
