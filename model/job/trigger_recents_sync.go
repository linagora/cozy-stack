package job

import (
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// recentsSyncDelay is how long the trigger gathers the changes of an instance
// before pushing a job.
const recentsSyncDelay = 10 * time.Second

// recentsSyncBatchSize is the maximum number of items of a job.
const recentsSyncBatchSize = 200

// NewRecentsSyncTrigger creates the trigger that pushes the recents-sync jobs
// for the changes of the files and folders of all the instances.
//
// When the process stops, the changes that it has not pushed are lost: a row
// then keeps its metadata until the next change of the item, or stays after the
// item is destroyed.
func NewRecentsSyncTrigger(broker Broker) *FirehoseTrigger {
	return newRecentsSyncTrigger(broker, recentsSyncDelay)
}

func newRecentsSyncTrigger(broker Broker, delay time.Duration) *FirehoseTrigger {
	t := &FirehoseTrigger{
		worker:    RecentsSyncWorker,
		delay:     delay,
		batchSize: recentsSyncBatchSize,
		collect:   collectChangedFile,
		message:   func(ids []string) any { return &RecentsSyncMessage{IDs: ids} },
	}
	return t.init(broker)
}

// collectChangedFile keeps what can change an existing row. A row is written
// after the item is created, with the metadata of the item.
func collectChangedFile(e *realtime.Event, add func(key, value string)) {
	if e.Doc.DocType() != consts.Files {
		return
	}
	if e.Verb == realtime.EventUpdate || e.Verb == realtime.EventDelete {
		add(e.Doc.ID(), e.Doc.ID())
	}
}
