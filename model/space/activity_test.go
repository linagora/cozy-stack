package space

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	_ "github.com/cozy/cozy-stack/worker/broker"
	"github.com/stretchr/testify/require"
)

type syncPublisher struct {
	mu       sync.Mutex
	requests []rabbitmq.PublishRequest
}

func (p *syncPublisher) Publish(_ context.Context, req rabbitmq.PublishRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	return nil
}

func (p *syncPublisher) StartManagers() ([]*rabbitmq.RabbitMQManager, error) { return nil, nil }

func (p *syncPublisher) published() []rabbitmq.PublishRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]rabbitmq.PublishRequest(nil), p.requests...)
}

func TestSpaceDriveActivity(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	newFile := func(t *testing.T, dirID, name string) *vfs.FileDoc {
		t.Helper()
		doc, err := vfs.NewFileDoc(name, dirID, 0, nil, "text/plain", "text", time.Now(), false, false, false, nil)
		require.NoError(t, err)
		doc.SetID("file-" + utils.RandomString(8))
		return doc
	}
	alice := &sharing.Member{Email: "alice@acme.test"}

	t.Run("PublishesAFileCreatedInTheSpaceDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		pub := &syncPublisher{}
		t.Cleanup(rabbitmq.SetDefault(pub))

		s, err := ProvisionDrive(org, Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: "Launch"})
		require.NoError(t, err)
		file := newFile(t, s.Rules[0].Values[0], "plan.md")

		require.NoError(t, NotifyFileCreated(org, s, alice, file))

		require.Eventually(t, func() bool { return len(pub.published()) == 1 }, 10*time.Second, 50*time.Millisecond)
		req := pub.published()[0]
		require.Equal(t, org.ContextName, req.ContextName)
		require.Equal(t, "activity", req.Exchange)
		require.Equal(t, "com.twake.drive.file.created.v1", req.RoutingKey)

		var event struct {
			SpecVersion string `json:"specversion"`
			ID          string `json:"id"`
			Source      string `json:"source"`
			Type        string `json:"type"`
			Time        string `json:"time"`
			TwakeOrg    string `json:"twakeorg"`
			TwakeActor  string `json:"twakeactor"`
			Data        struct {
				Object struct {
					Type      string `json:"type"`
					ID        string `json:"id"`
					Title     string `json:"title"`
					Container struct {
						Kind string `json:"kind"`
						ID   string `json:"id"`
					} `json:"container"`
				} `json:"object"`
			} `json:"data"`
		}
		raw, err := json.Marshal(req.Payload)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &event))
		require.Equal(t, "1.0", event.SpecVersion)
		require.NotEmpty(t, event.ID)
		require.Equal(t, req.MessageID, event.ID)
		require.Equal(t, "twake://drive", event.Source)
		require.Equal(t, "com.twake.drive.file.created.v1", event.Type)
		require.NotEmpty(t, event.Time)
		require.Equal(t, org.OrgID, event.TwakeOrg)
		require.Equal(t, "alice@acme.test", event.TwakeActor)
		require.Equal(t, "file", event.Data.Object.Type)
		require.Equal(t, file.ID(), event.Data.Object.ID)
		require.Equal(t, "plan.md", event.Data.Object.Title)
		require.Equal(t, "drive", event.Data.Object.Container.Kind)
		require.Equal(t, s.SID, event.Data.Object.Container.ID)
		require.NotContains(t, string(raw), "space_id")
	})

	t.Run("LeavesTheActorOutForTheOrganizationInstance", func(t *testing.T) {
		org := newOrgInstance(t)
		pub := &syncPublisher{}
		t.Cleanup(rabbitmq.SetDefault(pub))

		s, err := ProvisionDrive(org, Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: "Launch"})
		require.NoError(t, err)

		require.NoError(t, NotifyFileCreated(org, s, nil, newFile(t, s.Rules[0].Values[0], "plan.md")))

		require.Eventually(t, func() bool { return len(pub.published()) == 1 }, 10*time.Second, 50*time.Millisecond)
		raw, err := json.Marshal(pub.published()[0].Payload)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "twakeactor")
	})

	t.Run("PublishesAFileCreatedInASubfolderOfTheDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		pub := &syncPublisher{}
		t.Cleanup(rabbitmq.SetDefault(pub))

		s, err := ProvisionDrive(org, Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: "Launch"})
		require.NoError(t, err)
		sub, err := vfs.Mkdir(org.VFS(), "/Launch/Specs", nil)
		require.NoError(t, err)

		require.NoError(t, NotifyFileCreated(org, s, alice, newFile(t, sub.ID(), "api.md")))

		require.Eventually(t, func() bool { return len(pub.published()) == 1 }, 10*time.Second, 50*time.Millisecond)
	})

	t.Run("PublishesNothingForAFileOutsideTheDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		pub := &syncPublisher{}
		t.Cleanup(rabbitmq.SetDefault(pub))

		s, err := ProvisionDrive(org, Space{ID: "space-" + utils.RandomString(8), OrganizationID: org.OrgID, Name: "Launch"})
		require.NoError(t, err)
		for _, path := range []string{"/Elsewhere", "/Launch (2)"} {
			dir, err := vfs.Mkdir(org.VFS(), path, nil)
			require.NoError(t, err)
			require.NoError(t, NotifyFileCreated(org, s, nil, newFile(t, dir.ID(), "secret.md")))
		}

		require.Never(t, func() bool { return len(pub.published()) > 0 }, time.Second, 50*time.Millisecond)
	})

	t.Run("PublishesNothingForAnotherDrive", func(t *testing.T) {
		org := newOrgInstance(t)
		pub := &syncPublisher{}
		t.Cleanup(rabbitmq.SetDefault(pub))

		dir, err := vfs.Mkdir(org.VFS(), "/Not a space", nil)
		require.NoError(t, err)
		s, err := createDrive(org, dir, "Not a space")
		require.NoError(t, err)

		require.NoError(t, NotifyFileCreated(org, s, alice, newFile(t, dir.ID(), "notes.md")))

		var notifs []couchdb.JSONDoc
		err = couchdb.GetAllDocs(org, consts.Notifications, nil, &notifs)
		if !couchdb.IsNoDatabaseError(err) {
			require.NoError(t, err)
		}
		require.Empty(t, notifs)
		require.Never(t, func() bool { return len(pub.published()) > 0 }, time.Second, 50*time.Millisecond)
	})
}
