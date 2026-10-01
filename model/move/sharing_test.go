package move

import (
	"testing"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/gofrs/uuid/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func uuidv7() string {
	id, _ := uuid.NewV7()
	return id.String()
}

func createTestSharing(t *testing.T, inst prefixer.Prefixer, s *sharing.Sharing) {
	// Use CreateNamedDoc to create a sharing with a specific ID
	s.SetID(s.SID)
	err := couchdb.CreateNamedDocWithDB(inst, s)
	require.NoError(t, err)
}

func TestUpdateSelfMemberInstance(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	// Reset the sharings database. ResetDB retries CreateDB on the CouchDB
	// file_exists race that makes a plain DeleteDB+CreateDB flaky.
	err := couchdb.ResetDB(inst, consts.Sharings)
	require.NoError(t, err)

	newInstance := inst.PageURL("", nil)
	oldDomain := "old-domain.mycozy.cloud"

	// Simulate a migrated instance by setting OldDomain
	inst.OldDomain = oldDomain

	t.Run("UpdateOwnerMemberInstance", func(t *testing.T) {
		// Create a sharing where we are the owner with old instance URL
		s := &sharing.Sharing{
			SID:   uuidv7(),
			Owner: true,
			Members: []sharing.Member{
				{
					Status:   sharing.MemberStatusOwner,
					Instance: "https://" + oldDomain + "/",
					Email:    "owner@test.com",
				},
				{
					Status:   sharing.MemberStatusReady,
					Instance: "https://recipient.mycozy.cloud",
					Email:    "recipient@test.com",
				},
			},
			Credentials: []sharing.Credentials{
				{InboundClientID: "client-123"},
			},
		}
		createTestSharing(t, inst, s)

		// Run the fixer
		err = UpdateSelfMemberInstance(inst)
		assert.NoError(t, err)

		// Verify the owner's instance URL was updated
		var updated sharing.Sharing
		err = couchdb.GetDoc(inst, consts.Sharings, s.SID, &updated)
		require.NoError(t, err)
		assert.Equal(t, newInstance, updated.Members[0].Instance)
		// Recipient should not be changed
		assert.Equal(t, "https://recipient.mycozy.cloud", updated.Members[1].Instance)
	})

	t.Run("UpdateRecipientMemberInstance", func(t *testing.T) {
		// Create a sharing where we are a recipient with old instance URL
		s := &sharing.Sharing{
			SID:   uuidv7(),
			Owner: false,
			Members: []sharing.Member{
				{
					Status:   sharing.MemberStatusOwner,
					Instance: "https://owner.mycozy.cloud",
					Email:    "owner@test.com",
				},
				{
					Status:   sharing.MemberStatusReady,
					Instance: "https://" + oldDomain + "/", // This is us (matches OldDomain)
					Email:    "self@test.com",
				},
				{
					Status:   sharing.MemberStatusReady,
					Instance: "https://other-recipient.mycozy.cloud",
					Email:    "other@test.com",
				},
			},
			Credentials: []sharing.Credentials{
				{InboundClientID: "inbound-client-123"},
			},
		}
		createTestSharing(t, inst, s)

		// Run the fixer
		err = UpdateSelfMemberInstance(inst)
		assert.NoError(t, err)

		// Verify our member instance was updated
		var updated sharing.Sharing
		err = couchdb.GetDoc(inst, consts.Sharings, s.SID, &updated)
		require.NoError(t, err)
		assert.Equal(t, "https://owner.mycozy.cloud", updated.Members[0].Instance)
		assert.Equal(t, newInstance, updated.Members[1].Instance) // This was us, should be updated
		assert.Equal(t, "https://other-recipient.mycozy.cloud", updated.Members[2].Instance)
	})

	t.Run("SkipWhenNoOldDomain", func(t *testing.T) {
		// Clear OldDomain to test skip behavior
		inst.OldDomain = ""

		s := &sharing.Sharing{
			SID:   uuidv7(),
			Owner: true,
			Members: []sharing.Member{
				{
					Status:   sharing.MemberStatusOwner,
					Instance: "https://some-old-domain.mycozy.cloud/",
					Email:    "owner@test.com",
				},
			},
		}
		createTestSharing(t, inst, s)

		oldRev := s.Rev()

		// Run the fixer - should skip because no OldDomain is set
		err = UpdateSelfMemberInstance(inst)
		assert.NoError(t, err)

		// Verify the document was not updated (same rev)
		var updated sharing.Sharing
		err = couchdb.GetDoc(inst, consts.Sharings, s.SID, &updated)
		require.NoError(t, err)
		assert.Equal(t, oldRev, updated.Rev())

		// Restore OldDomain for other tests
		inst.OldDomain = oldDomain
	})

	t.Run("SkipAlreadyUpdatedSharing", func(t *testing.T) {
		// Create a sharing where the instance URL is already correct
		s := &sharing.Sharing{
			SID:   uuidv7(),
			Owner: true,
			Members: []sharing.Member{
				{
					Status:   sharing.MemberStatusOwner,
					Instance: newInstance, // Already correct
					Email:    "owner@test.com",
				},
			},
		}
		createTestSharing(t, inst, s)

		oldRev := s.Rev()

		// Run the fixer
		err = UpdateSelfMemberInstance(inst)
		assert.NoError(t, err)

		// Verify the document was not updated (same rev)
		var updated sharing.Sharing
		err = couchdb.GetDoc(inst, consts.Sharings, s.SID, &updated)
		require.NoError(t, err)
		assert.Equal(t, oldRev, updated.Rev())
	})

	t.Run("NoFalsePositiveSubstringMatch", func(t *testing.T) {
		// Test that a member with a domain containing OldDomain as substring
		// is NOT matched (e.g., alice-old-domain.mycozy.cloud should not match old-domain.mycozy.cloud)
		s := &sharing.Sharing{
			SID:   uuidv7(),
			Owner: false,
			Members: []sharing.Member{
				{
					Status:   sharing.MemberStatusOwner,
					Instance: "https://owner.mycozy.cloud",
					Email:    "owner@test.com",
				},
				{
					Status:   sharing.MemberStatusReady,
					Instance: "https://alice-" + oldDomain + "/", // Contains oldDomain as substring but different host
					Email:    "alice@test.com",
				},
				{
					Status:   sharing.MemberStatusReady,
					Instance: "https://" + oldDomain + "/", // Exact match - this is us
					Email:    "self@test.com",
				},
			},
			Credentials: []sharing.Credentials{
				{InboundClientID: "inbound-client-123"},
			},
		}
		createTestSharing(t, inst, s)

		// Run the fixer
		err = UpdateSelfMemberInstance(inst)
		assert.NoError(t, err)

		// Verify only the exact match was updated, not the substring match
		var updated sharing.Sharing
		err = couchdb.GetDoc(inst, consts.Sharings, s.SID, &updated)
		require.NoError(t, err)
		assert.Equal(t, "https://owner.mycozy.cloud", updated.Members[0].Instance)
		assert.Equal(t, "https://alice-"+oldDomain+"/", updated.Members[1].Instance) // Should NOT be updated
		assert.Equal(t, newInstance, updated.Members[2].Instance)                    // Should be updated (exact match)
	})
}

func TestUpdateTriggersAfterMove(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	sched := job.System()
	newDomain := inst.ContextualDomain()

	t.Run("UpdateTriggerWithOldDomain", func(t *testing.T) {
		// Create a trigger through the scheduler
		triggerInfos := job.TriggerInfos{
			Type:       "@event",
			WorkerType: "share-track",
			Arguments:  "io.cozy.files:CREATED,UPDATED",
		}

		trigger, err := job.NewTrigger(inst, triggerInfos, nil)
		require.NoError(t, err)
		err = sched.AddTrigger(trigger)
		require.NoError(t, err)

		triggerID := trigger.Infos().TID

		// Simulate migration: manually set the domain to an old value
		// This is what happens when instance domain changes after trigger creation
		oldDomain := "old-domain.mycozy.cloud"
		trigger.Infos().Domain = oldDomain

		// Also update in CouchDB to simulate the real state
		infos := trigger.Infos()
		err = couchdb.UpdateDoc(inst, infos)
		require.NoError(t, err)

		// Verify the trigger has the old domain
		createdTrigger, err := sched.GetTrigger(inst, triggerID)
		require.NoError(t, err)
		assert.Equal(t, oldDomain, createdTrigger.Infos().Domain)

		// Run the fixer
		err = UpdateTriggersAfterMove(inst)
		assert.NoError(t, err)

		// Verify the trigger domain was updated in the scheduler memory
		updatedTrigger, err := sched.GetTrigger(inst, triggerID)
		require.NoError(t, err)
		assert.Equal(t, newDomain, updatedTrigger.Infos().Domain)

		// Verify the trigger domain was updated in CouchDB
		var updated job.TriggerInfos
		err = couchdb.GetDoc(inst, consts.Triggers, triggerID, &updated)
		require.NoError(t, err)
		assert.Equal(t, newDomain, updated.Domain)

		// Clean up
		err = sched.DeleteTrigger(inst, triggerID)
		assert.NoError(t, err)
	})

	t.Run("SkipTriggerWithCorrectDomain", func(t *testing.T) {
		// Create a trigger with the correct domain
		triggerInfos := job.TriggerInfos{
			Type:       "@event",
			WorkerType: "share-track",
			Arguments:  "io.cozy.files:CREATED,UPDATED",
		}

		trigger, err := job.NewTrigger(inst, triggerInfos, nil)
		require.NoError(t, err)
		err = sched.AddTrigger(trigger)
		require.NoError(t, err)

		triggerID := trigger.Infos().TID

		// Get the initial rev from CouchDB
		var initial job.TriggerInfos
		err = couchdb.GetDoc(inst, consts.Triggers, triggerID, &initial)
		require.NoError(t, err)
		oldRev := initial.TRev

		// Run the fixer
		err = UpdateTriggersAfterMove(inst)
		assert.NoError(t, err)

		// Verify the document was not updated (same rev)
		var updated job.TriggerInfos
		err = couchdb.GetDoc(inst, consts.Triggers, triggerID, &updated)
		require.NoError(t, err)
		assert.Equal(t, oldRev, updated.TRev)

		// Clean up
		err = sched.DeleteTrigger(inst, triggerID)
		assert.NoError(t, err)
	})
}
