package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/metrics"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	defaultConcurrency  = runtime.NumCPU()
	defaultMaxExecCount = 1
	defaultRetryDelay   = 60 * time.Millisecond
	defaultTimeout      = 10 * time.Second
)

type (
	// WorkerInitFunc is called at the start of the worker system, only once. It
	// is not called before every job process. It can be useful to initialize a
	// global variable used by the worker.
	WorkerInitFunc func() error

	// WorkerStartFunc is optionally called at the beginning of the each job
	// process and can produce a context value.
	WorkerStartFunc func(ctx *TaskContext) (*TaskContext, error)

	// WorkerFunc represent the work function that a worker should implement.
	WorkerFunc func(ctx *TaskContext) error

	// WorkerCommit is an optional method that is always called once after the
	// execution of the WorkerFunc.
	WorkerCommit func(ctx *TaskContext, errjob error) error

	// WorkerBeforeHook is an optional method that is always called before the
	// job is being pushed into the queue. It can be useful to skip the job
	// beforehand.
	WorkerBeforeHook func(job *Job) (bool, error)

	// JobErrorCheckerHook is an optional method called at the beginning of the
	// job execution to prevent a retry according to the previous error
	// (specifically useful in the retries loop)
	JobErrorCheckerHook func(err error) bool

	// WorkerConfig is the configuration parameter of a worker defined by the job
	// system. It contains parameters of the worker along with the worker main
	// function that perform the work against a job's message.
	WorkerConfig struct {
		WorkerInit   WorkerInitFunc
		WorkerStart  WorkerStartFunc
		WorkerFunc   WorkerFunc
		Publisher    messaging.Publisher
		WorkerCommit WorkerCommit
		WorkerType   string
		BeforeHook   WorkerBeforeHook
		ErrorHook    JobErrorCheckerHook
		Concurrency  int
		MaxExecCount int
		Reserved     bool // true when the clients must not push jobs for this worker
		Timeout      time.Duration
		RetryDelay   time.Duration
	}

	// Worker is a unit of work that will consume from a queue and execute the do
	// method for each jobs it pulls.
	Worker struct {
		Type    string
		Conf    *WorkerConfig
		jobs    chan *Job
		running uint32
		closing chan struct{}
		closed  chan struct{}
	}

	// TaskContext is a context.Context passed to the worker for each task
	// execution and contains specific values from the job.
	TaskContext struct {
		context.Context
		Instance  *instance.Instance
		Publisher messaging.Publisher
		job       *Job
		log       logger.Logger
		id        string
		cookie    interface{}
		noRetry   bool
		execCount int
	}
)

var slots chan struct{}

func setNbSlots(nb int) {
	slots = make(chan struct{}, nb)
	for i := 0; i < nb; i++ {
		slots <- struct{}{}
	}
}

// Clone clones the worker config
func (w *WorkerConfig) Clone() *WorkerConfig {
	cloned := *w
	return &cloned
}

// NewTaskContext returns a context.Context usable by a worker.
func NewTaskContext(workerID string, job *Job, inst *instance.Instance) (*TaskContext, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	id := fmt.Sprintf("%s/%s", workerID, job.ID())
	entry := logger.WithDomain(job.Domain).WithNamespace("jobs")

	if job.ForwardLogs {
		hook := realtime.LogHook(job, realtime.GetHub(), consts.Jobs, job.ID())
		entry.AddHook(hook)
	}

	log := entry.
		WithField("job_id", job.ID()).
		WithField("worker_id", workerID)

	return &TaskContext{
		Context:  ctx,
		Instance: inst,
		job:      job,
		log:      log,
		id:       id,
	}, cancel
}

// WithTimeout returns a clone of the context with a different deadline.
func (c *TaskContext) WithTimeout(timeout time.Duration) (*TaskContext, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(c.Context, timeout)
	newCtx := c.clone()
	newCtx.Context = ctx
	return newCtx, cancel
}

// WithCookie returns a clone of the context with a new cookie value.
func (c *TaskContext) WithCookie(cookie interface{}) *TaskContext {
	newCtx := c.clone()
	newCtx.cookie = cookie
	return newCtx
}

// SetNoRetry set the no-retry flag to prevent a retry on the next execution.
func (c *TaskContext) SetNoRetry() {
	c.noRetry = true
}

// NoRetry returns the no-retry flag.
func (c *TaskContext) NoRetry() bool {
	return c.noRetry
}

// ExecCount returns how many times the job was already executed, 0 on the first try.
func (c *TaskContext) ExecCount() int {
	return c.execCount
}

func (c *TaskContext) clone() *TaskContext {
	return &TaskContext{
		Context:   c.Context,
		Instance:  c.Instance,
		Publisher: c.Publisher,
		job:       c.job,
		log:       c.log,
		id:        c.id,
		cookie:    c.cookie,
	}
}

// ID returns a unique identifier for the worker context.
func (c *TaskContext) ID() string {
	return c.id
}

// Logger return the logger associated with the worker context.
func (c *TaskContext) Logger() logger.Logger {
	return c.log
}

// UnmarshalMessage unmarshals the message contained in the worker context.
func (c *TaskContext) UnmarshalMessage(v interface{}) error {
	return c.job.Message.Unmarshal(v)
}

// UnmarshalEvent unmarshals the event contained in the worker context.
func (c *TaskContext) UnmarshalEvent(v interface{}) error {
	if c.job == nil || c.job.Event == nil {
		return errors.New("jobs: does not have an event associated")
	}
	return c.job.Event.Unmarshal(v)
}

// UnmarshalPayload unmarshals the payload contained in the worker context.
func (c *TaskContext) UnmarshalPayload() (map[string]interface{}, error) {
	var payload map[string]interface{}
	if err := c.job.Payload.Unmarshal(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// TriggerID returns the possible trigger identifier responsible for launching
// the job.
func (c *TaskContext) TriggerID() (string, bool) {
	triggerID := c.job.TriggerID
	return triggerID, triggerID != ""
}

// Cookie returns the cookie associated with the worker context.
func (c *TaskContext) Cookie() interface{} {
	return c.cookie
}

// Manual returns if the job was started manually
func (c *TaskContext) Manual() bool {
	return c.job.Manual
}

// NewWorker creates a new instance of Worker with the given configuration.
func NewWorker(conf *WorkerConfig) *Worker {
	return &Worker{
		Type: conf.WorkerType,
		Conf: conf,
	}
}

// Start is used to start the worker consumption of messages from its queue.
func (w *Worker) Start(jobs chan *Job) error {
	if !atomic.CompareAndSwapUint32(&w.running, 0, 1) {
		return ErrClosed
	}
	w.jobs = jobs
	w.closing = make(chan struct{}, w.Conf.Concurrency)
	w.closed = make(chan struct{})
	if w.Conf.WorkerInit != nil {
		if err := w.Conf.WorkerInit(); err != nil {
			return fmt.Errorf("Could not start worker %s: %s", w.Type, err)
		}
	}
	for i := 0; i < w.Conf.Concurrency; i++ {
		name := fmt.Sprintf("%s/%d", w.Type, i)
		joblog.Debugf("Start worker %s", name)
		go w.work(name)
	}
	return nil
}

// Shutdown is used to close the worker, waiting for all tasks to end
func (w *Worker) Shutdown(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&w.running, 1, 0) {
		return ErrClosed
	}
	close(w.jobs)
	for i := 0; i < w.Conf.Concurrency; i++ {
		w.closing <- struct{}{}
	}
	for i := 0; i < w.Conf.Concurrency; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-w.closed:
		}
	}
	return nil
}

func (w *Worker) work(workerID string) {
	for job := range w.jobs {
		domain := job.Domain
		if domain == "" {
			joblog.Errorf("%s: missing domain from job request", workerID)
			continue
		}
		var inst *instance.Instance
		if domain != prefixer.GlobalPrefixer.DomainName() {
			var err error
			inst, err = instance.Get(job.Domain)
			if err != nil {
				joblog.Errorf("Instance not found for %s: %s", job.Domain, err)
				continue
			}
			// Do not execute jobs for instances with blocking not signed TOS,
			// except for:
			// - mails because the user may needs a mail to login and accept
			//   the new TOS (2FA, password reset, etc.)
			// - migrations because the old version may be no longer supported
			//   when the user will sign the TOS
			if w.Type != "sendmail" && w.Type != "migrations" {
				notSigned, deadline := inst.CheckTOSNotSignedAndDeadline()
				if notSigned && deadline == instance.TOSBlocked {
					continue
				}
			}
		}
		w.runTask(inst, workerID, job)
	}
	joblog.Debugf("%s: worker shut down", workerID)
	w.closed <- struct{}{}
}

func (w *Worker) runTask(inst *instance.Instance, workerID string, job *Job) {
	taskCtx, cancel := NewTaskContext(workerID, job, inst)
	defer cancel()
	taskCtx.Publisher = w.Conf.Publisher
	if err := job.AckConsumed(); err != nil {
		taskCtx.Logger().Errorf("error acking consume job: %s",
			err.Error())
		return
	}
	t := &task{
		w:    w,
		ctx:  taskCtx,
		job:  job,
		conf: w.defaultedConf(job.Options),
	}

	ch := make(chan error)
	go func() {
		errRun := t.run()
		if errRun == ErrAbort {
			errRun = nil
		}
		ch <- errRun
	}()

	var errRun error
	select {
	case <-w.closing:
		cancel()
		errRun = <-ch
	case errRun = <-ch:
	}

	var runResultLabel string
	var errAck error
	if errRun != nil {
		taskCtx.Logger().Errorf("error while performing job: %s",
			errRun.Error())
		runResultLabel = metrics.WorkerExecResultErrored
		errAck = job.Nack(errRun.Error())
	} else {
		runResultLabel = metrics.WorkerExecResultSuccess
		errAck = job.Ack()
	}

	// Distinguish classic job execution and konnector/account deletion
	msg := struct {
		Account        string `json:"account"`
		AccountRev     string `json:"account_rev"`
		Konnector      string `json:"konnector"`
		AccountDeleted bool   `json:"account_deleted"`
	}{}
	err := json.Unmarshal(job.Message, &msg)

	if err == nil && w.Type == "konnector" && msg.AccountDeleted {
		metrics.WorkerKonnectorExecDeleteCounter.WithLabelValues(w.Type, runResultLabel).Inc()
	} else {
		metrics.WorkerExecCounter.WithLabelValues(w.Type, runResultLabel).Inc()
	}

	if errAck != nil {
		taskCtx.Logger().Errorf("error while acking job done: %s",
			errAck.Error())
	}

	// Delete the trigger associated with the job (if any) when we receive a
	// BadTriggerError.
	if job.TriggerID != "" && globalJobSystem != nil {
		if _, ok := errRun.(BadTriggerError); ok {
			_ = globalJobSystem.DeleteTrigger(job, job.TriggerID)
		}
	}
}

func (w *Worker) defaultedConf(opts *JobOptions) *WorkerConfig {
	c := w.Conf.Clone()
	if c.Concurrency == 0 {
		c.Concurrency = defaultConcurrency
	}
	if c.MaxExecCount == 0 {
		c.MaxExecCount = defaultMaxExecCount
	}
	if c.RetryDelay == 0 {
		c.RetryDelay = defaultRetryDelay
	}
	if c.Timeout == 0 {
		c.Timeout = defaultTimeout
	}
	if opts == nil {
		return c
	}
	if opts.MaxExecCount != 0 && opts.MaxExecCount < c.MaxExecCount {
		c.MaxExecCount = opts.MaxExecCount
	}
	if opts.Timeout > 0 && opts.Timeout < c.Timeout {
		c.Timeout = opts.Timeout
	}
	return c
}

type task struct {
	w    *Worker
	ctx  *TaskContext
	conf *WorkerConfig
	job  *Job

	startTime time.Time
	endTime   time.Time
	execCount int
}

func (t *task) run() (err error) {
	t.startTime = time.Now()
	t.execCount = 0

	if t.conf.WorkerStart != nil {
		t.ctx, err = t.conf.WorkerStart(t.ctx)
		if err != nil {
			return err
		}
	}
	defer func() {
		if t.conf.WorkerCommit != nil {
			t.ctx.log = t.ctx.Logger().WithField("exec_time", t.endTime.Sub(t.startTime))
			if errc := t.conf.WorkerCommit(t.ctx, err); errc != nil {
				t.ctx.Logger().Warnf("Error while committing job: %s",
					errc.Error())
			}
		}
	}()
	for {
		retry, delay, timeout := t.nextDelay(err)

		// The optional ErrorHook function allows to prevent retries depending
		// on the previous error
		if retry && t.conf.ErrorHook != nil {
			retry = t.conf.ErrorHook(err)
		}
		if !retry {
			break
		}
		if err != nil {
			t.ctx.Logger().Warnf("Error while performing job: %s (retry in %s)",
				err.Error(), delay)
		}

		if delay > 0 {
			time.Sleep(delay)
		}

		t.ctx.Logger().Debugf("Executing job (%d) (timeout set to %s)",
			t.execCount, timeout)

		var execResultLabel string
		timer := prometheus.NewTimer(prometheus.ObserverFunc(func(v float64) {
			metrics.WorkerExecDurations.WithLabelValues(t.w.Type, execResultLabel).Observe(v)
		}))

		ctx, cancel := t.ctx.WithTimeout(timeout)
		ctx.execCount = t.execCount
		err = t.exec(ctx)
		if err == nil {
			execResultLabel = metrics.WorkerExecResultSuccess
			timer.ObserveDuration()
			t.endTime = time.Now()
			cancel()
			break
		}
		execResultLabel = metrics.WorkerExecResultErrored
		timer.ObserveDuration()
		t.endTime = time.Now()

		// Incrementing timeouts counter
		if t.job.Message != nil {
			var slug string
			var msg map[string]interface{}

			if errd := json.Unmarshal(t.job.Message, &msg); errd != nil {
				ctx.Logger().Errorf("Cannot unmarshal job message %s", t.job.Message)
			} else {
				switch t.w.Type {
				case "konnector":
					slug, _ = msg["konnector"].(string)
				case "service":
					slug, _ = msg["slug"].(string)
				default:
					slug = ""
				}

				// Forcing the timeout counter to 0 if it has not been initialized
				metrics.WorkerExecTimeoutsCounter.WithLabelValues(t.w.Type, slug)

				if errors.Is(err, context.DeadlineExceeded) { // This is a timeout
					metrics.WorkerExecTimeoutsCounter.WithLabelValues(t.w.Type, slug).Inc()
				}
			}
		}

		// Even though ctx should have expired already, it is good practice to call
		// its cancelation function in any case. Failure to do so may keep the
		// context and its parent alive longer than necessary.
		cancel()
		t.execCount++

		if ctx.NoRetry() {
			break
		}
	}

	metrics.WorkerExecRetries.WithLabelValues(t.w.Type).Observe(float64(t.execCount))
	return
}

func (t *task) exec(ctx *TaskContext) (err error) {
	var slot struct{}
	if slots != nil {
		slot = <-slots
	}
	defer func() {
		if slots != nil {
			slots <- slot
		}
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("%v", r)
			}
			ctx.Logger().Errorf("[panic] %s: %s", r, debug.Stack())
		}
	}()
	return t.conf.WorkerFunc(ctx)
}

func (t *task) nextDelay(prevError error) (bool, time.Duration, time.Duration) {
	// for certain kinds of errors, we do not have a retry since these error
	// cannot be recovered from
	{
		if _, ok := prevError.(BadTriggerError); ok {
			return false, 0, 0
		}
		switch prevError {
		case ErrAbort, ErrMessageUnmarshal, ErrMessageNil:
			return false, 0, 0
		}
	}

	c := t.conf

	if t.execCount >= c.MaxExecCount {
		return false, 0, 0
	}

	// the worker timeout should take into account the maximum execution time
	// allowed to the task
	timeout := c.Timeout

	var nextDelay time.Duration
	if t.execCount == 0 {
		// on first execution, execute immediately
		nextDelay = 0
	} else {
		nextDelay = c.RetryDelay << uint(t.execCount-1)

		// fuzzDelay number between delay * (1 +/- 0.1)
		fuzzDelay := int(0.1 * float64(nextDelay))
		nextDelay += time.Duration((rand.Intn(2*fuzzDelay) - fuzzDelay))
	}

	return true, nextDelay, timeout
}
