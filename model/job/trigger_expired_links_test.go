package job_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkScanPaginationAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	t.Setenv("COZY_SKIP_LOADING_TRIGGERS", "1")
	config.UseTestFile(t)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	at := time.Now().Add(-72 * time.Hour).UTC().Truncate(time.Second)
	enabled := true
	var pending []*permission.Permission
	for i := 0; i < 135; i++ {
		p := &permission.Permission{PID: fmt.Sprintf("report-link-%03d", i), Type: permission.TypeShareByLink,
			ActivityReportEnabled: &enabled, ExpiresAt: at.In(time.FixedZone("offset", 3600)),
			Codes:       map[string]string{"link": "secret-must-not-enter-job"},
			Permissions: permission.Set{{Type: consts.Files, Values: []string{"source"}}}}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, p))
		if i < 110 {
			state := &publiclink.Report{DocID: publiclink.ReportID(p.ID(), at), ReportMessage: publiclink.ReportMessage{LinkID: p.ID(), Expiry: at}, Status: "sent"}
			require.NoError(t, couchdb.CreateNamedDocWithDB(inst, state))
		} else {
			pending = append(pending, p)
		}
	}
	future := pending[0].Clone().(*permission.Permission)
	future.PID = "future"
	future.PRev = ""
	future.ExpiresAt = time.Now().Add(time.Hour)
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, future))
	broker := job.NewMemBroker()
	processed := make(chan publiclink.ReportMessage, 100)
	require.NoError(t, broker.StartWorkers(job.WorkersList{{
		WorkerType: "expired-link-report", Concurrency: 1, MaxExecCount: 1, Timeout: time.Minute, Reserved: true,
		// The scanner's contract is real job delivery; the mail worker's effects
		// are separately tested through VFS and an actual SMTP connection.
		WorkerFunc: func(ctx *job.TaskContext) error {
			var msg publiclink.ReportMessage
			if err := ctx.UnmarshalMessage(&msg); err != nil {
				return err
			}
			processed <- msg
			return nil
		},
	}}))
	t.Cleanup(func() { _ = broker.ShutdownWorkers(context.Background()) })
	require.NoError(t, job.QueueExpiredLinkReports(context.Background(), broker, inst, time.Now()))
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		select {
		case msg := <-processed:
			require.False(t, seen[msg.LinkID])
			seen[msg.LinkID] = true
			require.True(t, msg.Expiry.Equal(at))
		case <-time.After(10 * time.Second):
			t.Fatal("eligible links behind terminal first page were starved")
		}
	}
	for _, p := range pending {
		require.True(t, seen[p.ID()])
	}
	// Persist completion, then emulate a process restart by running a fresh scan:
	// the checkpoint, not scheduler memory, suppresses historical work.
	for _, p := range pending {
		state := &publiclink.Report{DocID: publiclink.ReportID(p.ID(), at), ReportMessage: publiclink.ReportMessage{LinkID: p.ID(), Expiry: at}, Status: "no-email"}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, state))
	}
	require.NoError(t, job.QueueExpiredLinkReports(context.Background(), broker, inst, time.Now()))
	select {
	case <-processed:
		t.Fatal("terminal report queued after restart")
	default:
	}
	// A distinct expiry is a new report even for the same link; long downtime
	// and an equivalent offset spelling are normalized by the persisted key.
	p := pending[len(pending)-1]
	changed := at.Add(time.Hour)
	p.ExpiresAt = changed
	require.NoError(t, couchdb.UpdateDoc(inst, p))
	require.NoError(t, job.QueueExpiredLinkReports(context.Background(), broker, inst, time.Now()))
	select {
	case msg := <-processed:
		require.Equal(t, p.ID(), msg.LinkID)
		require.True(t, msg.Expiry.Equal(changed))
	case <-time.After(10 * time.Second):
		t.Fatal("new expiry not queued")
	}
	require.NoError(t, couchdb.ForeachDocs(inst, consts.Jobs, func(_ string, raw json.RawMessage) error {
		require.NotContains(t, string(raw), "secret-must-not-enter-job")
		return nil
	}))
}
