package rag_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testActions are the definitions of the actions of the assistant of the
// Twake apps.
func testActions(t *testing.T) []rag.ActionDefinition {
	t.Helper()
	raw, err := os.ReadFile("testdata/chat_actions.json")
	require.NoError(t, err)
	var actions []rag.ActionDefinition
	require.NoError(t, json.Unmarshal(raw, &actions))
	return actions
}

// newRouterConversation saves a conversation with one user message, and returns
// the query of the rag-query job for it, not streamed like the canned answer
// of the fake openRAG.
func newRouterConversation(t *testing.T, r *ragTest, id, question string, actions []rag.ActionDefinition) rag.QueryMessage {
	t.Helper()
	chat := rag.ChatConversation{
		DocID:        id,
		Messages:     []rag.ChatMessage{{ID: id + "-q", Role: rag.UserRole, Content: question, CreatedAt: time.Now()}},
		CozyMetadata: metadata.New(),
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(r.inst, &chat))
	return rag.QueryMessage{Task: "chat-completion", DocID: id, Actions: actions}
}

// subscribeRouterEvents returns a function giving the objects of the chat
// events published so far, up to the `done` or `error` one.
func subscribeRouterEvents(t *testing.T, r *ragTest) func() []map[string]interface{} {
	t.Helper()
	sub := realtime.GetHub().Subscriber(r.inst)
	sub.Subscribe(consts.ChatEvents)
	t.Cleanup(sub.Close)
	return func() []map[string]interface{} {
		var events []map[string]interface{}
		for {
			select {
			case e := <-sub.Channel:
				doc := e.Doc.(*couchdb.JSONDoc)
				events = append(events, doc.M)
				if doc.M["object"] == "done" || doc.M["object"] == "error" {
					return events
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("no done event, got %v", events)
			}
		}
	}
}

func objects(events []map[string]interface{}) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i], _ = e["object"].(string)
	}
	return out
}

func lastMessage(t *testing.T, r *ragTest, id string) rag.ChatMessage {
	t.Helper()
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, id, &chat))
	return chat.Messages[len(chat.Messages)-1]
}

// completions returns the bodies of the completions sent to openRAG: the
// direct calls to the LLM of the chat router, without a model, or the
// queries for an answer from the documents.
func completions(t *testing.T, fake *rag.FakeOpenRAG, direct bool) []string {
	t.Helper()
	var bodies []string
	for _, req := range fake.Rec.All() {
		if req.Path != "/v1/chat/completions" {
			continue
		}
		var payload struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(req.Body, &payload))
		if (payload.Model == "") == direct {
			bodies = append(bodies, string(req.Body))
		}
	}
	return bodies
}

// fills returns the bodies of the queries to openRAG that fill the params of
// an action with the documents. The answer started with the router is not
// one of them: it may be cancelled before it reaches openRAG.
func fills(t *testing.T, fake *rag.FakeOpenRAG) []string {
	t.Helper()
	var bodies []string
	for _, body := range completions(t, fake, false) {
		if strings.Contains(body, `"response_format"`) {
			bodies = append(bodies, body)
		}
	}
	return bodies
}

func llmCalls(t *testing.T, fake *rag.FakeOpenRAG) int {
	t.Helper()
	return len(completions(t, fake, true))
}

// deltas joins the content of the delta events.
func deltas(events []map[string]interface{}) string {
	var b strings.Builder
	for _, e := range events {
		if e["object"] == "delta" {
			b.WriteString(e["content"].(string))
		}
	}
	return b.String()
}

func TestQueryWithoutActionsSkipsTheRouter(t *testing.T) {
	r := newRAGTest(t)
	query := newRouterConversation(t, r, "conversation-without-actions", "What is the refund policy?", nil)
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Equal(t, 0, llmCalls(t, r.fake))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryAnswersASearch(t *testing.T) {
	r := newRAGTest(t)
	var route rag.LLMCall
	r.fake.LLM = func(call rag.LLMCall) string {
		route = call
		return `{"intent": "search"}`
	}
	query := newRouterConversation(t, r, "conversation-search", "What is the refund policy?", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Equal(t, 1, llmCalls(t, r.fake))
	assert.Equal(t, "route", route.Step())
	assert.Equal(t, "", route.Model, "the router asks the LLM alone")
	assert.Contains(t, route.Prompt(), `"create_document"`)
	assert.Contains(t, route.Prompt(), `Last user message: "What is the refund policy?"`)
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryProposesAnActionInsteadOfTheAnswer(t *testing.T) {
	r := newRAGTest(t)
	ragStarted := make(chan struct{})
	disconnected := make(chan struct{})
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "Hello"}}], "extra": {}}`+"\n\n")
		w.(http.Flusher).Flush()
		close(ragStarted)
		<-req.Context().Done()
		close(disconnected)
	}
	r.fake.LLM = func(call rag.LLMCall) string {
		// The answer has started on openRAG when the router decides.
		<-ragStarted
		return `{"intent": "draft_email"}`
	}
	r.fake.Fill = func(call rag.LLMCall) string {
		return "```json\n" + `{"to": ["Paul"], "subject": "Point", "body": "Bonjour Paul", "cc": "dropped"}` + "\n```"
	}
	query := newRouterConversation(t, r, "conversation-action", "Écris un mail à Paul pour faire le point", testActions(t))
	query.Stream = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("the answer was not cancelled on openRAG")
	}

	published := events()
	require.Equal(t, []string{"action", "done"}, objects(published), "nothing of the cancelled answer is published")
	action := published[0]["action"].(*rag.ChatAction)
	assert.Equal(t, "draft_email", action.Name)
	expected := map[string]interface{}{"to": []string{"Paul"}, "subject": "Point", "body": "Bonjour Paul"}
	assert.Equal(t, expected, action.Params)

	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, answer.ID, published[0]["message_id"], "the client knows where to write the outcome")
	assert.Equal(t, rag.AssistantRole, answer.Role)
	assert.Equal(t, "", answer.Content)
	require.NotNil(t, answer.Action)
	assert.Equal(t, "draft_email", answer.Action.Name)
	assert.Equal(t, []interface{}{"Paul"}, answer.Action.Params["to"])
}

func TestQueryProposesAnActionWithTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "draft_email"}` }
	var fill rag.LLMCall
	r.fake.Fill = func(call rag.LLMCall) string {
		fill = call
		return `{"to": [], "subject": "Atlas", "body": "The decisions of the last committee, as an email"}`
	}
	query := newRouterConversation(t, r, "conversation-action-documents", "Rédige un mail à l'équipe Atlas avec les décisions du dernier comité", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"action", "done"}, objects(events()))
	assert.Equal(t, "ragondin-"+r.inst.Domain, fill.Model, "openRAG retrieves the documents for the params")
	assert.Equal(t, "draft_email", fill.Step())
	require.NotEmpty(t, fill.Messages)
	assert.Equal(t, "system", fill.Messages[0].Role, "the fill instructions lead the conversation, as the prompt of an assistant")
	assert.Contains(t, fill.Messages[0].Content, "and the user's documents given as context")
	assert.Equal(t, "Rédige un mail à l'équipe Atlas avec les décisions du dernier comité", fill.Messages[len(fill.Messages)-1].Content)
	assert.Equal(t, 1, llmCalls(t, r.fake), "the LLM alone is only asked to route")
	assert.Len(t, fills(t, r.fake), 1, "the params, once")
	answer := lastMessage(t, r, query.DocID)
	require.NotNil(t, answer.Action)
	assert.Equal(t, "Atlas", answer.Action.Params["subject"])
}

func TestQueryProposesAnActionOfTheApp(t *testing.T) {
	r := newRAGTest(t)
	var route, fill rag.LLMCall
	r.fake.LLM = func(call rag.LLMCall) string {
		route = call
		return `{"intent": "create_task"}`
	}
	r.fake.Fill = func(call rag.LLMCall) string {
		fill = call
		return `{"title": "Relancer Acme", "due": "2026-10-03", "assignee": "Marc"}`
	}
	task := rag.ActionDefinition{
		Name:        "create_task",
		Description: "create a task in the user's task list",
		Parameters: &rag.ParametersSchema{Type: "object", Properties: map[string]rag.ParamSchema{
			"title":    {Type: "string", Description: "what to do"},
			"due":      {Type: "string", Description: "the due date, as YYYY-MM-DD"},
			"assignee": {Type: "string", Description: "who does it", UserWritten: true},
		}, Required: []string{"title"}},
		Instructions: "The title starts with a verb.",
	}
	require.NoError(t, rag.ValidateActions([]rag.ActionDefinition{task}))
	query := newRouterConversation(t, r, "conversation-task", "Ajoute une tâche pour relancer Acme demain", []rag.ActionDefinition{task})
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	published := events()
	require.Equal(t, []string{"action", "done"}, objects(published))
	assert.Contains(t, route.Prompt(), `- "create_task": create a task in the user's task list`)
	assert.Equal(t, "create_task", fill.Step())
	assert.Contains(t, fill.Prompt(), "The title starts with a verb.")
	assert.Contains(t, fill.Prompt(), `- "due" (string): the due date, as YYYY-MM-DD`)
	action := published[0]["action"].(*rag.ChatAction)
	assert.Equal(t, map[string]interface{}{"title": "Relancer Acme", "due": "2026-10-03", "assignee": ""}, action.Params,
		"the assignee the user did not write is dropped")
}

func TestQueryProposesAnActionWithoutTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	var fill rag.LLMCall
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "draft_email"}`
		}
		fill = call
		return `{"to": ["Paul"], "subject": "Point", "body": "Bonjour Paul"}`
	}
	r.fake.Fill = func(call rag.LLMCall) string {
		t.Error("the documents are not used")
		return "{}"
	}
	query := newRouterConversation(t, r, "conversation-direct-params", "Écris un mail à Paul pour faire le point", testActions(t))
	query.DirectLLM = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"action", "done"}, objects(events()))
	assert.Equal(t, "draft_email", fill.Step())
	assert.Equal(t, "", fill.Model, "the LLM alone fills the params")
	assert.Contains(t, fill.Prompt(), "The messages of the conversation are quoted")
	assert.Contains(t, fill.Prompt(), `Last user message: "Écris un mail à Paul pour faire le point"`)
	assert.Empty(t, completions(t, r.fake, false), "openRAG is not asked for an answer from the documents")
	answer := lastMessage(t, r, query.DocID)
	require.NotNil(t, answer.Action)
	assert.Equal(t, []interface{}{"Paul"}, answer.Action.Params["to"])
}

func TestQueryAnswersWhenTheRouterFails(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return "I think it is a note." }
	query := newRouterConversation(t, r, "conversation-router-fails", "Fais-en une note", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryAnswersWhenTheActionCannotBePrepared(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "draft_email"}` }
	r.fake.Fill = func(call rag.LLMCall) string { return `{"to": [], "subject": "", "body": ""}` }
	query := newRouterConversation(t, r, "conversation-fill-fails", "Écris un mail à Paul", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Len(t, fills(t, r.fake), 1)
	queries := completions(t, r.fake, false)
	assert.NotContains(t, queries[len(queries)-1], `"response_format"`, "the answer is asked again after the params")
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryKeepsTheOutcomeOfAnAction(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "create_note"}` }
	r.fake.Fill = func(call rag.LLMCall) string { return `{"title": "Courses", "content": "- lait"}` }
	query := newRouterConversation(t, r, "conversation-outcome", "Fais-en une note", testActions(t))
	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))

	// The client writes the outcome on the message, as a plain document.
	var doc couchdb.JSONDoc
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &doc))
	messages := doc.M["messages"].([]interface{})
	action := messages[len(messages)-1].(map[string]interface{})["action"].(map[string]interface{})
	action["status"] = "done"
	action["url"] = "https://notes.example.net/#/n/123"
	doc.Type = consts.ChatConversations
	require.NoError(t, couchdb.UpdateDoc(r.inst, &doc))

	// The next message of the conversation goes through the stack.
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	chat.Messages = append(chat.Messages, rag.ChatMessage{ID: "next", Role: rag.UserRole, Content: "Merci", CreatedAt: time.Now()})
	require.NoError(t, couchdb.UpdateDoc(r.inst, &chat))
	r.fake.LLM = nil
	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))

	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	proposed := chat.Messages[1].Action
	require.NotNil(t, proposed)
	assert.Equal(t, "done", proposed.Status)
	assert.Equal(t, "https://notes.example.net/#/n/123", proposed.URL)
}

func TestQueryFillsTheParamsFromTheAttachments(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "draft_email"}` }
	var fill rag.LLMCall
	r.fake.Fill = func(call rag.LLMCall) string {
		fill = call
		return `{"to": [], "subject": "Compte rendu", "body": "Le document dit..."}`
	}
	query := newRouterConversation(t, r, "conversation-attachment-params", "Fais un mail avec le compte rendu de ce document", testActions(t))
	query.AttachmentIDs = []string{"file-1"}
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"action", "done"}, objects(events()))
	assert.Contains(t, string(fill.Metadata), `"attachments":[{"id":"file-1"}]`, "the attachments go with the params")
	assert.Equal(t, "Compte rendu", lastMessage(t, r, query.DocID).Action.Params["subject"])
}

func TestQuerySavesTheAnswerOnTheConversationChangedMeanwhile(t *testing.T) {
	r := newRAGTest(t)
	id := "conversation-changed"
	chat := rag.ChatConversation{
		DocID: id,
		Messages: []rag.ChatMessage{
			{ID: "q1", Role: rag.UserRole, Content: "Fais-en une note", CreatedAt: time.Now()},
			{ID: "a1", Role: rag.AssistantRole, Content: "# Courses", CreatedAt: time.Now(),
				Action: &rag.ChatAction{Name: "create_note", Params: map[string]interface{}{"title": "Courses"}}},
			{ID: "q2", Role: rag.UserRole, Content: "Merci, et la politique de télétravail ?", CreatedAt: time.Now()},
		},
		CozyMetadata: metadata.New(),
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(r.inst, &chat))
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		// The client writes the outcome of the note while openRAG answers
		assert.NoError(t, writeOutcome(r.inst, id))
		rag.WriteCompletion(w, "fake answer")
	}
	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), rag.QueryMessage{Task: "chat-completion", DocID: id}))

	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, id, &chat))
	require.Len(t, chat.Messages, 4)
	require.NotNil(t, chat.Messages[1].Action)
	assert.Equal(t, "done", chat.Messages[1].Action.Status, "the outcome written meanwhile is kept")
	assert.Equal(t, rag.AssistantRole, chat.Messages[3].Role)
	assert.Equal(t, "fake answer", chat.Messages[3].Content)
}

// newConversationWithAction saves a conversation where a note was proposed,
// then a new question asked, and returns the query answering it.
func newConversationWithAction(t *testing.T, r *ragTest, id string, actions []rag.ActionDefinition) rag.QueryMessage {
	t.Helper()
	chat := rag.ChatConversation{
		DocID: id,
		Messages: []rag.ChatMessage{
			{ID: id + "-q1", Role: rag.UserRole, Content: "Fais-en une note", CreatedAt: time.Now()},
			{ID: id + "-a1", Role: rag.AssistantRole, Content: "# Courses", CreatedAt: time.Now(),
				Action: &rag.ChatAction{Name: "create_note", Params: map[string]interface{}{"title": "Courses"}}},
			{ID: id + "-q2", Role: rag.UserRole, Content: "Merci, et la politique de télétravail ?", CreatedAt: time.Now()},
		},
		CozyMetadata: metadata.New(),
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(r.inst, &chat))
	return rag.QueryMessage{Task: "chat-completion", DocID: id, Actions: actions}
}

// writeOutcome writes the outcome of the note of the conversation, as the
// client does: on the whole document, read again.
func writeOutcome(inst *instance.Instance, id string) error {
	var doc couchdb.JSONDoc
	if err := couchdb.GetDoc(inst, consts.ChatConversations, id, &doc); err != nil {
		return err
	}
	messages := doc.M["messages"].([]interface{})
	action := messages[1].(map[string]interface{})["action"].(map[string]interface{})
	action["status"] = "done"
	action["url"] = "https://notes.example.net/#/n/123"
	doc.Type = consts.ChatConversations
	return couchdb.UpdateDoc(inst, &doc)
}

func TestQueryRetriesTheSaveOnConflict(t *testing.T) {
	r := newRAGTest(t)
	query := newConversationWithAction(t, r, "conversation-conflict", nil)
	writes := 0
	t.Cleanup(rag.SetUpdateConversationForTest(func(inst *instance.Instance, chat *rag.ChatConversation) error {
		writes++
		if writes == 1 {
			// The client writes the outcome between the read and the write
			require.NoError(t, writeOutcome(inst, chat.DocID))
		}
		return couchdb.UpdateDoc(inst, chat)
	}))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Equal(t, 2, writes, "the first write conflicts")
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	require.Len(t, chat.Messages, 4)
	require.NotNil(t, chat.Messages[1].Action)
	assert.Equal(t, "done", chat.Messages[1].Action.Status, "the outcome is kept")
	assert.Equal(t, "fake answer", chat.Messages[3].Content, "the answer is saved")
}

func TestQueryGivesUpTheSaveAfterThreeConflicts(t *testing.T) {
	r := newRAGTest(t)
	query := newConversationWithAction(t, r, "conversation-conflicts", nil)
	writes := 0
	t.Cleanup(rag.SetUpdateConversationForTest(func(inst *instance.Instance, chat *rag.ChatConversation) error {
		writes++
		require.NoError(t, writeOutcome(inst, chat.DocID))
		return couchdb.UpdateDoc(inst, chat)
	}))
	events := subscribeRouterEvents(t, r)

	err := rag.Query(r.inst, rag.TestingLogger(), query)
	assert.True(t, couchdb.IsConflictError(err), "got %v", err)
	assert.Equal(t, 3, writes)
	assert.Equal(t, []string{"delta", "done"}, objects(events()), "the client has the answer, which is not saved")
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	assert.Len(t, chat.Messages, 3)
}

func TestQueryHoldsBackAStreamedSearchUntilTheRouterDecides(t *testing.T) {
	r := newRAGTest(t)
	ragStarted := make(chan struct{})
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"role": "assistant", "content": ""}}], "extra": {}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "Hel"}}], "extra": {}}`+"\n\n")
		w.(http.Flusher).Flush()
		close(ragStarted)
		_, _ = io.WriteString(w, `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "lo"}}], "extra": {}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"object": "chat.completion.chunk", "choices": [{"delta": {}, "finish_reason": "stop"}], "extra": {}}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	r.fake.LLM = func(call rag.LLMCall) string {
		// The first token of the answer is there when the router decides
		<-ragStarted
		return `{"intent": "search"}`
	}
	query := newRouterConversation(t, r, "conversation-held-back", "What is the refund policy?", testActions(t))
	query.Stream = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	published := events()
	assert.Equal(t, []string{"delta", "delta", "done"}, objects(published))
	assert.Equal(t, "Hello", deltas(published), "nothing of the answer is lost")
	assert.Equal(t, 0, published[0]["position"], "the empty delta opening the stream takes no position")
	assert.Equal(t, "Hello", lastMessage(t, r, query.DocID).Content)
}

func TestQueryAnswersWhenTheRouterTimesOut(t *testing.T) {
	r := newRAGTest(t)
	t.Cleanup(rag.SetRouterTimeoutForTest(100 * time.Millisecond))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() != "route" {
			t.Errorf("no call to the LLM but the router, got %q", call.Step())
		}
		<-release
		return `{"intent": "create_note"}`
	}
	query := newRouterConversation(t, r, "conversation-router-timeout", "Fais-en une note", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryAnswersWhenTheIntentIsUnknown(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "delete_everything"}` }
	query := newRouterConversation(t, r, "conversation-unknown-intent", "Fais-en une note", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Equal(t, 1, llmCalls(t, r.fake), "nothing is prepared")
	assert.Nil(t, lastMessage(t, r, query.DocID).Action)
}

func TestQueryFailsWhenTheAnswerFails(t *testing.T) {
	r := newRAGTest(t)
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusBadGateway) }
	query := newRouterConversation(t, r, "conversation-answer-fails", "What is the refund policy?", testActions(t))
	events := subscribeRouterEvents(t, r)

	assert.EqualError(t, rag.Query(r.inst, rag.TestingLogger(), query), "POST status code: 502")
	published := events()
	require.Equal(t, []string{"error"}, objects(published))
	assert.Equal(t, "POST status code: 502", published[0]["message"])
	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	assert.Len(t, chat.Messages, 1, "no answer is saved")
}

func TestQueryAnswersASearchWithoutTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "search"}` }
	query := newRouterConversation(t, r, "conversation-direct-search", "Traduis en anglais : bonjour", testActions(t))
	query.DirectLLM = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Empty(t, completions(t, r.fake, false), "openRAG is not asked for the documents")
	assert.Equal(t, 2, llmCalls(t, r.fake), "the router, then the answer by the LLM alone")
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryAnswersWhenTheParamsCannotBeFilledWithoutTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "draft_email"}`
		}
		return "I would rather not."
	}
	query := newRouterConversation(t, r, "conversation-direct-fill-fails", "Écris un mail à Paul", testActions(t))
	query.DirectLLM = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Empty(t, completions(t, r.fake, false))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryCreatesThePartitionForTheParams(t *testing.T) {
	r := newRAGTest(t)
	r.fake.NoPartition = true
	r.fake.LLM = func(call rag.LLMCall) string { return `{"intent": "draft_email"}` }
	r.fake.Fill = func(call rag.LLMCall) string { return `{"to": ["Paul"], "subject": "Point", "body": "Bonjour Paul"}` }
	query := newRouterConversation(t, r, "conversation-no-partition", "Écris un mail à Paul pour faire le point", testActions(t))
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"action", "done"}, objects(events()))
	created := false
	for _, req := range r.fake.Rec.All() {
		if req.Method == http.MethodPost && req.Path == "/partition/"+r.inst.Domain {
			created = true
		}
	}
	assert.True(t, created, "the partition of the instance is created on openRAG")
	assert.Equal(t, "Point", lastMessage(t, r, query.DocID).Action.Params["subject"])
}

func TestQueryProposesAnActionWithoutParams(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() != "route" {
			t.Errorf("nothing to fill, got %q", call.Step())
		}
		return `{"intent": "start_visio"}`
	}
	r.fake.Fill = func(call rag.LLMCall) string {
		t.Error("nothing to fill")
		return "{}"
	}
	visio := rag.ActionDefinition{
		Name:        "start_visio",
		Description: "start a video call",
		Parameters:  &rag.ParametersSchema{Type: "object"},
	}
	require.NoError(t, rag.ValidateActions([]rag.ActionDefinition{visio}))
	query := newRouterConversation(t, r, "conversation-visio", "Lance une visio", []rag.ActionDefinition{visio})
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	published := events()
	require.Equal(t, []string{"action", "done"}, objects(published))
	assert.Equal(t, &rag.ChatAction{Name: "start_visio", Params: map[string]interface{}{}}, published[0]["action"])
	assert.Equal(t, 1, llmCalls(t, r.fake), "the router only")
	assert.Empty(t, fills(t, r.fake), "nothing to fill")
}

func TestQueryProposesANoteWithTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() != "route" {
			t.Errorf("the note is made by openRAG, not by the LLM alone (got %q)", call.Step())
		}
		return `{"intent": "create_note"}`
	}
	var fill rag.LLMCall
	r.fake.Fill = func(call rag.LLMCall) string {
		fill = call
		return `{"title": "Politique de télétravail", "content": "## Principes\n- 3 jours par semaine"}`
	}
	r.fake.FillSources = []map[string]interface{}{
		{"source_type": "document", "chunk": map[string]interface{}{"file_id": "file-1", "filename": "teletravail.pdf"}},
	}
	query := newRouterConversation(t, r, "conversation-note", "Fais une note avec les points à retenir de la politique de télétravail", testActions(t))
	query.Stream = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	published := events()
	assert.Equal(t, []string{"sources", "action", "done"}, objects(published), "the sources of the documents, then the action")
	assert.Equal(t, "create_note", fill.Step())
	assert.Equal(t, "ragondin-"+r.inst.Domain, fill.Model)
	assert.Contains(t, fill.Prompt(), `- "content" (string, required): the note itself, in full, in Markdown, without its title`)
	assert.Contains(t, fill.Prompt(), "never with the title, which is in its own field", "the instructions of the action")
	assert.NotContains(t, string(fill.ResponseFormat.JSONSchema.Schema), "facts_missing", "with the documents, the LLM fills the params")

	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "", answer.Content, "the note is in the params, not in the chat")
	require.Len(t, answer.Sources, 1)
	assert.Equal(t, "teletravail.pdf", answer.Sources[0].Filename)
	require.NotNil(t, answer.Action)
	assert.Equal(t, map[string]interface{}{"title": "Politique de télétravail", "content": "## Principes\n- 3 jours par semaine"}, answer.Action.Params)
}

func TestQueryProposesANoteWithoutTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	var fill rag.LLMCall
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "create_note"}`
		}
		fill = call
		return `{"facts_missing": false, "title": "Déploiement", "content": "- Le déploiement passe par la CI."}`
	}
	r.fake.Fill = func(call rag.LLMCall) string {
		t.Error("the documents are not used")
		return "{}"
	}
	query := newRouterConversation(t, r, "conversation-note-direct", "Résume cette conversation dans une note", testActions(t))
	query.DirectLLM = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"action", "done"}, objects(events()))
	assert.Equal(t, "create_note", fill.Step())
	assert.Equal(t, "", fill.Model, "by the LLM alone")
	assert.Contains(t, fill.Prompt(), `Last user message: "Résume cette conversation dans une note"`)
	assert.Empty(t, completions(t, r.fake, false), "openRAG is not asked for an answer from the documents")
	answer := lastMessage(t, r, query.DocID)
	require.NotNil(t, answer.Action)
	assert.Equal(t, map[string]interface{}{"title": "Déploiement", "content": "- Le déploiement passe par la CI."}, answer.Action.Params,
		"facts_missing is not a param")
	assert.Nil(t, answer.Sources)
	assert.Contains(t, string(fill.ResponseFormat.JSONSchema.Schema), `"facts_missing":{"type":"boolean"}`)
	assert.Contains(t, string(fill.ResponseFormat.JSONSchema.Schema), `"required":["facts_missing","content","title"]`)
}

func TestQueryAnswersWhenTheFactsAreMissing(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "create_note"}`
		}
		return `{"facts_missing": true, "title": "Décisions du comité Atlas", "content": "- **Projet X** : validation du budget"}`
	}
	query := newRouterConversation(t, r, "conversation-facts-missing", "Fais une note avec les décisions du comité Atlas", testActions(t))
	query.DirectLLM = true
	events := subscribeRouterEvents(t, r)

	require.NoError(t, rag.Query(r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()), "no action made of invented facts, the answer instead")
	assert.Empty(t, completions(t, r.fake, false), "still without the documents")
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}
