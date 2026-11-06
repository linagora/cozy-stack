package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// memScheduler is a centralized scheduler of many triggers. It starts all of
// them and schedules jobs accordingly.
type memScheduler struct {
	broker Broker

	ts      map[string]Trigger
	thumb   *ThumbnailTrigger
	share   *ShareGroupTrigger
	av      *AntivirusTrigger
	reports *ExpiredLinksTrigger
	mu      sync.RWMutex
	log     *logger.Entry
}

// NewMemScheduler creates a new in-memory scheduler that will load all
// registered triggers and schedule their work.
func NewMemScheduler() Scheduler {
	return newMemScheduler()
}

func newMemScheduler() *memScheduler {
	return &memScheduler{
		ts:  make(map[string]Trigger),
		log: logger.WithNamespace("mem-scheduler"),
	}
}

// StartScheduler will start the scheduler by actually loading all triggers
// from the scheduler's storage and associate for each of them a go routine in
// which they wait for the trigger send job requests.
func (s *memScheduler) StartScheduler(b Broker) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broker = b

	s.thumb = NewThumbnailTrigger(s.broker)
	go s.thumb.Schedule()
	s.share = NewShareGroupTrigger(s.broker)
	go s.share.Schedule()
	s.av = NewAntivirusTrigger(s.broker)
	go s.av.Schedule()
	s.reports = NewExpiredLinksTrigger(s.broker)
	reportCtx, cancel := context.WithCancel(context.Background())
	s.reports.cancel = cancel
	if _, skip := os.LookupEnv("COZY_SKIP_LOADING_TRIGGERS"); !skip {
		go s.reports.Schedule(reportCtx)
	}

	// XXX The memory scheduler loads the triggers from CouchDB when the stack
	// is started. This can cause some stability issues when running system
	// tests in parallel. To avoid that, an env variable
	// COZY_SKIP_LOADING_TRIGGERS can be set to skip loading the triggers from
	// CouchDB. It is correct for system tests, as instances are created and
	// destroyed by the same process. But, it should not be used elsewhere.
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "COZY_SKIP_LOADING_TRIGGERS=") {
			return nil
		}
	}

	var ts []*TriggerInfos
	err := couchdb.ForeachDocs(prefixer.GlobalPrefixer, consts.Instances, func(_ string, data json.RawMessage) error {
		db := &instance.Instance{}
		if err := json.Unmarshal(data, db); err != nil {
			return err
		}
		err := couchdb.ForeachDocs(db, consts.Triggers, func(_ string, data json.RawMessage) error {
			var t *TriggerInfos
			if err := json.Unmarshal(data, &t); err != nil {
				return err
			}

			// Remove the legacy @event trigger for thumbnail, it is now hardcoded
			if t.WorkerType == "thumbnail" {
				_ = couchdb.DeleteDoc(db, t)
				return nil
			}

			ts = append(ts, t)
			return nil
		})
		if err != nil && !couchdb.IsNoDatabaseError(err) {
			return err
		}
		return nil
	})
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return err
	}

	for _, infos := range ts {
		t, err := fromTriggerInfos(infos)
		if err != nil {
			joblog.Errorf("Could not start trigger with ID %s: %s",
				infos.ID(), err.Error())
			continue
		}
		s.ts[t.DBPrefix()+"/"+infos.TID] = t
		go s.schedule(t)
	}

	return nil
}

// ShutdownScheduler shuts down the scheduling of triggers
func (s *memScheduler) ShutdownScheduler(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Print("  shutting down in-memory scheduler...")
	for _, t := range s.ts {
		t.Unschedule()
	}
	s.thumb.Unschedule()
	s.share.Unschedule()
	s.av.Unschedule()
	s.reports.cancel()
	fmt.Println("ok.")
	return nil
}

// AddTrigger will add a new trigger to the scheduler. The trigger is persisted
// in storage.
func (s *memScheduler) AddTrigger(t Trigger) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := createTrigger(t); err != nil {
		return err
	}
	s.ts[t.DBPrefix()+"/"+t.Infos().TID] = t
	go s.schedule(t)
	return nil
}

// GetTrigger returns the trigger with the specified ID.
func (s *memScheduler) GetTrigger(db prefixer.Prefixer, id string) (Trigger, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.ts[db.DBPrefix()+"/"+id]
	if !ok {
		return nil, ErrNotFoundTrigger
	}
	return t, nil
}

// UpdateMessage changes the message for the given trigger.
func (s *memScheduler) UpdateMessage(db prefixer.Prefixer, trigger Trigger, message json.RawMessage) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	infos := trigger.Infos()
	infos.Message = Message(message)
	return couchdb.UpdateDoc(db, infos)
}

// UpdateCron will change the frequency of execution for the given trigger.
func (s *memScheduler) UpdateCron(db prefixer.Prefixer, trigger Trigger, arguments string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if trigger.Type() != "@cron" {
		return ErrNotCronTrigger
	}
	infos := trigger.Infos()
	infos.Arguments = arguments
	updated, err := NewCronTrigger(infos)
	if err != nil {
		return err
	}
	trigger.Unschedule()
	s.ts[updated.DBPrefix()+"/"+infos.TID] = updated
	go s.schedule(updated)
	return couchdb.UpdateDoc(db, infos)
}

// DeleteTrigger removes the trigger with the specified ID. The trigger is unscheduled
// and remove from the storage.
func (s *memScheduler) DeleteTrigger(db prefixer.Prefixer, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.ts[db.DBPrefix()+"/"+id]
	if !ok {
		return ErrNotFoundTrigger
	}
	delete(s.ts, db.DBPrefix()+"/"+id)
	t.Unschedule()
	return couchdb.DeleteDoc(db, t.Infos())
}

// GetAllTriggers returns all the running in-memory triggers.
func (s *memScheduler) GetAllTriggers(db prefixer.Prefixer) ([]Trigger, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := db.DBPrefix() + "/"
	v := make([]Trigger, 0)
	for n, t := range s.ts {
		if strings.HasPrefix(n, prefix) {
			v = append(v, t)
		}
	}
	return v, nil
}

// HasEventTrigger returns true if the given trigger already exists. Only the
// type (@event, @cron...), worker, and arguments (if not empty) are looked at.
func (s *memScheduler) HasTrigger(db prefixer.Prefixer, infos TriggerInfos) bool {
	prefix := db.DBPrefix() + "/"
	for n, t := range s.ts {
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		i := t.Infos()
		if infos.Type == i.Type && infos.WorkerType == i.WorkerType {
			if infos.Arguments == "" || infos.Arguments == i.Arguments {
				return true
			}
		}
	}
	return false
}

func (s *memScheduler) schedule(t Trigger) {
	s.log.Debugf("trigger %s(%s): Starting trigger",
		t.Type(), t.Infos().TID)
	ch := t.Schedule()
	if ch == nil {
		return
	}
	var debounced <-chan time.Time
	var combinedReq *JobRequest
	var d time.Duration
	infos := t.Infos()
	if infos.Debounce != "" {
		var err error
		if d, err = time.ParseDuration(infos.Debounce); err != nil {
			s.log.Errorf("trigger %s has an invalid debounce: %s",
				infos.TID, infos.Debounce)
		}
	}
	for {
		select {
		case req, ok := <-ch:
			if !ok {
				return
			}
			if d == 0 {
				s.pushJob(t, req)
			} else if debounced == nil {
				debounced = time.After(d)
				combinedReq = combineRequests(t, req, nil)
			} else {
				combinedReq = combineRequests(t, combinedReq, req)
			}
		case <-debounced:
			s.pushJob(t, combinedReq)
			debounced = nil
			combinedReq = nil
		}
	}
}

func combineRequests(t Trigger, req1, req2 *JobRequest) *JobRequest {
	switch t.CombineRequest() {
	case appendPayload:
		if req2 == nil {
			req1.Payload = Payload(`{"payloads":[` + string(req1.Payload) + "]}")
		} else {
			was := string(req1.Payload)
			cut := was[:len(was)-2] // Remove the final ]}
			req1.Payload = Payload(cut + "," + string(req2.Payload) + "]}")
		}
		return req1
	case suppressPayload:
		return t.Infos().JobRequest()
	default: // keepOriginalRequest
		return req1
	}
}

func (s *memScheduler) pushJob(t Trigger, req *JobRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	log := s.log.WithField("domain", t.DomainName())
	log.Infof("trigger %s(%s): Pushing new job %s",
		t.Type(), t.Infos().TID, req.WorkerType)
	if _, err := s.broker.PushJob(t, req); err != nil {
		log.Errorf("trigger %s(%s): Could not schedule a new job: %s",
			t.Type(), t.Infos().TID, err.Error())
	}
}

func (s *memScheduler) PollScheduler(now int64) error {
	return errors.New("memScheduler cannot be polled")
}

// CleanRedis does nothing for the in memory scheduler. It's just
// here to implement the Scheduler interface.
func (s *memScheduler) CleanRedis() error {
	s.log.Warnf("CleanRedis called on memScheduler, which does not use Redis")
	return nil
}

// RebuildRedis does nothing for the in memory scheduler. It's just
// here to implement the Scheduler interface.
func (s *memScheduler) RebuildRedis(db prefixer.Prefixer) error {
	s.log.Warnf("RebuildRedis called on memScheduler for %s, which does not use Redis", db.DomainName())
	return nil
}

var _ Scheduler = &memScheduler{}
