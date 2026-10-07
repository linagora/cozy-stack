// Package broker is the worker publishing the events of the notification
// center on RabbitMQ.
package broker

import (
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
)

func init() {
	// The retries, 5s apart then doubling, span about 2 minutes so that an
	// event survives a broker restart, and a job run before rabbitmq.Init.
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "broker",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 6,
		RetryDelay:   5 * time.Second,
		Timeout:      30 * time.Second,
		Reserved:     true,
		WorkerFunc:   Worker,
	})
}

// Worker publishes a rabbitmq.PublishRequest with the stack's RabbitMQ service.
func Worker(ctx *job.TaskContext) error {
	var req rabbitmq.PublishRequest
	if err := ctx.UnmarshalMessage(&req); err != nil {
		return err
	}
	return rabbitmq.Default().Publish(ctx, req)
}
