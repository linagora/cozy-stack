package driveevent

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ownerUUID = "6208ad54-5683-1041-8c8f-c9e6dccfd4ac"

type fakePublisher struct {
	requests []rabbitmq.PublishRequest
	err      error
}

func (p *fakePublisher) Publish(_ context.Context, req rabbitmq.PublishRequest) error {
	p.requests = append(p.requests, req)
	return p.err
}

// publishTo makes the events go to a new fake publisher for the test.
func publishTo(t *testing.T, err error) *fakePublisher {
	t.Helper()
	pub := &fakePublisher{err: err}
	SetPublisher(pub)
	t.Cleanup(func() { SetPublisher(nil) })
	return pub
}

func fileEventsContext(t *testing.T, on bool) {
	t.Helper()
	contexts := config.GetConfig().Contexts
	t.Cleanup(func() { config.GetConfig().Contexts = contexts })
	config.GetConfig().Contexts = map[string]interface{}{
		config.DefaultInstanceContext: map[string]interface{}{
			"drive_activity": map[string]interface{}{"file_events": on},
		},
	}
}

func owner() *instance.Instance {
	return &instance.Instance{
		Domain:      "alice.twake.local",
		OrgID:       "acme",
		ContextName: config.DefaultInstanceContext,
		OIDCUUID:    ownerUUID,
	}
}

func onlyEvent(t *testing.T, pub *fakePublisher, routingKey string) rabbitmq.CloudEvent {
	t.Helper()
	require.Len(t, pub.requests, 1)
	req := pub.requests[0]
	assert.Equal(t, rabbitmq.ExchangeActivity, req.Exchange)
	assert.Equal(t, routingKey, req.RoutingKey)
	event := req.Payload.(rabbitmq.CloudEvent)
	assert.Equal(t, event.ID, req.MessageID)
	assert.Equal(t, "twake://drive", event.Source)
	assert.Equal(t, "acme", event.TwakeOrg)
	return event
}

var invoice = &vfs.FileDoc{
	DocID:     "file-1",
	DocRev:    "2-abc",
	DirID:     "dir-1",
	DocName:   "invoice.pdf",
	CreatedAt: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC),
	UpdatedAt: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC),
}

var invoiceObject = Object{
	Type:      "file",
	ID:        "file-1",
	Title:     "invoice.pdf",
	Folder:    &Reference{ID: "dir-1"},
	Container: Container{Kind: "drive", ID: "alice.twake.local"},
}

func TestFileCreated(t *testing.T) {
	config.UseTestFile(t)
	ctx := context.Background()

	t.Run("IsOffByDefault", func(t *testing.T) {
		pub := publishTo(t, nil)
		FileCreated(ctx, owner(), invoice, true)
		assert.Empty(t, pub.requests)
	})

	t.Run("PublishesTheFileForItsOwner", func(t *testing.T) {
		fileEventsContext(t, true)
		pub := publishTo(t, nil)
		FileCreated(ctx, owner(), invoice, true)
		event := onlyEvent(t, pub, rabbitmq.RoutingKeyDriveFileCreated)
		assert.Equal(t, "file-1", event.ID)
		assert.Equal(t, ownerUUID, event.TwakeActorID)
		assert.WithinDuration(t, time.Now(), event.Time, time.Minute)
		assert.Equal(t, Data{
			Object:     invoiceObject,
			Recipients: []Recipient{{UUID: ownerUUID, Reason: "owner"}},
		}, event.Data)
	})

	t.Run("NamesNoActorForAFileFromASharingLink", func(t *testing.T) {
		fileEventsContext(t, true)
		pub := publishTo(t, nil)
		FileCreated(ctx, owner(), invoice, false)
		event := onlyEvent(t, pub, rabbitmq.RoutingKeyDriveFileCreated)
		assert.Empty(t, event.TwakeActorID)
		assert.Equal(t, []Recipient{{UUID: ownerUUID, Reason: "owner"}}, event.Data.(Data).Recipients)
	})

	t.Run("PublishesNothingUntilTheUUIDOfTheOwnerIsKnown", func(t *testing.T) {
		fileEventsContext(t, true)
		pub := publishTo(t, nil)
		inst := owner()
		inst.OIDCUUID = ""
		FileCreated(ctx, inst, invoice, true)
		assert.Empty(t, pub.requests)
	})

	t.Run("OnlyLogsAFailedPublish", func(t *testing.T) {
		fileEventsContext(t, true)
		publishTo(t, assert.AnError)
		FileCreated(ctx, owner(), invoice, true)
		SetPublisher(nil)
		FileCreated(ctx, owner(), invoice, true)
	})
}

func TestFileUpdated(t *testing.T) {
	config.UseTestFile(t)
	fileEventsContext(t, true)
	pub := publishTo(t, nil)
	FileUpdated(context.Background(), owner(), invoice, false)
	event := onlyEvent(t, pub, rabbitmq.RoutingKeyDriveFileUpdated)
	assert.Equal(t, "file-1/updated/2-abc", event.ID)
	assert.Empty(t, event.TwakeActorID)
	assert.WithinDuration(t, time.Now(), event.Time, time.Minute)
	assert.Equal(t, invoiceObject, event.Data.(Data).Object)
}

func TestFileDeleted(t *testing.T) {
	config.UseTestFile(t)
	fileEventsContext(t, true)
	pub := publishTo(t, nil)
	FileDeleted(context.Background(), owner(), invoice, true)
	event := onlyEvent(t, pub, rabbitmq.RoutingKeyDriveFileDeleted)
	assert.Equal(t, "file-1/deleted/2-abc", event.ID)
	assert.Equal(t, ownerUUID, event.TwakeActorID)
	assert.WithinDuration(t, time.Now(), event.Time, time.Minute)
	// The folder the file was in, not the trash.
	assert.Equal(t, invoiceObject, event.Data.(Data).Object)
}

func TestShareReceived(t *testing.T) {
	config.UseTestFile(t)
	// Even with the file events off.
	pub := publishTo(t, nil)
	ShareReceived(context.Background(), owner(), "sharing-1", "Invoices", "shortcut-1")
	event := onlyEvent(t, pub, rabbitmq.RoutingKeyDriveShareReceived)
	assert.Equal(t, "sharing-1/received", event.ID)
	assert.Empty(t, event.TwakeActorID)
	assert.Equal(t, Data{
		Object: Object{
			Type:      "sharing",
			ID:        "sharing-1",
			Title:     "Invoices",
			Shortcut:  &Reference{ID: "shortcut-1"},
			Container: Container{Kind: "drive", ID: "alice.twake.local"},
		},
		Recipients: []Recipient{{UUID: ownerUUID, Reason: "owner"}},
	}, event.Data)
}
