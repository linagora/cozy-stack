package rag_test

// checkpoint_test.go lives in the external rag_test package because
// tests/testutils imports model/instance/lifecycle, which imports
// model/rag: an internal "package rag" test file cannot import testutils
// without an import cycle. See export_test.go for the unexported symbols
// this test needs (loadCheckpoint/saveCheckpoint/deleteCheckpoint/checkpoint,
// re-exported as LoadCheckpoint/SaveCheckpoint/DeleteCheckpoint/Checkpoint).

import (
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckpoint(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance()

	cp, err := rag.LoadCheckpoint(inst, consts.Files, "rag-index-test")
	require.NoError(t, err)
	assert.Equal(t, rag.Checkpoint{}, cp)

	require.NoError(t, rag.SaveCheckpoint(inst, consts.Files, "rag-index-test", rag.Checkpoint{LastSeq: "5-abc", Retries: 2}))
	cp, err = rag.LoadCheckpoint(inst, consts.Files, "rag-index-test")
	require.NoError(t, err)
	assert.Equal(t, rag.Checkpoint{LastSeq: "5-abc", Retries: 2}, cp)

	// An older sequence never overwrites a newer one.
	require.NoError(t, rag.SaveCheckpoint(inst, consts.Files, "rag-index-test", rag.Checkpoint{LastSeq: "3-old"}))
	cp, err = rag.LoadCheckpoint(inst, consts.Files, "rag-index-test")
	require.NoError(t, err)
	assert.Equal(t, "5-abc", cp.LastSeq)

	// The same sequence can be re-saved (retries bump).
	require.NoError(t, rag.SaveCheckpoint(inst, consts.Files, "rag-index-test", rag.Checkpoint{LastSeq: "5-abc", Retries: 3}))
	cp, err = rag.LoadCheckpoint(inst, consts.Files, "rag-index-test")
	require.NoError(t, err)
	assert.Equal(t, 3, cp.Retries)

	require.NoError(t, rag.DeleteCheckpoint(inst, consts.Files, "rag-index-test"))
	require.NoError(t, rag.DeleteCheckpoint(inst, consts.Files, "rag-index-test"), "deleting twice is fine")
	cp, err = rag.LoadCheckpoint(inst, consts.Files, "rag-index-test")
	require.NoError(t, err)
	assert.Equal(t, rag.Checkpoint{}, cp)
}
