package rag_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

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
		Enabled:   true,
		Nodes:     map[string]config.RabbitMQNode{config.DefaultInstanceContext: {Enabled: true}},
		Exchanges: []config.RabbitExchange{{Name: rag.IndexExchange, Kind: "topic"}},
	}
	t.Cleanup(func() { cfg.RabbitMQ = previous })
}

func (r *ragTest) settleWith(pub messaging.Publisher) {
	r.t.Helper()
	require.NoError(r.t, rag.Index(context.Background(), r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files}, pub))
	for _, dirID := range r.drainReconcileJobs() {
		require.NoError(r.t, rag.Index(context.Background(), r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files, ReconcileDirID: dirID}, pub))
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
	assert.Len(t, pub.msgs, 1, "the feed leaves the file to the reconcile job of the new folder: one message")

	msg := pub.msgs[0]
	assert.Equal(t, "sync", msg["action"])
	assert.Equal(t, r.inst.Domain, msg["partition"])
	assert.Equal(t, inKB.Rev(), msg["doc_rev"])
	assert.Equal(t, r.inst.PageURL(rag.IndexFilesPath+inKB.DocID, nil), msg["state_url"])
	assert.Equal(t, r.inst.PageURL(rag.IndexStatusPath, nil), msg["callback_url"])
	assert.Equal(t, r.fake.Server.URL, msg["rag_base_url"])
	assert.NotContains(t, msg, "rag_api_key", "the indexer holds the keys")
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

// The indexer checks the formats: the stack publishes every file of the
// knowledge base, and never asks openRAG which formats it indexes.
func TestBrokerLeavesTheFormatsToTheIndexer(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	pub := &recordingPublisher{}
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.md", "# alpha")
	js := r.writeFile("/KB/bundle.js", "console.log(1)")
	r.addAssistant("KB assistant", kb.DocID)
	r.fake.Fail = func(method, path string) int {
		if path == "/indexer/supported/types" {
			t.Errorf("the stack asked openRAG for its formats")
		}
		return 0
	}

	r.settleWith(pub)

	assert.ElementsMatch(t, []string{doc.DocID, js.DocID}, pub.fileIDs())
}

// A file changed in a knowledge base folder already indexed is sent by the
// feed, once.
func TestBrokerFeedPublishesTheChangesOfAnIndexedFolder(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)
	r.settleWith(&recordingPublisher{})

	doc := r.writeFile("/KB/a.txt", "alpha")
	pub := &recordingPublisher{}
	r.settleWith(pub)

	assert.Equal(t, []string{doc.DocID}, pub.fileIDs())
	assert.Len(t, pub.msgs, 1)
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

// RabbitMQ turned off by its global switch, the node and the exchange still
// listed: the workers then get a publisher that always fails, so the files
// must be uploaded directly.
func TestBrokerDisabledUploadsDirectly(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	config.GetConfig().RabbitMQ.Enabled = false
	pub := &recordingPublisher{}
	kb := r.mkdir("/KB")
	doc := r.writeFile("/KB/a.txt", "alpha")
	r.addAssistant("KB assistant", kb.DocID)

	r.settleWith(pub)

	assert.Empty(t, pub.msgs)
	assert.Positive(t, r.uploads(doc))
}

// The state and the content are read when the indexer processes the message:
// a class disabled after the publication makes the file ineligible.
func TestBrokerMediaFlagReadWhenProcessed(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	kb := r.mkdir("/KB")
	img := r.writeFile("/KB/pic.jpg", "not really a jpeg")
	r.addAssistant("KB assistant", kb.DocID)
	r.inst.FeatureFlags = map[string]interface{}{"rag.index.image.enabled": true}
	t.Cleanup(func() { r.inst.FeatureFlags = nil })
	pub := &recordingPublisher{}
	r.settleWith(pub)
	require.Equal(t, []string{img.DocID}, pub.fileIDs())
	_, err := rag.GetFileState(r.inst, img.DocID)
	require.NoError(t, err)
	content, err := rag.OpenFileContent(r.inst, img.DocID)
	require.NoError(t, err)
	content.Close()

	r.inst.FeatureFlags = nil

	_, err = rag.GetFileState(r.inst, img.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = rag.OpenFileContent(r.inst, img.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

// blockingPublisher never gets the broker's confirmation: it returns when
// the context is done.
type blockingPublisher struct{}

func (blockingPublisher) Publish(ctx context.Context, _ messaging.PublishRequest) error {
	<-ctx.Done()
	return ctx.Err()
}

// The deadline of the job bounds the publications.
func TestBrokerPublishHonoursTheJobDeadline(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	kb := r.mkdir("/KB")
	r.addAssistant("KB assistant", kb.DocID)
	r.settleWith(&recordingPublisher{})
	r.writeFile("/KB/a.txt", "alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := rag.Index(ctx, r.inst, rag.TestingLogger(), rag.IndexMessage{Doctype: consts.Files}, blockingPublisher{})

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}

// A folder leaving the knowledge bases: its files get a sync message, the
// stack does not touch their memberships on openRAG itself.
func TestBrokerFolderLeavingTheKnowledgeBasesAsksForSyncs(t *testing.T) {
	r := newRAGTest(t)
	enableBroker(t)
	kb := r.mkdir("/KB")
	other := r.mkdir("/KB/Other")
	alone := r.writeFile("/KB/a.txt", "alpha")
	shared := r.writeFile("/KB/Other/b.txt", "beta")
	assistant := r.addAssistant("KB assistant", kb.DocID)
	r.addAssistant("Other assistant", other.DocID)
	r.settleWith(&recordingPublisher{})
	// What the indexer would have made of the messages.
	r.fake.AddFile(alone.DocID, hex.EncodeToString(alone.MD5Sum), workspaceIDs(kb.DocID)...)
	r.fake.AddFile(shared.DocID, hex.EncodeToString(shared.MD5Sum), workspaceIDs(kb.DocID, other.DocID)...)

	r.deleteAssistant(assistant)
	pub := &recordingPublisher{}
	r.settleWith(pub)

	assert.False(t, r.fake.HasWorkspace(kb.DocID), "the stack still removes the workspace")
	assert.ElementsMatch(t, []string{alone.DocID, shared.DocID}, pub.fileIDs())
	assert.Zero(t, r.requestsAbout(alone), "the indexer deletes the file")
	assert.Zero(t, r.requestsAbout(shared), "the indexer sets the memberships")
	_, err := rag.GetFileState(r.inst, alone.DocID)
	assert.ErrorIs(t, err, os.ErrNotExist)
	state, err := rag.GetFileState(r.inst, shared.DocID)
	require.NoError(t, err)
	assert.Equal(t, workspaceIDs(other.DocID), state.WorkspaceIDs)
}
