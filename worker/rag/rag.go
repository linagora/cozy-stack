package rag

import (
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/rag"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "rag-index",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 3,
		RetryDelay:   30 * time.Second,
		Timeout:      15 * time.Minute,
		WorkerFunc:   WorkerIndex,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "rag-query",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 1,
		Reserved:     true,
		Timeout:      15 * time.Minute,
		WorkerFunc:   WorkerQuery,
	})
}

func WorkerIndex(ctx *job.TaskContext) error {
	logger := ctx.Logger()
	var msg rag.IndexMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	logger.Debugf("RAG: index %s", msg.Doctype)
	return rag.Index(ctx.Instance, logger, msg)
}

func WorkerQuery(ctx *job.TaskContext) error {
	logger := ctx.Logger()
	var msg rag.QueryMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	logger.Debugf("RAG: query %v", msg)
	return rag.Query(ctx.Instance, logger, msg)
}
