package orgdrive

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoval(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	ctx := context.Background()

	t.Run("CutsTheAccessAndQueuesTheRemoval", func(t *testing.T) {
		org := newOrgInstance(t)
		inDirectory(t, org, "alice@example.net")
		require.NoError(t, NewHandler(&fakePublisher{}).Handle(ctx, ensureWith(t, org, "sp", 1, []Member{
			{Email: "alice@example.net", Role: RoleWrite},
		}, nil)))

		require.NoError(t, NewHandler(&fakePublisher{}).Handle(ctx, removed(t, org, "sp", 2)))

		s := driveOf(t, org, "sp")
		assert.False(t, s.Active)
		assert.Equal(t, int64(2), s.Reference.Revision)
		require.Eventually(t, func() bool {
			return vfs.IsInTrash(rootDir(t, org, s).Fullpath)
		}, 10*time.Second, 100*time.Millisecond)
	})

	t.Run("RevokesTheMembersTrashesTheFolderAndReplies", func(t *testing.T) {
		org := newOrgInstance(t)
		inDirectory(t, org, "alice@example.net")
		inDirectory(t, org, "bob@example.net")
		require.NoError(t, NewHandler(&fakePublisher{}).Handle(ctx, ensureWith(t, org, "sp", 1, []Member{
			{Email: "alice@example.net", Role: RoleWrite},
			{Email: "bob@example.net", Role: RoleRead},
		}, nil)))
		msg := markRemoved(t, org, "sp", 2)
		pub := &fakePublisher{}

		pending := driveOf(t, org, "sp")
		assert.False(t, pending.Active)
		assert.NotEqual(t, sharing.MemberStatusRevoked, pending.Members[1].Status)
		assert.False(t, vfs.IsInTrash(rootDir(t, org, pending).Fullpath))
		_, err := permission.CreateShareInteractSet(org, pending.ID(), nil, permission.Permission{Permissions: pending.CreateInteractSet()})
		require.NoError(t, err)
		require.NoError(t, remove(ctx, pub, org, msg))

		s := driveOf(t, org, "sp")
		for _, m := range s.Members[1:] {
			assert.Equal(t, sharing.MemberStatusRevoked, m.Status, m.Email)
		}
		for _, c := range s.Credentials {
			assert.Equal(t, sharing.Credentials{}, c)
		}
		assert.Empty(t, s.Triggers.TrackIDs)
		dir := rootDir(t, org, s)
		assert.True(t, vfs.IsInTrash(dir.Fullpath))
		assertNoRemovalTriggers(t, org)

		require.Len(t, pub.requests, 1)
		assert.Equal(t, rabbitmq.RoutingKeyOrgDriveRemoved, pub.requests[0].RoutingKey)
		data := pub.requests[0].Payload.(rabbitmq.CloudEvent).Data.(RemovedData)
		assert.Equal(t, RemovedData{Reference: Reference{Source: "twake-space", ID: "sp"}, Revision: 2}, data)

		t.Run("RedeliveryRepliesAgain", func(t *testing.T) {
			_, err := permission.GetForShareInteract(org, s.ID())
			require.ErrorIs(t, err, permission.ErrExpiredToken)
			markRemoved(t, org, "sp", 2)
			require.NoError(t, remove(ctx, pub, org, msg))
			assert.Len(t, pub.requests, 2)
			assertNoRemovalTriggers(t, org)
		})
	})

	t.Run("ExpiredPermissionsDoNotHideIncompleteRevocation", func(t *testing.T) {
		org := newOrgInstance(t)
		inDirectory(t, org, "alice@example.net")
		require.NoError(t, NewHandler(&fakePublisher{}).Handle(ctx, ensureWith(t, org, "sp", 1, []Member{
			{Email: "alice@example.net", Role: RoleWrite},
		}, nil)))
		msg := markRemoved(t, org, "sp", 2)
		s := driveOf(t, org, "sp")
		_, err := permission.CreateShareInteractSet(org, s.ID(), nil, permission.Permission{Permissions: s.CreateInteractSet()})
		require.NoError(t, err)
		require.NoError(t, s.RevokeInteractPermissions(org))
		pub := &fakePublisher{}

		require.ErrorIs(t, remove(ctx, pub, org, msg), permission.ErrExpiredToken)
		assert.Empty(t, pub.requests)
		assert.False(t, vfs.IsInTrash(rootDir(t, org, s).Fullpath))
	})

	t.Run("OlderRevisionIsIgnored", func(t *testing.T) {
		org := newOrgInstance(t)
		require.NoError(t, NewHandler(&fakePublisher{}).Handle(ctx, ensure(t, org, "sp", 3, "Space")))

		ok, err := MarkRemoved(org, mustCommand(t, removed(t, org, "sp", 2)))
		require.NoError(t, err)
		assert.False(t, ok)
		assert.True(t, onlyDrive(t, org).Active)
	})

	t.Run("NoDriveStillReplies", func(t *testing.T) {
		org := newOrgInstance(t)
		pub := &fakePublisher{}

		require.NoError(t, remove(ctx, pub, org, &RemovalMessage{
			OrganizationID: org.OrgID,
			Reference:      Reference{Source: "twake-space", ID: "unknown"},
			Revision:       1,
		}))

		assert.Len(t, pub.requests, 1)
	})

	t.Run("UsesNormalTrashRetention", func(t *testing.T) {
		cfg := config.GetConfig()
		previous := cfg.Fs.AutoCleanTrashedAfter
		t.Cleanup(func() { cfg.Fs.AutoCleanTrashedAfter = previous })
		for _, after := range []string{"", "7d"} {
			t.Run("retention="+after, func(t *testing.T) {
				org := newOrgInstance(t)
				cfg.Fs.AutoCleanTrashedAfter = map[string]string{org.ContextName: after}
				for _, id := range []string{"first", "second"} {
					require.NoError(t, NewHandler(&fakePublisher{}).Handle(ctx, ensure(t, org, id, 1, id)))
					require.NoError(t, remove(ctx, &fakePublisher{}, org, markRemoved(t, org, id, 2)))
					assert.True(t, vfs.IsInTrash(rootDir(t, org, driveOf(t, org, id)).Fullpath))
				}
				triggers, err := job.System().GetAllTriggers(org)
				require.NoError(t, err)
				count := 0
				for _, trigger := range triggers {
					if trigger.Infos().WorkerType == "clean-old-trashed" {
						count++
					}
				}
				if after == "" {
					assert.Zero(t, count)
				} else {
					assert.Equal(t, 1, count)
				}
				assertNoRemovalTriggers(t, org)
			})
		}
	})
}

func removed(t *testing.T, org *instance.Instance, ref string, revision int64) amqp.Delivery {
	t.Helper()
	return amqp.Delivery{RoutingKey: rabbitmq.RoutingKeyOrgDriveEnsure, Body: mustJSON(t, map[string]any{
		"organizationId": org.OrgID,
		"reference":      map[string]any{"source": "twake-space", "id": ref},
		"revision":       revision,
		"state":          "removed",
	})}
}

func driveOf(t *testing.T, org *instance.Instance, ref string) *sharing.Sharing {
	t.Helper()
	drives, err := sharing.FindByReference(org, "twake-space", ref)
	require.NoError(t, err)
	require.Len(t, drives, 1)
	return drives[0]
}

func mustCommand(t *testing.T, d amqp.Delivery) *Command {
	t.Helper()
	cmd, err := ParseCommand(d.Body)
	require.NoError(t, err)
	return cmd
}

// markRemoved does the handler's part without queuing the job, so that the
// test runs the removal itself.
func markRemoved(t *testing.T, org *instance.Instance, ref string, revision int64) *RemovalMessage {
	t.Helper()
	cmd := mustCommand(t, removed(t, org, ref, revision))
	ok, err := MarkRemoved(org, cmd)
	require.NoError(t, err)
	require.True(t, ok)
	return &RemovalMessage{OrganizationID: org.OrgID, Reference: cmd.Reference, Revision: revision}
}

func assertNoRemovalTriggers(t *testing.T, org *instance.Instance) {
	t.Helper()
	triggers, err := job.System().GetAllTriggers(org)
	require.NoError(t, err)
	for _, trigger := range triggers {
		assert.NotEqual(t, WorkerRemoval, trigger.Infos().WorkerType)
	}
}
