package job

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingBroker keeps the jobs that it is given, with the values of the only
// field of their message.
type recordingBroker struct {
	Broker
	mu   sync.Mutex
	jobs []recordedJob
}

type recordedJob struct {
	worker, domain string
	values         []string
}

func (b *recordingBroker) PushJob(db prefixer.Prefixer, req *JobRequest) (*Job, error) {
	var msg map[string][]string
	if err := json.Unmarshal(req.Message, &msg); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, values := range msg {
		b.jobs = append(b.jobs, recordedJob{req.WorkerType, db.DomainName(), values})
	}
	return &Job{}, nil
}

func (b *recordingBroker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.jobs)
}

// recorded returns the values of the jobs of a worker on an instance, job by
// job.
func (b *recordingBroker) recorded(worker string, db prefixer.Prefixer) [][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var values [][]string
	for _, j := range b.jobs {
		if j.worker == worker && j.domain == db.DomainName() {
			values = append(values, j.values)
		}
	}
	return values
}

// startTrigger runs the trigger on its own subscription: the subscription is
// done before the first event of the test is published.
func startTrigger(t *testing.T, delay time.Duration) *recordingBroker {
	t.Helper()
	broker := &recordingBroker{}
	trigger := newRecentsSyncTrigger(broker, delay)
	sub := realtime.GetHub().SubscribeFirehose()
	done := make(chan struct{})
	go func() {
		defer close(done)
		trigger.run(sub)
	}()
	t.Cleanup(func() {
		trigger.Unschedule()
		<-done
		sub.Close()
	})
	return broker
}

// fileDoc is a document of io.cozy.files for the events of the tests.
type fileDoc string

func (d fileDoc) ID() string      { return string(d) }
func (d fileDoc) DocType() string { return consts.Files }

func TestRecentsSyncTrigger(t *testing.T) {
	config.UseTestFile(t)
	hub := realtime.GetHub()
	db := prefixer.NewPrefixer(0, "recents-sync-a.test", "recents-sync-a")
	other := prefixer.NewPrefixer(0, "recents-sync-b.test", "recents-sync-b")

	t.Run("DeletionsOfAWindowGiveOneJob", func(t *testing.T) {
		delay := 300 * time.Millisecond
		broker := startTrigger(t, delay)
		// The deletions go on for longer than the window, and the first job does
		// not wait for them to stop.
		sent := 0
		for stop := time.Now().Add(5 * delay); time.Now().Before(stop) && broker.count() == 0; sent++ {
			hub.Publish(db, realtime.EventDelete, fileDoc(fmt.Sprintf("item-%03d", sent)), nil)
			time.Sleep(10 * time.Millisecond)
		}
		require.Equal(t, 1, broker.count())
		first := broker.recorded(RecentsSyncWorker, db)[0]
		require.Greater(t, len(first), 2)

		// What comes after the job is in another window, for one more job.
		hub.Publish(db, realtime.EventDelete, fileDoc("later"), nil)
		require.Eventually(t, func() bool {
			return broker.count() == 2
		}, 10*time.Second, 10*time.Millisecond)
		jobs := broker.recorded(RecentsSyncWorker, db)
		assert.Equal(t, first, jobs[0])
		assert.Equal(t, sent+1, len(jobs[0])+len(jobs[1]))
		assert.Contains(t, jobs[1], "later")
	})

	t.Run("ItemsAreGroupedByInstanceInBoundedJobs", func(t *testing.T) {
		broker := startTrigger(t, 500*time.Millisecond)
		ids := make([]string, recentsSyncBatchSize+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("item-%03d", i)
		}
		for _, id := range slices.Backward(ids) {
			hub.Publish(db, realtime.EventDelete, fileDoc(id), nil)
			hub.Publish(db, realtime.EventDelete, fileDoc(id), nil)
		}
		hub.Publish(other, realtime.EventDelete, fileDoc("c"), nil)
		require.Eventually(t, func() bool {
			return broker.count() == 3
		}, 10*time.Second, 10*time.Millisecond)

		jobs := broker.recorded(RecentsSyncWorker, db)
		require.Len(t, jobs, 2)
		assert.Len(t, jobs[0], recentsSyncBatchSize)
		assert.Equal(t, ids, slices.Concat(jobs...))
		assert.Equal(t, [][]string{{"c"}}, broker.recorded(RecentsSyncWorker, other))
	})

	t.Run("OnlyDeletionsOfFiles", func(t *testing.T) {
		broker := startTrigger(t, 50*time.Millisecond)
		hub.Publish(db, realtime.EventCreate, fileDoc("created"), nil)
		hub.Publish(db, realtime.EventUpdate, fileDoc("updated"), nil)
		hub.Publish(db, realtime.EventNotify, fileDoc("notified"), nil)
		hub.Publish(db, realtime.EventDelete, &Job{JobID: "not-a-file"}, nil)
		time.Sleep(300 * time.Millisecond)
		assert.Zero(t, broker.count())
	})
}
