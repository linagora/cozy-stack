package rag_test

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queryInBackground runs the rag-query job and returns a channel giving its
// error when it is over.
func queryInBackground(r *ragTest, query rag.QueryMessage) chan error {
	done := make(chan error, 1)
	go func() { done <- rag.Query(context.Background(), r.inst, rag.TestingLogger(), query) }()
	return done
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

func waitQuery(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the query does not stop")
	}
}

func messageCount(t *testing.T, r *ragTest, id string) int {
	t.Helper()
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, id, &chat))
	return len(chat.Messages)
}

func TestCancelStopsTheAnswer(t *testing.T) {
	r := newRAGTest(t)
	started := make(chan struct{})
	disconnected := make(chan struct{})
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "The budget "}}], "extra": {}}`+"\n\n")
		w.(http.Flusher).Flush()
		close(started)
		// A long answer: openRAG goes on until the stack goes away
		<-req.Context().Done()
		close(disconnected)
	}
	query := newConversation(t, r, "conversation-cancel", "Quel est le budget du projet Atlas ?", nil)
	query.Stream = true
	events := subscribeChatEvents(t, r)

	done := queryInBackground(r, query)
	waitFor(t, started, "the answer to start")
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))

	waitQuery(t, done)
	waitFor(t, disconnected, "openRAG to see the stack go away")
	assert.Equal(t, []string{"delta", "done"}, objects(events()), "the clients are told it is over, without an error")
	assert.Equal(t, 1, messageCount(t, r, query.DocID), "no answer is saved")
}

func TestCancelBeforeTheQueryStarts(t *testing.T) {
	r := newRAGTest(t)
	query := newConversation(t, r, "conversation-cancel-early", "Quel est le budget du projet Atlas ?", nil)
	events := subscribeChatEvents(t, r)

	// The user stops the answer before the job runs
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	assert.Equal(t, []string{"done"}, objects(events()))
	assert.Empty(t, completions(t, r.fake, false), "openRAG is not asked")
	assert.Equal(t, 1, messageCount(t, r, query.DocID))
}

func TestCancelDoesNotStopTheNextMessage(t *testing.T) {
	r := newRAGTest(t)
	query := newConversation(t, r, "conversation-cancel-next", "Bonjour", nil)
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	// Nothing is running: the cancel request is a no-op
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))

	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	chat.Messages = append(chat.Messages, rag.ChatMessage{ID: "second", Role: rag.UserRole, Content: "Et le budget ?", CreatedAt: time.Now()})
	require.NoError(t, couchdb.UpdateDoc(r.inst, &chat))
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, "fake answer", lastMessage(t, r, query.DocID).Content)

	// The cancel request of a message that did not get its answer does not
	// stop the next one either
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	chat.Messages = append(chat.Messages, rag.ChatMessage{ID: "third", Role: rag.UserRole, Content: "Et les risques ?", CreatedAt: time.Now()})
	require.NoError(t, couchdb.UpdateDoc(r.inst, &chat))
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	chat.Messages = append(chat.Messages, rag.ChatMessage{ID: "fourth", Role: rag.UserRole, Content: "Et le planning ?", CreatedAt: time.Now()})
	require.NoError(t, couchdb.UpdateDoc(r.inst, &chat))
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, "fake answer", lastMessage(t, r, query.DocID).Content)
}

func TestCancelStopsTheWritingOfANote(t *testing.T) {
	r := newRAGTest(t)
	writing := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "create_note", "needs_documents": false}`
		}
		// The note is being written when the user stops it
		close(writing)
		<-release
		return "# Note\n\n- written too late"
	}
	query := newConversation(t, r, "conversation-cancel-note", "Résume cette conversation dans une note", allActions)
	query.Stream = true
	events := subscribeChatEvents(t, r)

	done := queryInBackground(r, query)
	waitFor(t, writing, "the note to be written")
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))

	waitQuery(t, done)
	assert.Equal(t, []string{"done"}, objects(events()), "no action is proposed")
	assert.Equal(t, 1, messageCount(t, r, query.DocID))
}
