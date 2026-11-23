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
	var lastWindow time.Time
	var backoff reportScanBackoff
	for ctx.Err() == nil {
		now := time.Now()
		start, end := publiclink.ReportWindow(now)
		if !start.Equal(lastWindow) {
			lastWindow, backoff = start, 0
		}
		wake := start
		if !now.Before(start) {
			window, cancel := context.WithDeadline(ctx, end)
			done, err := scanExpiredLinks(window, b, time.Now)
			cancel()
			if err != nil && ctx.Err() == nil && time.Now().Before(end) {
				// Scan errors carry only a safe diagnostic stage in Error().
				joblog.Warnf("Could not scan expired link reports: %s", err)
			}
			if done {
				wake, _ = publiclink.ReportWindow(end)
			} else {
				wake = time.Now().Add(backoff.nextDelay(err != nil))
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

type reportScanBackoff time.Duration

func (b *reportScanBackoff) nextDelay(failed bool) time.Duration {
	const fastRetry = 5 * time.Minute
	if !failed {
		// A healthy broker with a backlog should still be checked promptly.
		return fastRetry
	}
	*b = reportScanBackoff(min(max(2*time.Duration(*b), fastRetry), time.Hour))
	return time.Duration(*b)
}

type reportScanError struct {
	stage string
	cause error
}

func (e reportScanError) Error() string { return e.stage }
func (e reportScanError) Unwrap() error { return e.cause }

// scanExpiredLinks reports whether no further scan is needed in this window.
func scanExpiredLinks(ctx context.Context, b reportBroker, now func() time.Time) (bool, error) {
	cutoff := now()
	start, end := publiclink.ReportWindow(cutoff)
	if cutoff.Before(start) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	locks := lock.Getter(localReportScanLock)
	depthOf := b.WorkerQueueLen
	rb, _ := b.(*redisBroker)
	if rb != nil {
		locks = rb.reportScanLock
		depthOf = func(workerType string) (int, error) { return rb.queueLen(ctx, workerType, true) }
	}
	mu := locks.LongOperation(prefixer.GlobalPrefixer, "expired-link-report-scan")
	if err := mu.Lock(); err != nil {
		return false, reportScanError{"acquire scan lease", err}
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var marker string
	if rb != nil {
		marker = redisPrefix + publiclink.ReportWorker + "/scanned/" + start.Format(time.DateOnly)
		seen, err := rb.reportScanDone(ctx, marker)
		if err != nil && ctx.Err() == nil {
			joblog.Warn("Could not read expired link report scan marker")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if seen {
			return true, nil
		}
	}
	// A wave is admitted only when the previous one has left both queues.
	// Jobs already picked up by workers do not count toward queue depth.
	depth, err := depthOf(publiclink.ReportWorker)
	if err != nil {
		return false, reportScanError{"read report queue depth", err}
	}
	if depth > 0 {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	var instanceErr error
	resolveFlag := feature.NewFreshFlagResolver(publiclink.FlagEnabled)
	err = instance.ForeachInstances(func(inst *instance.Instance) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		enabled, err := resolveFlag(inst)
		if err != nil {
			err = reportScanError{"resolve instance report flag", err}
		} else if enabled {
			err = queueExpiredLinks(ctx, b, inst, cutoff)
		}
		if err != nil && instanceErr == nil {
			// Keep scanning healthy instances, but don't memoize a partial scan.
			instanceErr = err
		}
		return ctx.Err()
	})
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err != nil {
		return false, reportScanError{"read instance list from CouchDB", err}
	}
	if instanceErr != nil {
		return false, instanceErr
	}
	finished := now()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if rb != nil && !finished.Before(start) && finished.Before(end) {
		// Traversal has completed; losing the marker may cost a scan, not delivery.
		if err := rb.client.Set(ctx, marker, "1", end.Sub(finished)).Err(); err != nil && ctx.Err() == nil {
			joblog.Warn("Could not write expired link report scan marker")
		}
	}
	return true, nil
}

func (b *redisBroker) reportScanDone(ctx context.Context, key string) (bool, error) {
	client, err := b.primaryForKey(ctx, key)
	if err != nil {
		return false, err
	}
	n, err := client.Exists(ctx, key).Result()
	return n > 0 && err == nil, err
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
			return reportScanError{"read report state from CouchDB", err}
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
				return reportScanError{"encode report job", err}
			}
			if _, err := b.pushJob(ctx, inst, &JobRequest{WorkerType: publiclink.ReportWorker, Message: message}); err != nil {
				return reportScanError{"enqueue report job", err}
			}
		}
		batch = batch[:0]
		return nil
	}
	visited := false
	err := couchdb.ForeachDocs(inst, consts.Permissions, func(_ string, raw json.RawMessage) error {
		visited = true
		if err := ctx.Err(); err != nil {
			return err
		}
		var p permission.Permission
		if err := json.Unmarshal(raw, &p); err != nil {
			// A malformed legacy permission cannot authorize a report, but
			// must not hide the other links in this instance.
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
	if !visited && couchdb.IsNoDatabaseError(err) {
		return nil
	}
	if err != nil {
		if _, ok := err.(reportScanError); ok {
			return err
		}
		return reportScanError{"read permissions from CouchDB", err}
	}
	return flush()
}
