package job

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	multierror "github.com/hashicorp/go-multierror"
)

type (
	// memQueue is a queue in-memory implementation of the Queue interface.
	memQueue struct {
		MaxCapacity int
		Jobs        chan *Job
		closed      chan struct{}

		list *list.List
		run  bool
		jmu  sync.RWMutex
	}

	// memBroker is an in-memory broker implementation of the Broker interface.
	memBroker struct {
		queues       map[string]*memQueue
		workers      []*Worker
		workersTypes []string
		running      uint32
	}
)

// newMemQueue creates and a new in-memory queue.
func newMemQueue(workerType string) *memQueue {
	return &memQueue{
		list:   list.New(),
		Jobs:   make(chan *Job),
		closed: make(chan struct{}),
	}
}

func (q *memQueue) enqueue(ctx context.Context, job *Job) error {
	q.jmu.Lock()
	defer q.jmu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	q.list.PushBack(job.Clone())
	if !q.run {
		q.run = true
		go q.send()
	}
	return nil
}

func (q *memQueue) send() {
	for {
		q.jmu.Lock()
		e := q.list.Front()
		if e == nil || !q.run {
			q.run = false
			q.jmu.Unlock()
			return
		}
		q.list.Remove(e)
		q.jmu.Unlock()
		select {
		case <-q.closed:
			return
		case q.Jobs <- e.Value.(*Job):
		}
	}
}

func (q *memQueue) close() {
	q.jmu.Lock()
	defer q.jmu.Unlock()
	if !q.run {
		return
	}
	q.run = false
	go func() { q.closed <- struct{}{} }()
}

// Len returns the length of the queue
func (q *memQueue) Len() int {
	q.jmu.RLock()
	defer q.jmu.RUnlock()
	return q.list.Len()
}

// NewMemBroker creates a new in-memory broker system.
//
// The in-memory implementation of the job system has the specifity that
// workers are actually launched by the broker at its creation.
func NewMemBroker() Broker {
	return &memBroker{
		queues: make(map[string]*memQueue),
	}
}

func (b *memBroker) StartWorkers(ws WorkersList) error {
	if !atomic.CompareAndSwapUint32(&b.running, 0, 1) {
		return ErrClosed
	}

	for _, conf := range ws {
		b.workersTypes = append(b.workersTypes, conf.WorkerType)
		if conf.Concurrency <= 0 {
			continue
		}
		q := newMemQueue(conf.WorkerType)
		w := NewWorker(conf)
		b.queues[conf.WorkerType] = q
		b.workers = append(b.workers, w)
		if err := w.Start(q.Jobs); err != nil {
			return err
		}
	}

	if len(b.workers) > 0 {
		joblog.Infof("Started in-memory broker for %d workers type", len(b.workers))
	}

	// XXX for retro-compat
	if slots := config.GetConfig().Jobs.NbWorkers; len(b.workers) > 0 && slots > 0 {
		joblog.Warnf("Limiting the number of total concurrent workers to %d", slots)
		joblog.Warnf("Please update your configuration file to avoid a hard limit")
		setNbSlots(slots)
	}

	return nil
}

func (b *memBroker) ShutdownWorkers(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&b.running, 1, 0) {
		return ErrClosed
	}
	if len(b.workers) == 0 {
		return nil
	}

	fmt.Print("  shutting down in-memory broker...")

	for _, q := range b.queues {
		q.close()
	}

	errs := make(chan error)
	for _, w := range b.workers {
		go func(w *Worker) { errs <- w.Shutdown(ctx) }(w)
	}
	var errm error
	for i := 0; i < len(b.workers); i++ {
		if err := <-errs; err != nil {
			errm = multierror.Append(errm, err)
		}
	}

	if errm != nil {
		fmt.Println("failed:", errm)
	} else {
		fmt.Println("ok.")
	}
	return errm
}

// PushJob will produce a new Job with the given options and enqueue the job in
// the proper queue.
func (b *memBroker) PushJob(db prefixer.Prefixer, req *JobRequest) (*Job, error) {
	return b.pushJob(context.Background(), db, req)
}

func (b *memBroker) pushJob(ctx context.Context, db prefixer.Prefixer, req *JobRequest) (*Job, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if atomic.LoadUint32(&b.running) == 0 {
		return nil, ErrClosed
	}

	workerType := req.WorkerType
	var worker *Worker
	for _, w := range b.workers {
		if w.Type == workerType {
			worker = w
			break
		}
	}
	if worker == nil && workerType != "client" {
		return nil, ErrUnknownWorker
	}

	// Check for limits
	ct, err := GetCounterTypeFromWorkerType(req.WorkerType)
	if err == nil {
		err := config.GetRateLimiter().CheckRateLimit(db, ct)
		if errors.Is(err, limits.ErrRateLimitReached) {
			joblog.WithFields(logger.Fields{
				"worker_type": req.WorkerType,
				"instance":    db.DomainName(),
			}).Warn(err.Error())
			return nil, err
		}
		if limits.IsLimitReachedOrExceeded(err) {
			return nil, err
		}
	}

	job := NewJob(db, req)
	if worker != nil && worker.Conf.BeforeHook != nil {
		ok, err := worker.Conf.BeforeHook(job)
		if err != nil {
			return nil, err
		}
		if !ok {
			return job, nil
		}
	}

	if err := job.create(ctx); err != nil {
		return nil, err
	}

	// For client jobs, we don't need to enqueue the job.
	if workerType == "client" {
		return job, nil
	}

	q := b.queues[workerType]
	if err := q.enqueue(ctx, job); err != nil {
		return nil, err
	}
	return job, nil
}

// WorkerQueueLen returns the size of the number of elements in queue of the
// specified worker type.
func (b *memBroker) WorkerQueueLen(workerType string) (int, error) {
	q, ok := b.queues[workerType]
	if !ok {
		return 0, ErrUnknownWorker
	}
	return q.Len(), nil
}

func (b *memBroker) WorkerIsReserved(workerType string) (bool, error) {
	for _, w := range b.workers {
		if w.Type == workerType {
			return w.Conf.Reserved, nil
		}
	}
	return false, ErrUnknownWorker
}

func (b *memBroker) WorkersTypes() []string {
	return b.workersTypes
}

var _ Broker = &memBroker{}
