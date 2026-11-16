package job

import (
	"context"
	"encoding/json"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// ExpiredLinksTrigger reconciles all instances, even inactive ones, on startup
// and every five minutes. Unlike @at this catches arbitrarily long downtime.
type ExpiredLinksTrigger struct {
	broker Broker
	cancel context.CancelFunc
}

func NewExpiredLinksTrigger(b Broker) *ExpiredLinksTrigger {
	return &ExpiredLinksTrigger{broker: b}
}

func (t *ExpiredLinksTrigger) Schedule(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		if err := instance.ForeachInstances(func(inst *instance.Instance) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// One broken instance must not starve the following instances.
			_ = QueueExpiredLinkReports(ctx, t.broker, inst, time.Now())
			return nil
		}); err != nil && ctx.Err() == nil {
			joblog.Warn("Could not enumerate instances for link reports")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// QueueExpiredLinkReports paginates all permissions rather than limiting the
// first page of expired links (which would starve newer expirations).
func QueueExpiredLinkReports(ctx context.Context, b Broker, inst *instance.Instance, now time.Time) error {
	return couchdb.ForeachDocs(inst, consts.Permissions, func(_ string, raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p permission.Permission
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		at, ok := p.ActivityReportExpiry()
		if !ok || at.After(now) {
			return nil
		}
		var state publiclink.Report
		err := couchdb.GetDoc(inst, consts.PublicLinkReports, publiclink.ReportID(p.ID(), at), &state)
		if err == nil && (state.Terminal() || state.NextAttempt.After(now)) {
			return nil
		}
		if err != nil && !couchdb.IsNotFoundError(err) && !couchdb.IsNoDatabaseError(err) {
			return err
		}
		msg, _ := json.Marshal(publiclink.ReportMessage{LinkID: p.ID(), Expiry: at})
		_, err = b.PushJob(inst, &JobRequest{WorkerType: "expired-link-report", Message: msg})
		return err
	})
}
