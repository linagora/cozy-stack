package job

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/redis/go-redis/v9"
)

const (
	// redisPrefix is the prefix for jobs queues in redis.
	redisPrefix = "j/"
	// redisHighPrioritySuffix suffix is the suffix used for prioritized queue.
	redisHighPrioritySuffix = "/p0"
)

type redisBroker struct {
	client         redis.UniversalClient
	scanLocks      lock.Getter
	ctx            context.Context
	workers        []*Worker
	workersRunning []*Worker
	workersTypes   []string
	running        uint32
	closed         chan struct{}
}

// NewRedisBroker creates a new broker that will use redis to distribute
// the jobs among several cozy-stack processes.
func NewRedisBroker(client redis.UniversalClient) Broker {
	return &redisBroker{
		client:    client,
		scanLocks: lock.NewRedisLockGetter(client),
		ctx:       context.Background(),
		closed:    make(chan struct{}),
	}
}

// StartWorkers polling jobs from redis queues
func (b *redisBroker) StartWorkers(ws WorkersList) error {
	if !atomic.CompareAndSwapUint32(&b.running, 0, 1) {
		return ErrClosed
	}

	for _, conf := range ws {
		b.workersTypes = append(b.workersTypes, conf.WorkerType)
		w := NewWorker(conf)
		b.workers = append(b.workers, w)
		if conf.Concurrency <= 0 {
			continue
		}
		b.workersRunning = append(b.workersRunning, w)
		ch := make(chan *Job)
		if err := w.Start(ch); err != nil {
			return err
		}
		go b.pollLoop(redisPrefix+conf.WorkerType, ch)
	}

	if len(b.workersRunning) > 0 {
		joblog.Infof("Started redis broker for %d workers type", len(b.workersRunning))
	}

	// XXX for retro-compat
	if slots := config.GetConfig().Jobs.NbWorkers; len(b.workersRunning) > 0 && slots > 0 {
		joblog.Warnf("Limiting the number of total concurrent workers to %d", slots)
		joblog.Warnf("Please update your configuration file to avoid a hard limit")
		setNbSlots(slots)
	}

	return nil
}

func (b *redisBroker) WorkersTypes() []string {
	return b.workersTypes
}

func (b *redisBroker) ShutdownWorkers(ctx context.Context) error {
	if !atomic.CompareAndSwapUint32(&b.running, 1, 0) {
		return ErrClosed
	}
	if len(b.workersRunning) == 0 {
		return nil
	}

	fmt.Print("  shutting down redis broker...")
	defer b.client.Close()

	for i := 0; i < len(b.workersRunning); i++ {
		select {
		case <-ctx.Done():
			fmt.Println("failed:", ctx.Err())
			return ctx.Err()
		case <-b.closed:
		}
	}

	errs := make(chan error)
	for _, w := range b.workersRunning {
		go func(w *Worker) { errs <- w.Shutdown(ctx) }(w)
	}

	var errm error
	for i := 0; i < len(b.workersRunning); i++ {
		if err := <-errs; err != nil {
			errm = multierror.Append(errm, err)
		}
	}

	if errm != nil {
		fmt.Println("failed: ", errm)
	} else {
		fmt.Println("ok")
	}
	return errm
}

var redisBRPopTimeout = 10 * time.Second

// SetRedisTimeoutForTest is used by unit test to avoid waiting 10 seconds on
// cleanup.
func SetRedisTimeoutForTest() {
	redisBRPopTimeout = 1 * time.Second
}

func (b *redisBroker) pollLoop(key string, ch chan<- *Job) {
	defer func() {
		b.closed <- struct{}{}
	}()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for {
		if atomic.LoadUint32(&b.running) == 0 {
			return
		}

		// The brpop redis command will always take elements in priority from the
		// first key containing elements at the call. By always priorizing the
		// manual queue, this would cause a starvation for our main queue if too
		// many "manual" jobs are pushed. By randomizing the order we make sure we
		// avoid such starvation. For one in three call, the main queue is
		// selected.
		keyP0 := key + redisHighPrioritySuffix
		keyP1 := key
		if rng.Intn(3) == 0 {
			keyP1, keyP0 = keyP0, keyP1
		}
		results, err := b.client.BRPop(b.ctx, redisBRPopTimeout, keyP0, keyP1).Result()
		if err != nil || len(results) < 2 {
			time.Sleep(100 * time.Millisecond)
			continue
		}

		key, val := results[0], results[1]
		if len(key) < len(redisPrefix) {
			joblog.Warnf("Invalid key %s", key)
			continue
		}

		parts := strings.SplitN(val, "/", 2)
		if len(parts) != 2 {
			joblog.Warnf("Invalid val %s", val)
			continue
		}

		jobID := parts[1]
		parts = strings.SplitN(parts[0], "%", 2)
		prefix := parts[0]
		var cluster int
		if len(parts) > 1 {
			cluster, _ = strconv.Atoi(parts[1])
		}
		job, err := Get(prefixer.NewPrefixer(cluster, "", prefix), jobID)
		if err != nil {
			joblog.Warnf("Cannot find job %s on domain %s (%d): %s",
				jobID, prefix, cluster, err)
			continue
		}

		ch <- job
	}
}

// PushJob will produce a new Job with the given options and enqueue the job in
// the proper queue.
func (b *redisBroker) PushJob(db prefixer.Prefixer, req *JobRequest) (*Job, error) {
	if atomic.LoadUint32(&b.running) == 0 {
		return nil, ErrClosed
	}

	var worker *Worker
	for _, w := range b.workers {
		if w.Type == req.WorkerType {
			worker = w
			break
		}
	}
	if worker == nil && req.WorkerType != "client" {
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

	if err := job.Create(); err != nil {
		return nil, err
	}

	// For client jobs, we don't need to enqueue the job in redis.
	if worker == nil {
		return job, nil
	}

	key := redisPrefix + job.WorkerType
	prefix := job.DBPrefix()
	if cluster := job.DBCluster(); cluster > 0 {
		prefix = fmt.Sprintf("%s%%%d", prefix, cluster)
	}
	val := prefix + "/" + job.JobID

	// When the job is manual, it is being pushed in a specific prioritized
	// queue.
	if job.Manual {
		key += redisHighPrioritySuffix
	}

	if err := b.client.LPush(b.ctx, key, val).Err(); err != nil {
		return nil, err
	}

	return job, nil
}

// QueueLen returns the size of the number of elements in queue of the
// specified worker type.
func (b *redisBroker) WorkerQueueLen(workerType string) (int, error) {
	return b.queueLen(workerType, false)
}

func (b *redisBroker) primaryQueueLen(workerType string) (int, error) {
	return b.queueLen(workerType, true)
}

func (b *redisBroker) queueLen(workerType string, primaryOnly bool) (int, error) {
	key := redisPrefix + workerType
	var depth int64
	for _, k := range []string{key, key + redisHighPrioritySuffix} {
		client := b.client
		if cluster, ok := client.(*redis.ClusterClient); ok && primaryOnly && cluster.Options().ReadOnly {
			// A lagging replica could report an empty queue and admit another wave.
			primary, err := cluster.MasterForKey(b.ctx, k)
			if err != nil {
				return 0, err
			}
			client = primary
		}
		n, err := client.LLen(b.ctx, k).Result()
		if err != nil {
			return 0, err
		}
		depth += n
	}
	return int(depth), nil
}

func (b *redisBroker) WorkerIsReserved(workerType string) (bool, error) {
	for _, w := range b.workers {
		if w.Type == workerType {
			return w.Conf.Reserved, nil
		}
	}
	return false, ErrUnknownWorker
}
