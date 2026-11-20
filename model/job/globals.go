// Package job is for the scheduling and execution of asynchronous jobs via the
// workers. The scheduling is done via the triggers. The jobs are put in queues
// before being processed by a worker.
package job

import (
	"context"
	"errors"

	"github.com/cozy/cozy-stack/pkg/utils"
)

// JobSystem is a pair of broker, scheduler linked together.
type JobSystem interface {
	Broker
	Scheduler
	utils.Shutdowner
}

type jobSystem struct {
	Broker
	Scheduler
	reports *expiredLinkScheduler
}

// Shutdown shuts down the job system. Implement the utils.Shutdowner
// interface.
func (j *jobSystem) Shutdown(ctx context.Context) error {
	// Don't admit new report jobs while the consumers are shutting down.
	reportErr := j.reports.stop(ctx)
	if err := j.Broker.ShutdownWorkers(ctx); err != nil {
		return errors.Join(reportErr, err)
	}
	return errors.Join(reportErr, j.Scheduler.ShutdownScheduler(ctx))
}

var globalJobSystem JobSystem

// SystemStart initializes and starts the global jobs system with the given
// broker, scheduler instances and workers list.
func SystemStart(b Broker, s Scheduler, workersList WorkersList) error {
	if globalJobSystem != nil {
		panic("Job system already started")
	}
	system := &jobSystem{Broker: b, Scheduler: s}
	globalJobSystem = system
	if err := b.StartWorkers(workersList); err != nil {
		return err
	}
	if err := s.StartScheduler(b); err != nil {
		return err
	}
	system.reports = startExpiredLinkScheduler(b)
	return nil
}

// System returns the global job system.
func System() JobSystem {
	if globalJobSystem == nil {
		panic("Job system not initialized")
	}
	return globalJobSystem
}

// SystemUnsafe returns the global job system, even if it's not initialized
func SystemUnsafe() JobSystem {
	return globalJobSystem
}
