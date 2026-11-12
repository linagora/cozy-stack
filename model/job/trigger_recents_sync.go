package job

import (
	"maps"
	"slices"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// recentsSyncDelay is how long the trigger gathers the deletions of an instance
// before pushing a job.
const recentsSyncDelay = 10 * time.Second

// recentsSyncBatchSize is the maximum number of items of a job.
const recentsSyncBatchSize = 200

// recentsSyncWindow holds the items of an instance deleted since the first one
// that has not been pushed yet.
type recentsSyncWindow struct {
	db  prefixer.Prefixer
	ids map[string]struct{}
}

// RecentsSyncTrigger pushes the recents-sync jobs for the files and folders
// deleted on all the instances, gathered by instance. The first deletion of an
// instance starts a delay and the next ones do not extend it, like the debounce
// of the other triggers.
//
// It listens to the firehose, which only gives the events published by this
// process: with several processes, an event is seen once by the process that
// has made the change. The @event triggers are fed by the firehose too, but
// they would need a migration to exist on every instance, and their debounce
// forgets the events, whereas the job must tell what has been deleted.
//
// The windows are in memory: when the process stops, the deletions that it has
// not pushed are lost, in the same way as the events of the firehose that have
// not been read. Their rows then stay after the item is destroyed.
type RecentsSyncTrigger struct {
	broker Broker
	log    *logger.Entry
	delay  time.Duration

	windows     map[string]*recentsSyncWindow
	expired     chan string
	unscheduled chan struct{}
}

// NewRecentsSyncTrigger creates the trigger that pushes the recents-sync jobs.
func NewRecentsSyncTrigger(broker Broker) *RecentsSyncTrigger {
	return newRecentsSyncTrigger(broker, recentsSyncDelay)
}

func newRecentsSyncTrigger(broker Broker, delay time.Duration) *RecentsSyncTrigger {
	return &RecentsSyncTrigger{
		broker:      broker,
		log:         logger.WithNamespace("scheduler"),
		delay:       delay,
		windows:     make(map[string]*recentsSyncWindow),
		expired:     make(chan string),
		unscheduled: make(chan struct{}),
	}
}

// Schedule starts listening to the events.
func (t *RecentsSyncTrigger) Schedule() {
	sub := realtime.GetHub().SubscribeFirehose()
	defer sub.Close()
	t.run(sub)
}

func (t *RecentsSyncTrigger) run(sub *realtime.Subscriber) {
	for {
		select {
		case e := <-sub.Channel:
			if e.Verb == realtime.EventDelete && e.Doc.DocType() == consts.Files {
				t.add(e)
			}
		case prefix := <-t.expired:
			t.push(prefix)
		case <-t.unscheduled:
			return
		}
	}
}

func (t *RecentsSyncTrigger) add(e *realtime.Event) {
	prefix := e.DBPrefix()
	window, ok := t.windows[prefix]
	if !ok {
		window = &recentsSyncWindow{
			db:  prefixer.NewPrefixer(e.Cluster, e.Domain, prefix),
			ids: make(map[string]struct{}),
		}
		t.windows[prefix] = window
		time.AfterFunc(t.delay, func() {
			select {
			case t.expired <- prefix:
			case <-t.unscheduled:
			}
		})
	}
	window.ids[e.Doc.ID()] = struct{}{}
}

func (t *RecentsSyncTrigger) push(prefix string) {
	window := t.windows[prefix]
	delete(t.windows, prefix)
	log := t.log.WithField("domain", window.db.DomainName())
	ids := slices.Sorted(maps.Keys(window.ids))
	for batch := range slices.Chunk(ids, recentsSyncBatchSize) {
		msg, err := NewMessage(&RecentsSyncMessage{IDs: batch})
		if err != nil {
			log.Errorf("trigger %s: cannot serialize message: %s", RecentsSyncWorker, err)
			continue
		}
		req := &JobRequest{WorkerType: RecentsSyncWorker, Message: msg}
		if _, err := t.broker.PushJob(window.db, req); err != nil {
			log.Errorf("trigger %s: Could not schedule a new job: %s", RecentsSyncWorker, err)
		}
	}
}

// Unschedule stops the trigger.
func (t *RecentsSyncTrigger) Unschedule() {
	close(t.unscheduled)
}
