package rag_test

import (
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPurge(t *testing.T) {
	r := newRAGTest(t)
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	r.writeFile("/Other/b.txt", "beta")
	r.addAssistant("KB assistant", kb.DocID)
	r.runUntilSettled()
	require.Equal(t, []string{inKB.DocID}, r.fake.FileIDs())
	require.True(t, r.fake.HasWorkspace(kb.DocID))

	require.NoError(t, rag.Purge(r.inst, rag.TestingLogger()))

	assert.Empty(t, r.fake.FileIDs())
	assert.False(t, r.fake.HasWorkspace(kb.DocID))
	var status rag.IndexStatus
	err := couchdb.GetDoc(r.inst, consts.ChatRAG, inKB.DocID, &status)
	assert.True(t, couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err), "index statuses are gone")
	lastSeq, _ := r.checkpoint()
	assert.Empty(t, lastSeq, "the checkpoint is dropped")

	// The next run indexes everything again.
	r.runUntilSettled()
	assert.Equal(t, []string{inKB.DocID}, r.fake.FileIDs())
	assert.True(t, r.fake.HasWorkspace(kb.DocID))
}
