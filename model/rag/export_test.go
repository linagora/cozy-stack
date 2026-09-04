package rag

// Test-only bridge to the checkpoint primitives.
//
// checkpoint_test.go must live in an external "rag_test" package: the
// package rag itself is imported by model/instance/lifecycle, which
// tests/testutils imports, so an internal "package rag" test file cannot
// import tests/testutils without creating an import cycle. This file stays
// free of that import and merely re-exports the unexported symbols under
// test, following the same export_test.go pattern already used by
// model/move.
type Checkpoint = checkpoint

var (
	LoadCheckpoint   = loadCheckpoint
	SaveCheckpoint   = saveCheckpoint
	DeleteCheckpoint = deleteCheckpoint
)

const MaxBatchRetries = maxBatchRetries

var IsRetryable = isRetryable
