// Package search registers the worker that indexes the files for the
// server-side file search.
package search

import (
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/search"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   search.WorkerType,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 3,
		RetryDelay:   30 * time.Second,
		Timeout:      15 * time.Minute,
		WorkerFunc: func(ctx *job.TaskContext) error {
			return search.Index(ctx, ctx.Instance, ctx.Logger())
		},
	})
}
