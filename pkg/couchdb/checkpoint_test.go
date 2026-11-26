package couchdb

import (
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("a couchdb is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	const doctype, id = "io.cozy.tests.checkpoints", "a-job"
	require.NoError(t, ResetDB(TestPrefix, doctype))
	t.Cleanup(func() { _ = DeleteDB(TestPrefix, doctype) })
	load := func() Checkpoint {
		cp, err := LoadCheckpoint(TestPrefix, doctype, id)
		require.NoError(t, err)
		return cp
	}

	assert.Equal(t, Checkpoint{}, load(), "a missing checkpoint is an empty one")

	require.NoError(t, SaveCheckpoint(TestPrefix, doctype, id, Checkpoint{LastSeq: "5-abc", Retries: 2}))
	assert.Equal(t, Checkpoint{LastSeq: "5-abc", Retries: 2}, load())

	require.NoError(t, SaveCheckpoint(TestPrefix, doctype, id, Checkpoint{LastSeq: "3-def"}))
	assert.Equal(t, Checkpoint{LastSeq: "5-abc", Retries: 2}, load(), "an older sequence does not overwrite a more recent one")

	require.NoError(t, DeleteCheckpoint(TestPrefix, doctype, id))
	assert.Equal(t, Checkpoint{}, load())
	assert.NoError(t, DeleteCheckpoint(TestPrefix, doctype, id), "deleting a missing checkpoint is not an error")
}
