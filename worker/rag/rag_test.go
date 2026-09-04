package rag

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/require"
)

func findWorker(t *testing.T, workerType string) *job.WorkerConfig {
	t.Helper()
	workers, err := job.GetWorkersList()
	require.NoError(t, err)
	for _, w := range workers {
		if w.WorkerType == workerType {
			return w
		}
	}
	t.Fatalf("worker %q is not registered", workerType)
	return nil
}

func TestRagIndexWorkerConfig(t *testing.T) {
	config.UseTestFile(t)

	index := findWorker(t, "rag-index")
	require.False(t, index.Reserved, "rag-index must be creatable by apps (triggers and jobs)")
	require.Equal(t, 3, index.MaxExecCount)
	require.Equal(t, 30*time.Second, index.RetryDelay)

	query := findWorker(t, "rag-query")
	require.True(t, query.Reserved, "rag-query stays reserved to the stack")
}
