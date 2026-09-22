package rag_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/stretchr/testify/require"
)

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// lastCompletionMessages returns the messages of the last chat completion
// the fake openRAG received.
func lastCompletionMessages(t *testing.T, fake *rag.FakeOpenRAG) []completionMessage {
	t.Helper()
	var body []byte
	for _, req := range fake.Rec.All() {
		if req.Method == http.MethodPost && req.Path == "/v1/chat/completions" {
			body = req.Body
		}
	}
	require.NotNil(t, body, "no chat completion was sent to openRAG")
	var payload struct {
		Messages []completionMessage `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &payload))
	return payload.Messages
}

func TestQuerySendsTheAssistantPromptOnEveryChat(t *testing.T) {
	r := newRAGTest(t)
	assistant := couchdb.JSONDoc{Type: consts.ChatAssistants, M: map[string]interface{}{
		"name":   "Lawyer",
		"prompt": "Answer as a lawyer.",
	}}
	require.NoError(t, couchdb.CreateDoc(r.inst, &assistant))

	// A conversation created by an earlier version of the stack, which saved
	// the prompt of the assistant as a system message.
	chat := rag.ChatConversation{
		DocID: "conversation-with-a-saved-prompt",
		Messages: []rag.ChatMessage{
			{ID: "m0", Role: rag.SystemRole, Content: "Answer as a clerk.", CreatedAt: time.Now()},
			{ID: "m1", Role: rag.UserRole, Content: "Hello", CreatedAt: time.Now()},
		},
		CozyMetadata: metadata.New(),
		Rels: jsonapi.RelationshipMap{"assistant": jsonapi.Relationship{
			Data: map[string]interface{}{"_id": assistant.ID(), "_type": consts.ChatAssistants},
		}},
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(r.inst, &chat))
	query := rag.QueryMessage{Task: "chat-completion", DocID: chat.ID()}

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	require.Equal(t, []completionMessage{
		{Role: rag.SystemRole, Content: "Answer as a lawyer."},
		{Role: rag.UserRole, Content: "Hello"},
	}, lastCompletionMessages(t, r.fake), "the current prompt of the assistant replaces the saved one")

	// The prompt is edited on the assistant: the next query of the same
	// conversation carries the new one.
	assistant.M["prompt"] = "Answer as a doctor."
	require.NoError(t, couchdb.UpdateDoc(r.inst, &assistant))
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, chat.ID(), &chat))
	chat.Messages = append(chat.Messages, rag.ChatMessage{ID: "m3", Role: rag.UserRole, Content: "Again", CreatedAt: time.Now()})
	require.NoError(t, couchdb.UpdateDoc(r.inst, &chat))

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	require.Equal(t, []completionMessage{
		{Role: rag.SystemRole, Content: "Answer as a doctor."},
		{Role: rag.UserRole, Content: "Hello"},
		{Role: rag.AssistantRole, Content: "fake answer"},
		{Role: rag.UserRole, Content: "Again"},
	}, lastCompletionMessages(t, r.fake))

	// Without a prompt, no system message is sent at all.
	assistant.M["prompt"] = ""
	require.NoError(t, couchdb.UpdateDoc(r.inst, &assistant))
	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	messages := lastCompletionMessages(t, r.fake)
	require.NotEmpty(t, messages)
	require.Equal(t, rag.UserRole, messages[0].Role)
}

func TestChatDoesNotSaveTheAssistantPrompt(t *testing.T) {
	r := newRAGTest(t)
	assistant := couchdb.JSONDoc{Type: consts.ChatAssistants, M: map[string]interface{}{
		"name":   "Lawyer",
		"prompt": "Answer as a lawyer.",
	}}
	require.NoError(t, couchdb.CreateDoc(r.inst, &assistant))

	chat, err := rag.Chat(r.inst, rag.ChatPayload{
		ChatConversationID: "conversation-of-a-prompted-assistant",
		Query:              "Hello",
		AssistantID:        assistant.ID(),
	})
	require.NoError(t, err)
	require.Len(t, chat.Messages, 1, "the prompt is read from the assistant at query time, not saved")
	require.Equal(t, rag.UserRole, chat.Messages[0].Role)
}
