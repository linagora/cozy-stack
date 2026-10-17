package driveactivity

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/orgdrive"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakePublisher struct {
	requests []rabbitmq.PublishRequest
	err      error
}

func (p *fakePublisher) Publish(_ context.Context, req rabbitmq.PublishRequest) error {
	p.requests = append(p.requests, req)
	return p.err
}

func (p *fakePublisher) events(t *testing.T) []rabbitmq.CloudEvent {
	t.Helper()
	events := make([]rabbitmq.CloudEvent, len(p.requests))
	for i, req := range p.requests {
		assert.Equal(t, rabbitmq.ExchangeActivity, req.Exchange)
		assert.Equal(t, rabbitmq.RoutingKeyDriveFolderChanged, req.RoutingKey)
		events[i] = req.Payload.(rabbitmq.CloudEvent)
		assert.Equal(t, events[i].ID, req.MessageID)
	}
	return events
}

func TestFlush(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	ctx := context.Background()
	alice := &sharing.Member{Email: "alice@example.net"}
	bob := &sharing.Member{Email: "bob@example.net"}

	t.Run("PublishesOneEventPerMemberAndTopFolder", func(t *testing.T) {
		org := newOrgInstance(t)
		s := newDrive(t, org, true)
		root := driveRoot(t, org, s)
		q4 := mkdir(t, org, root, "Q4 assets")
		deep := mkdir(t, org, q4, "logos")
		other := mkdir(t, org, root, "Other")

		Count(org, s, alice, Created, &vfs.FileDoc{DirID: deep.ID(), DocName: "logo.svg"})
		Count(org, s, alice, Created, &vfs.FileDoc{DirID: q4.ID(), DocName: "brief.pdf"})
		Count(org, s, alice, Updated, &vfs.FileDoc{DirID: q4.ID(), DocName: "brief.pdf"})
		Count(org, s, alice, Created, &vfs.FileDoc{DirID: root.ID(), DocName: "readme.md"})
		Count(org, s, bob, Deleted, &vfs.FileDoc{DirID: other.ID(), DocName: "old.txt"})

		msg := flushMessage(t, org, s)
		pub := &fakePublisher{}
		require.NoError(t, flush(ctx, pub, org, msg))

		events := pub.events(t)
		require.Len(t, events, 3)
		byObject := map[string]rabbitmq.CloudEvent{}
		for _, e := range events {
			assert.Equal(t, "twake://drive", e.Source)
			assert.Equal(t, org.OrgID, e.TwakeOrg)
			assert.False(t, e.Time.IsZero())
			data := e.Data.(FolderChangedData)
			assert.Equal(t, Container{Kind: "drive", ID: s.SID}, data.Object.Container)
			assert.Equal(t, "folder", data.Object.Type)
			byObject[e.TwakeActor+" "+data.Object.ID] = e
		}

		q4Event := byObject["alice@example.net "+q4.ID()].Data.(FolderChangedData)
		assert.Equal(t, "Q4 assets", q4Event.Object.Title)
		assert.Equal(t, Files{Created: 2, Updated: 1}, q4Event.Files)
		assert.ElementsMatch(t, []string{"logo.svg", "brief.pdf"}, q4Event.Samples)

		rootEvent := byObject["alice@example.net "+root.ID()].Data.(FolderChangedData)
		assert.Equal(t, root.DocName, rootEvent.Object.Title)
		assert.Equal(t, Files{Created: 1}, rootEvent.Files)
		assert.Equal(t, []string{"readme.md"}, rootEvent.Samples)

		otherEvent := byObject["bob@example.net "+other.ID()].Data.(FolderChangedData)
		assert.Equal(t, Files{Deleted: 1}, otherEvent.Files)

		again := &fakePublisher{}
		require.NoError(t, flush(ctx, again, org, msg))
		assert.Empty(t, again.requests)
	})

	t.Run("ARetryPublishesTheSameEvents", func(t *testing.T) {
		org := newOrgInstance(t)
		s := newDrive(t, org, true)
		root := driveRoot(t, org, s)
		Count(org, s, alice, Created, &vfs.FileDoc{DirID: root.ID(), DocName: "a.txt"})
		msg := flushMessage(t, org, s)

		failing := &fakePublisher{err: assert.AnError}
		require.ErrorIs(t, flush(ctx, failing, org, msg), assert.AnError)
		Count(org, s, alice, Created, &vfs.FileDoc{DirID: root.ID(), DocName: "next.txt"})

		first, second := &fakePublisher{}, &fakePublisher{}
		require.NoError(t, flush(ctx, first, org, msg))
		require.Len(t, first.requests, 1)
		event := first.events(t)[0]
		assert.Equal(t, failing.events(t)[0].ID, event.ID)
		assert.Equal(t, Files{Created: 1}, event.Data.(FolderChangedData).Files)

		next := flushMessage(t, org, s)
		require.NoError(t, flush(ctx, second, org, next))
		require.Len(t, second.requests, 1)
		assert.NotEqual(t, event.ID, second.events(t)[0].ID)
		assert.Equal(t, []string{"next.txt"}, second.events(t)[0].Data.(FolderChangedData).Samples)
	})

	t.Run("SkipsTheFoldersThatLeftTheDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		s := newDrive(t, org, true)
		gone := mkdir(t, org, driveRoot(t, org, s), "Gone")
		Count(org, s, alice, Created, &vfs.FileDoc{DirID: gone.ID(), DocName: "a.txt"})
		_, err := vfs.TrashDir(org.VFS(), gone)
		require.NoError(t, err)

		pub := &fakePublisher{}
		require.NoError(t, flush(ctx, pub, org, flushMessage(t, org, s)))
		assert.Empty(t, pub.requests)
	})

	t.Run("IgnoresTheDrivesThatDoNotReportActivity", func(t *testing.T) {
		org := newOrgInstance(t)
		notReady := newDrive(t, org, false)
		Count(org, notReady, alice, Created, &vfs.FileDoc{DirID: driveRoot(t, org, notReady).ID(), DocName: "a.txt"})
		owner := newDrive(t, org, true)
		Count(org, owner, nil, Created, &vfs.FileDoc{DirID: driveRoot(t, org, owner).ID(), DocName: "a.txt"})

		assert.Empty(t, flushTriggers(t, org))
	})

	t.Run("ReadsTheWindowFromTheContext", func(t *testing.T) {
		org := newOrgInstance(t)
		assert.Equal(t, defaultWindow, windowOf(org))

		contexts := config.GetConfig().Contexts
		t.Cleanup(func() { config.GetConfig().Contexts = contexts })
		config.GetConfig().Contexts = map[string]interface{}{
			config.DefaultInstanceContext: map[string]interface{}{
				"drive_activity": map[string]interface{}{"window": "5s"},
			},
		}
		assert.Equal(t, 5*time.Second, windowOf(org))
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

func newDrive(t *testing.T, org *instance.Instance, ready bool) *sharing.Sharing {
	t.Helper()
	s, _, err := orgdrive.Ensure(org, &orgdrive.Command{
		Reference: orgdrive.Reference{Source: "app", ID: utils.RandomString(10)},
		Revision:  1,
		Name:      "Space",
		Activity:  true,
	})
	require.NoError(t, err)
	s.Reference.Ready = ready
	require.NoError(t, couchdb.UpdateDoc(org, s))
	return s
}

func driveRoot(t *testing.T, org *instance.Instance, s *sharing.Sharing) *vfs.DirDoc {
	t.Helper()
	rootID, err := s.DriveRootID()
	require.NoError(t, err)
	dir, err := org.VFS().DirByID(rootID)
	require.NoError(t, err)
	return dir
}

func mkdir(t *testing.T, org *instance.Instance, parent *vfs.DirDoc, name string) *vfs.DirDoc {
	t.Helper()
	dir, err := vfs.NewDirDocWithParent(name, parent, nil)
	require.NoError(t, err)
	require.NoError(t, org.VFS().CreateDir(dir))
	return dir
}

func flushTriggers(t *testing.T, org *instance.Instance) []job.Trigger {
	t.Helper()
	triggers, err := job.System().GetAllTriggers(org)
	require.NoError(t, err)
	var flushes []job.Trigger
	for _, trigger := range triggers {
		if trigger.Infos().WorkerType == WorkerFlush {
			flushes = append(flushes, trigger)
		}
	}
	return flushes
}

// flushMessage returns the message of the flush scheduled for the drive, and
// deletes its trigger so that the test runs the flush itself.
func flushMessage(t *testing.T, org *instance.Instance, s *sharing.Sharing) *FlushMessage {
	t.Helper()
	for _, trigger := range flushTriggers(t, org) {
		infos := trigger.Infos()
		var msg FlushMessage
		require.NoError(t, infos.Message.Unmarshal(&msg))
		if msg.SharingID != s.SID {
			continue
		}
		assert.Equal(t, "@in", infos.Type)
		assert.Equal(t, defaultWindow.String(), infos.Arguments)
		require.NoError(t, job.System().DeleteTrigger(org, infos.TID))
		return &msg
	}
	require.FailNow(t, "no flush scheduled for drive "+s.SID)
	return nil
}
