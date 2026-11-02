package job

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/require"
)

func TestPublicLinkEventTrigger(t *testing.T) {
	config.UseTestFile(t)
	db := prefixer.NewPrefixer(0, "trigger-test", "trigger-test")
	for _, args := range []string{
		consts.PublicLinkEvents,
		consts.PublicLinkEvents + ":CREATED",
		"io.cozy.foos:CREATED " + consts.PublicLinkEvents + ":UPDATED",
	} {
		t.Run(args, func(t *testing.T) {
			_, err := NewTrigger(db, TriggerInfos{Type: "@event", Arguments: args}, nil)
			require.Error(t, err)
		})
	}
	_, err := NewTrigger(db, TriggerInfos{Type: "@event", Arguments: "io.cozy.foos:CREATED"}, nil)
	require.NoError(t, err)

	// Reconstruct a trigger saved before this doctype was protected.
	trigger, err := NewEventTrigger(&TriggerInfos{
		Type: "@event", Arguments: consts.PublicLinkEvents + " io.cozy.foos", Domain: db.DomainName(), Prefix: db.DBPrefix(),
	})
	require.NoError(t, err)
	jobs := trigger.Schedule()
	defer trigger.Unschedule()
	control := &couchdb.JSONDoc{Type: "io.cozy.foos", M: map[string]interface{}{"_id": "control"}}
	private := &couchdb.JSONDoc{Type: consts.PublicLinkEvents, M: map[string]interface{}{"_id": "private", "ip": "192.0.2.1"}}

	// Wait for the scheduler subscription using a real control event.
	require.Eventually(t, func() bool {
		realtime.GetHub().Publish(db, realtime.EventCreate, control, nil)
		select {
		case req := <-jobs:
			var event struct {
				Doc map[string]interface{} `json:"doc"`
			}
			require.NoError(t, req.Event.Unmarshal(&event))
			require.Equal(t, "control", event.Doc["_id"])
			return true
		case <-time.After(10 * time.Millisecond):
			return false
		}
	}, time.Second, 10*time.Millisecond)

	realtime.GetHub().Publish(db, realtime.EventCreate, private, nil)
	select {
	case <-jobs:
		t.Fatal("private public-link event was delivered to a persisted trigger")
	case <-time.After(200 * time.Millisecond):
	}
}
