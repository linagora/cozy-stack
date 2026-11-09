package rag_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingPublisher keeps the messages instead of sending them.
type recordingPublisher struct {
	msgs []map[string]interface{}
}

func (p *recordingPublisher) Publish(_ context.Context, req messaging.PublishRequest) error {
	if req.Exchange != rag.IndexExchange || req.RoutingKey != rag.IndexRoutingKey {
		panic("unexpected destination " + req.Exchange + "/" + req.RoutingKey)
	}
	// Through JSON, the way the indexer reads it.
	raw, err := json.Marshal(req.Payload)
	if err != nil {
		return err
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}
	p.msgs = append(p.msgs, msg)
	return nil
}

// fileIDs returns the ids of the files the messages are about, once each.
func (p *recordingPublisher) fileIDs() []string {
	var ids []string
	seen := map[string]bool{}
	for _, msg := range p.msgs {
		id, _ := msg["file_id"].(string)
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// enableBroker configures the exchange of the indexer and a RabbitMQ node.
func enableBroker(t *testing.T) {
	cfg := config.GetConfig()
	previous := cfg.RabbitMQ
	cfg.RabbitMQ = config.RabbitMQ{
		Nodes:     map[string]config.RabbitMQNode{config.DefaultInstanceContext: {Enabled: true}},
		Exchanges: []config.RabbitExchange{{Name: rag.IndexExchange, Kind: "topic"}},
	}
	t.Cleanup(func() { cfg.RabbitMQ = previous })
}

func (r *ragTest) settleWith(pub messaging.Publisher) {
	r.t.Helper()
	require.NoError(r.t, rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files}, pub))
	for _, dirID := range r.drainReconcileJobs() {
		require.NoError(r.t, rag.Index(r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files, ReconcileDirID: dirID}, pub))
	}
}

func TestBrokerPublishesInsteadOfUploading(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	pub := &recordingPublisher{}
	kb := r.mkdir("/KB")
	r.mkdir("/Other")
	inKB := r.writeFile("/KB/a.txt", "alpha")
	outside := r.writeFile("/Other/c.txt", "gamma")
	r.addAssistant("KB assistant", kb.DocID)

	r.settleWith(pub)

	assert.True(t, r.fake.HasWorkspace(kb.DocID), "the stack still creates the workspaces")
	assert.Equal(t, 0, r.uploads(inKB), "the indexer uploads, not the stack")
	assert.Equal(t, []string{inKB.DocID}, pub.fileIDs(), "a new file out of scope was never indexed: no message")

	msg := pub.msgs[0]
	assert.Equal(t, r.inst.Domain, msg["partition"])
	assert.Equal(t, r.inst.PageURL(rag.IndexFilesPath+inKB.DocID, nil), msg["file_url"])
	assert.Equal(t, r.inst.PageURL(rag.IndexStatusPath, nil), msg["callback_url"])
	assert.Equal(t, map[string]interface{}{"base_url": r.fake.Server.URL, "api_key": r.fake.Server.APIKey}, msg["rag"])
	token, _ := msg["file_token"].(string)
	assert.NoError(t, rag.CheckIndexToken(r.inst, token, inKB.DocID))
	assert.Error(t, rag.CheckIndexToken(r.inst, token, outside.DocID), "the token is only good for its file")

	state, err := rag.GetFileState(r.inst, inKB.DocID)
	require.NoError(t, err)
	assert.Equal(t, "a.txt", state.Name)
	assert.Equal(t, hex.EncodeToString(inKB.MD5Sum), state.MD5Sum)
	assert.Equal(t, workspaceIDs(kb.DocID), state.WorkspaceIDs)
	assert.Equal(t, inKB.Rev(), state.Metadata["doc_rev"])
	_, err = rag.GetFileState(r.inst, outside.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist, "out of scope: must not be on openRAG")
}

func TestBrokerMoveOutOfScopeAsksForASync(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	kb := r.mkdir("/KB")
	other := r.mkdir("/Other")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.settleWith(&recordingPublisher{})

	_, err := vfs.ModifyFileMetadata(r.inst.VFS(), doc, &vfs.DocPatch{DirID: &other.DocID})
	require.NoError(t, err)
	pub := &recordingPublisher{}
	r.settleWith(pub)

	assert.Equal(t, []string{doc.DocID}, pub.fileIDs())
	_, err = rag.GetFileState(r.inst, doc.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist, "the indexer will delete it")
}

func TestBrokerTrashedFileHasNoState(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)
	r.settleWith(&recordingPublisher{})

	_, err := vfs.TrashFile(r.inst.VFS(), doc)
	require.NoError(t, err)
	pub := &recordingPublisher{}
	r.settleWith(pub)

	assert.Equal(t, []string{doc.DocID}, pub.fileIDs())
	_, err = rag.GetFileState(r.inst, doc.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = rag.OpenFileContent(r.inst, doc.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestBrokerNotConfiguredUploadsDirectly(t *testing.T) {
	r := newRAGTest(t)
	pub := &recordingPublisher{}
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)

	r.settleWith(pub)

	assert.Empty(t, pub.msgs, "without the exchange, nothing is published")
	assert.Positive(t, r.uploads(doc))
}
