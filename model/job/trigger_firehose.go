package job

import (
	"maps"
	"slices"
	"time"

	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// firehoseWindow is what the events of an instance have given since the first
// one that has not been pushed yet.
type firehoseWindow struct {
	db     prefixer.Prefixer
	values map[string]string
}

// FirehoseTrigger pushes the jobs of a worker for what the events of all the
// instances give it, gathered by instance. The first event of an instance
// starts a delay and the next ones do not extend it, like the debounce of the
// other triggers: a value that keeps coming is in at most one job by delay and
// by process of the stack.
//
// It listens to the firehose, which only gives the events published by this
// process: with several processes, an event is seen once by the process that
// has made the change. The @event triggers are fed by the firehose too, but
// they would need a migration to exist on every instance, and their debounce
// forgets the events, whereas the job must tell what has changed.
//
// The windows are in memory: when the process stops, what they hold is lost, in
// the same way as the events of the firehose that have not been read.
type FirehoseTrigger struct {
	broker Broker
	log    *logger.Entry
	worker string
	delay  time.Duration
	// batchSize is the maximum number of values of a job.
	batchSize int
	// collect gives what an event adds to the next jobs of its instance. A key
	// is in a window once, with its last value.
	collect func(e *realtime.Event, add func(key, value string))
	// message builds the message of a job from sorted values.
	message func(values []string) any

	windows     map[string]*firehoseWindow
	expired     chan string
	unscheduled chan struct{}
}

func (t *FirehoseTrigger) init(broker Broker) *FirehoseTrigger {
	t.broker = broker
	t.log = logger.WithNamespace("scheduler")
	t.windows = make(map[string]*firehoseWindow)
	t.expired = make(chan string)
	t.unscheduled = make(chan struct{})
	return t
}

// Schedule starts listening to the events.
func (t *FirehoseTrigger) Schedule() {
	sub := realtime.GetHub().SubscribeFirehose()
	defer sub.Close()
	t.run(sub)
}

func (t *FirehoseTrigger) run(sub *realtime.Subscriber) {
	for {
		select {
		case e := <-sub.Channel:
			t.collect(e, func(key, value string) { t.add(e, key, value) })
		case key := <-t.expired:
			t.push(key)
		case <-t.unscheduled:
			return
		}
	}
}

func (t *FirehoseTrigger) add(e *realtime.Event, key, value string) {
	prefix := e.DBPrefix()
	window, ok := t.windows[prefix]
	if !ok {
		window = &firehoseWindow{
			db:     prefixer.NewPrefixer(e.Cluster, e.Domain, prefix),
			values: make(map[string]string),
		}
		t.windows[prefix] = window
		time.AfterFunc(t.delay, func() {
			select {
			case t.expired <- prefix:
			case <-t.unscheduled:
			}
		})
	}
	window.values[key] = value
}

func (t *FirehoseTrigger) push(prefix string) {
	window := t.windows[prefix]
	delete(t.windows, prefix)
	log := t.log.WithField("domain", window.db.DomainName())
	values := slices.Sorted(maps.Values(window.values))
	for batch := range slices.Chunk(values, t.batchSize) {
		msg, err := NewMessage(t.message(batch))
		if err != nil {
			log.Errorf("trigger %s: cannot serialize message: %s", t.worker, err)
			continue
		}
		req := &JobRequest{WorkerType: t.worker, Message: msg}
		if _, err := t.broker.PushJob(window.db, req); err != nil {
			log.Errorf("trigger %s: Could not schedule a new job: %s", t.worker, err)
		}
	}
}

// Unschedule stops the trigger.
func (t *FirehoseTrigger) Unschedule() {
	close(t.unscheduled)
}
