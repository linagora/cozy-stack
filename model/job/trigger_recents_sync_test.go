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

// startTrigger runs a trigger on its own subscription: the subscription is done
// before the first event of the test is published.
func startTrigger(t *testing.T, newTrigger func(Broker, time.Duration) *FirehoseTrigger, delay time.Duration) *recordingBroker {
	t.Helper()
	broker := &recordingBroker{}
	trigger := newTrigger(broker, delay)
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

	t.Run("SavesOfAnItemGiveOneJobByWindow", func(t *testing.T) {
		delay := 300 * time.Millisecond
		broker := startTrigger(t, newRecentsSyncTrigger, delay)
		// The saves go on for longer than the window, and the first job does not
		// wait for them to stop.
		saves := 0
		for stop := time.Now().Add(5 * delay); time.Now().Before(stop) && broker.count() == 0; saves++ {
			hub.Publish(db, realtime.EventUpdate, fileDoc("hot"), nil)
			time.Sleep(10 * time.Millisecond)
		}
		require.Greater(t, saves, 2)
		time.Sleep(2 * delay)
		// The last save may have come after the job, in a window of its own.
		pushed := broker.count()
		require.Contains(t, []int{1, 2}, pushed)

		// A later save opens another window, for one more job.
		hub.Publish(db, realtime.EventUpdate, fileDoc("hot"), nil)
		require.Eventually(t, func() bool {
			return broker.count() == pushed+1
		}, 10*time.Second, 10*time.Millisecond)
		assert.Equal(t, slices.Repeat([][]string{{"hot"}}, pushed+1), broker.recorded(RecentsSyncWorker, db))
	})

	t.Run("ItemsAreGroupedByInstanceInBoundedJobs", func(t *testing.T) {
		broker := startTrigger(t, newRecentsSyncTrigger, 500*time.Millisecond)
		ids := make([]string, recentsSyncBatchSize+1)
		for i := range ids {
			ids[i] = fmt.Sprintf("item-%03d", i)
		}
		verbs := []string{realtime.EventUpdate, realtime.EventDelete}
		for i, id := range slices.Backward(ids) {
			hub.Publish(db, verbs[i%2], fileDoc(id), nil)
		}
		hub.Publish(other, realtime.EventUpdate, fileDoc("c"), nil)
		require.Eventually(t, func() bool {
			return broker.count() == 3
		}, 10*time.Second, 10*time.Millisecond)

		jobs := broker.recorded(RecentsSyncWorker, db)
		require.Len(t, jobs, 2)
		assert.Len(t, jobs[0], recentsSyncBatchSize)
		assert.Equal(t, ids, slices.Concat(jobs...))
		assert.Equal(t, [][]string{{"c"}}, broker.recorded(RecentsSyncWorker, other))
	})

	t.Run("OnlyUpdatesAndDeletionsOfFiles", func(t *testing.T) {
		broker := startTrigger(t, newRecentsSyncTrigger, 50*time.Millisecond)
		hub.Publish(db, realtime.EventCreate, fileDoc("created"), nil)
		hub.Publish(db, realtime.EventNotify, fileDoc("notified"), nil)
		hub.Publish(db, realtime.EventUpdate, &Job{JobID: "not-a-file"}, nil)
		time.Sleep(300 * time.Millisecond)
		assert.Zero(t, broker.count())
	})
}
