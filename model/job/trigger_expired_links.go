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
	paused, err := inst.ReportsPaused()
	if err != nil || paused {
		return err
	}
	admission := inst.ReportAdmissionLock()
	if err := admission.Lock(); err != nil {
		return err
	}
	defer admission.Unlock()
	paused, err = inst.ReportsPaused()
	if err != nil || paused {
		return err
	}
	const batchSize = 100
	batch := make([]publiclink.ReportMessage, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		keys := make([]string, len(batch))
		for i, msg := range batch {
			keys[i] = publiclink.ReportID(msg.LinkID, msg.Expiry)
		}
		var reports []*publiclink.Report
		err := couchdb.GetAllDocs(inst, consts.PublicLinkReports, &couchdb.AllDocsRequest{Keys: keys}, &reports)
		if err != nil && !couchdb.IsNoDatabaseError(err) {
			return err
		}
		states := make(map[string]*publiclink.Report, len(reports))
		for _, report := range reports {
			if report != nil {
				states[report.DocID] = report
			}
		}
		for i, msg := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if state := states[keys[i]]; state != nil && (state.Terminal() || state.NextAttempt.After(now)) {
				continue
			}
			body, _ := json.Marshal(msg)
			if _, err := b.PushJob(inst, &JobRequest{WorkerType: "expired-link-report", Message: body}); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	err = couchdb.ForeachDocs(inst, consts.Permissions, func(_ string, raw json.RawMessage) error {
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
		batch = append(batch, publiclink.ReportMessage{LinkID: p.ID(), Expiry: at})
		if len(batch) == batchSize {
			return flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	return flush()
}
