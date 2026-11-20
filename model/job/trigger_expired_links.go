package job

import (
	"context"
	"encoding/json"
	"os"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Memory queues are local even when the stack has a shared instance lock backend.
var localReportScanLock = lock.NewInMemory()

type expiredLinkScheduler struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type reportBroker interface {
	Broker
	pushJob(context.Context, prefixer.Prefixer, *JobRequest) (*Job, error)
}

func startExpiredLinkScheduler(b Broker) *expiredLinkScheduler {
	if _, skip := os.LookupEnv("COZY_SKIP_LOADING_TRIGGERS"); skip {
		return nil
	}
	broker, ok := b.(reportBroker)
	if !ok {
		return nil
	}
	if reserved, err := b.WorkerIsReserved(publiclink.ReportWorker); err != nil || !reserved {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &expiredLinkScheduler{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.done)
		scheduleExpiredLinks(ctx, broker)
	}()
	return s
}

func (s *expiredLinkScheduler) stop(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func scheduleExpiredLinks(ctx context.Context, b reportBroker) {
	for ctx.Err() == nil {
		now := time.Now()
		start, end := publiclink.ReportWindow(now)
		wake := start
		if !now.Before(start) {
			window, cancel := context.WithDeadline(ctx, end)
			scanned, err := scanExpiredLinks(window, b, now)
			cancel()
			if err != nil && ctx.Err() == nil && time.Now().Before(end) {
				joblog.Warn("Could not scan expired link reports")
			}
			if scanned {
				wake, _ = publiclink.ReportWindow(end)
			} else {
				// A queue left from the previous wave is checked again this night.
				wake = time.Now().Add(5 * time.Minute)
				if wake.After(end) {
					wake = end
				}
			}
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func scanExpiredLinks(ctx context.Context, b reportBroker, cutoff time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	locks := lock.Getter(localReportScanLock)
	depthOf := b.WorkerQueueLen
	if rb, ok := b.(*redisBroker); ok {
		locks = rb.reportScanLock
		depthOf = func(workerType string) (int, error) { return rb.queueLen(ctx, workerType, true) }
	}
	mu := locks.LongOperation(prefixer.GlobalPrefixer, "expired-link-report-scan")
	if err := mu.Lock(); err != nil {
		return false, err
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// A wave is admitted only when the previous one has left both queues.
	// Jobs already picked up by workers do not count toward queue depth.
	depth, err := depthOf(publiclink.ReportWorker)
	if err != nil || depth > 0 {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	err = instance.ForeachInstances(func(inst *instance.Instance) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if feature.Enabled(inst, publiclink.FlagEnabled) {
			if err := queueExpiredLinks(ctx, b, inst, cutoff); err != nil && ctx.Err() == nil {
				// A broken instance must not prevent looking for work on the next one.
				joblog.Warn("Could not queue expired link reports for an instance")
			}
		}
		return ctx.Err()
	})
	if couchdb.IsNoDatabaseError(err) {
		return true, nil
	}
	return err == nil, err
}

func queueExpiredLinks(ctx context.Context, b reportBroker, inst *instance.Instance, cutoff time.Time) error {
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
		for idx, msg := range batch {
			keys[idx] = publiclink.DeliveryID(publiclink.ReportID(msg.LinkID, msg.Expiry))
		}
		var states []*publiclink.Delivery
		err := couchdb.GetAllDocs(inst, consts.PublicLinkReports, &couchdb.AllDocsRequest{Keys: keys}, &states)
		if err != nil && !couchdb.IsNoDatabaseError(err) {
			return err
		}
		byID := make(map[string]*publiclink.Delivery, len(states))
		for _, state := range states {
			if state != nil {
				byID[state.ID()] = state
			}
		}
		for idx, msg := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if state := byID[keys[idx]]; state != nil && !state.Due(cutoff) {
				continue
			}
			message, err := NewMessage(msg)
			if err != nil {
				return err
			}
			if _, err := b.pushJob(ctx, inst, &JobRequest{WorkerType: publiclink.ReportWorker, Message: message}); err != nil {
				return err
			}
		}
		batch = batch[:0]
		return nil
	}
	err := couchdb.ForeachDocs(inst, consts.Permissions, func(_ string, raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var p permission.Permission
		if err := json.Unmarshal(raw, &p); err != nil {
			// A malformed legacy permission cannot authorize a report, but
			// must not hide the other links in this instance.
			joblog.Warn("Could not read permission while scanning expired link reports")
			return nil
		}
		at, ok := p.ActivityReportExpiry()
		if !ok || at.After(cutoff) {
			return nil
		}
		batch = append(batch, publiclink.ReportMessage{LinkID: p.ID(), Expiry: at})
		if len(batch) == batchSize {
			return flush()
		}
		return nil
	})
	if couchdb.IsNoDatabaseError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return flush()
}
