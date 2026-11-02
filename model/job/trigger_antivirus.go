package job

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/realtime"
)

// AntivirusTrigger listens for file creation and update events and schedules antivirus scans.
type AntivirusTrigger struct {
	broker      Broker
	log         *logger.Entry
	unscheduled chan struct{}
}

// NewAntivirusTrigger creates a new antivirus trigger.
func NewAntivirusTrigger(broker Broker) *AntivirusTrigger {
	return &AntivirusTrigger{
		broker:      broker,
		log:         logger.WithNamespace("scheduler"),
		unscheduled: make(chan struct{}),
	}
}

// Schedule starts listening for file creation and update events.
func (t *AntivirusTrigger) Schedule() {
	t.log.Infof("trigger antivirus: Starting antivirus trigger, subscribing to firehose")
	sub := realtime.GetHub().SubscribeFirehose()
	defer sub.Close()
	for {
		select {
		case e := <-sub.Channel:
			if t.match(e) {
				t.log.Debugf("trigger antivirus: matched event for domain %s, doctype %s", e.Domain, e.Doc.DocType())
				t.pushJob(e)
			}
		case <-t.unscheduled:
			t.log.Infof("trigger antivirus: Stopping antivirus trigger")
			return
		}
	}
}

func (t *AntivirusTrigger) match(e *realtime.Event) bool {
	// Only match file events (both FileDoc and DirDoc have DocType == consts.Files)
	if e.Doc.DocType() != consts.Files {
		return false
	}
	// Check if it's a file (not directory)
	doc, ok := e.Doc.(*vfs.FileDoc)
	if !ok {
		return false
	}
	if doc.Type != consts.FileType {
		return false
	}
	// For file creation, always trigger scan
	if e.Verb == realtime.EventCreate {
		return true
	}
	// For file update, only trigger if content changed
	return t.hasContentChanged(e)
}

// hasContentChanged checks if the file content has changed by comparing MD5 checksums.
func (t *AntivirusTrigger) hasContentChanged(e *realtime.Event) bool {
	newDoc, ok := e.Doc.(*vfs.FileDoc)
	if !ok {
		t.log.Warnf("Event doc is not *vfs.FileDoc, actual type: %T", e.Doc)
		return false
	}
	// OldDoc should always be available for file updates via VFS.
	// If nil, it's not a standard file update - skip scan.
	if e.OldDoc == nil {
		t.log.Warnf("Update event for file %s has no OldDoc, skipping scan", newDoc.DocID)
		return false
	}
	oldDoc, ok := e.OldDoc.(*vfs.FileDoc)
	if !ok {
		t.log.Warnf("Event OldDoc is not *vfs.FileDoc, actual type: %T", e.OldDoc)
		return false
	}
	return !newDoc.SameContent(oldDoc)
}

func (t *AntivirusTrigger) pushJob(e *realtime.Event) {
	t.log.Debugf("trigger antivirus: received event for domain %s, doctype %s, verb %s", e.Domain, e.Doc.DocType(), e.Verb)

	// Get instance to check if antivirus is enabled
	inst, err := instance.Get(e.Domain)
	if err != nil {
		t.log.Errorf("trigger antivirus: could not get instance %s: %v", e.Domain, err)
		return
	}

	cfg := config.GetAntivirusConfig(inst.ContextName)
	if cfg == nil {
		t.log.Debugf("trigger antivirus: no config for context %s", inst.ContextName)
		return
	}
	if !cfg.Enabled {
		t.log.Debugf("trigger antivirus: disabled for context %s", inst.ContextName)
		return
	}

	doc, ok := e.Doc.(*vfs.FileDoc)
	if !ok {
		t.log.Debugf("trigger antivirus: event doc is not a FileDoc, type: %T", e.Doc)
		return
	}

	log := t.log.WithField("domain", e.Domain)

	t.setPendingStatus(inst, doc, log)

	// Push antivirus job
	event, err := NewEvent(e)
	if err != nil {
		log.Errorf("trigger antivirus: Could not create event for file %s: %s", doc.DocID, err.Error())
		return
	}

	msg, _ := NewMessage(map[string]string{"file_id": doc.DocID})
	req := &JobRequest{
		WorkerType: "antivirus",
		Message:    msg,
		Event:      event,
	}

	log.Infof("trigger antivirus: Pushing scan job for file %s", doc.DocID)
	if _, err := t.broker.PushJob(inst, req); err != nil {
		log.Errorf("trigger antivirus: Could not schedule job: %s", err.Error())
	}
}

func (t *AntivirusTrigger) setPendingStatus(inst *instance.Instance, doc *vfs.FileDoc, log logger.Logger) {
	fs := inst.VFS()
	file, err := fs.FileByID(doc.DocID)
	if err != nil {
		// File may have been deleted between event and now - not an error
		log.Warnf("trigger antivirus: Could not get file %s for pending status: %s", doc.DocID, err)
		return
	}

	newdoc := file.Clone().(*vfs.FileDoc)
	newdoc.AntivirusScan = &vfs.AntivirusScan{
		Status: vfs.AVStatusPending,
	}
	if err := couchdb.UpdateDoc(fs, newdoc); err != nil {
		// Conflict or other error - log and continue, job will still run
		log.Warnf("trigger antivirus: Could not set pending status for file %s: %s", doc.DocID, err)
	}
}

// Unschedule stops the trigger.
func (t *AntivirusTrigger) Unschedule() {
	close(t.unscheduled)
}
