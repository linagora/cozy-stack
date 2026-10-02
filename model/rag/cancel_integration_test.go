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
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newConversation saves a conversation with one user message, and returns
// the query of the rag-query job for it.
func newConversation(t *testing.T, r *ragTest, id, question string) rag.QueryMessage {
	t.Helper()
	chat := rag.ChatConversation{
		DocID:        id,
		Messages:     []rag.ChatMessage{{ID: id + "-q", Role: rag.UserRole, Content: question, CreatedAt: time.Now()}},
		CozyMetadata: metadata.New(),
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(r.inst, &chat))
	return rag.QueryMessage{Task: "chat-completion", DocID: id}
}

// addQuestion adds a user message to the conversation.
func addQuestion(t *testing.T, r *ragTest, id, msgID, question string) {
	t.Helper()
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, id, &chat))
	chat.Messages = append(chat.Messages, rag.ChatMessage{ID: msgID, Role: rag.UserRole, Content: question, CreatedAt: time.Now()})
	require.NoError(t, couchdb.UpdateDoc(r.inst, &chat))
}

// subscribeChatEvents returns a function giving the objects of the chat
// events published so far, up to the `done` or `error` one.
func subscribeChatEvents(t *testing.T, r *ragTest) func() []string {
	t.Helper()
	sub := realtime.GetHub().Subscriber(r.inst)
	sub.Subscribe(consts.ChatEvents)
	t.Cleanup(sub.Close)
	return func() []string {
		var objects []string
		for {
			select {
			case e := <-sub.Channel:
				object, _ := e.Doc.(*couchdb.JSONDoc).M["object"].(string)
				objects = append(objects, object)
				if object == "done" || object == "error" {
					return objects
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no done event, got %v", objects)
			}
		}
	}
}

func roles(t *testing.T, r *ragTest, id string) []string {
	t.Helper()
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, id, &chat))
	out := make([]string, len(chat.Messages))
	for i, msg := range chat.Messages {
		out[i] = msg.Role
	}
	return out
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
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
	query := newConversation(t, r, "conversation-cancel", "Quel est le budget du projet Atlas ?")
	query.Stream = true
	events := subscribeChatEvents(t, r)

	done := make(chan error, 1)
	go func() { done <- rag.Query(context.Background(), r.inst, rag.TestingLogger(), query) }()
	waitFor(t, started, "the answer to start")
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the query does not stop")
	}
	waitFor(t, disconnected, "openRAG to see the stack go away")
	assert.Equal(t, []string{"delta", "done"}, events(), "the clients are told it is over, without an error")
	assert.Equal(t, []string{rag.UserRole}, roles(t, r, query.DocID), "no answer is saved")
}

func TestCancelBeforeTheQueryStarts(t *testing.T) {
	r := newRAGTest(t)
	query := newConversation(t, r, "conversation-cancel-early", "Quel est le budget du projet Atlas ?")
	events := subscribeChatEvents(t, r)

	// The user stops the answer before the job runs
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	assert.Equal(t, []string{"done"}, events())
	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodPost, "/v1/chat/completions"), "openRAG is not asked")
	assert.Equal(t, []string{rag.UserRole}, roles(t, r, query.DocID))
}

func TestCancelDoesNotStopTheNextMessage(t *testing.T) {
	r := newRAGTest(t)
	query := newConversation(t, r, "conversation-cancel-next", "Bonjour")
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	// Nothing is running: the cancel request is a no-op
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))
	addQuestion(t, r, query.DocID, "second", "Et le budget ?")
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{rag.UserRole, rag.AssistantRole, rag.UserRole, rag.AssistantRole}, roles(t, r, query.DocID))

	// The cancel request of a message that did not get its answer does not
	// stop the next one either
	addQuestion(t, r, query.DocID, "third", "Et les risques ?")
	require.NoError(t, rag.CancelChat(r.inst, query.DocID))
	addQuestion(t, r, query.DocID, "fourth", "Et le planning ?")
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{rag.UserRole, rag.AssistantRole, rag.UserRole, rag.AssistantRole, rag.UserRole, rag.UserRole, rag.AssistantRole},
		roles(t, r, query.DocID), "the fourth question is answered")
}
