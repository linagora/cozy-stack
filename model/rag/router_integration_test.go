package rag_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allActions = []string{"create_note", "create_document", "draft_email"}

// newConversation saves a conversation with one user message, and returns
// the query of the rag-query job for it, not streamed like the canned answer
// of the fake openRAG.
func newConversation(t *testing.T, r *ragTest, id, question string, actions []string) rag.QueryMessage {
	t.Helper()
	chat := rag.ChatConversation{
		DocID:        id,
		Messages:     []rag.ChatMessage{{ID: id + "-q", Role: rag.UserRole, Content: question, CreatedAt: time.Now()}},
		CozyMetadata: metadata.New(),
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(r.inst, &chat))
	return rag.QueryMessage{Task: "chat-completion", DocID: id, Actions: actions}
}

// subscribeChatEvents returns a function giving the objects of the chat
// events published so far, up to the `done` or `error` one.
func subscribeChatEvents(t *testing.T, r *ragTest) func() []map[string]interface{} {
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
	query := newConversation(t, r, "conversation-without-actions", "What is the refund policy?", nil)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
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
		return `{"intent": "search", "needs_documents": false}`
	}
	query := newConversation(t, r, "conversation-search", "What is the refund policy?", allActions)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	assert.Equal(t, 1, llmCalls(t, r.fake))
	assert.Equal(t, "route", route.Step())
	assert.Contains(t, route.Prompt(), `"create_document"`)
	assert.Contains(t, route.Prompt(), "Last user message:\nWhat is the refund policy?")
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
		if call.Step() == "route" {
			// The answer has started on openRAG when the router decides.
			<-ragStarted
			return `{"intent": "draft_email", "needs_documents": false}`
		}
		return "```json\n" + `{"to": ["Paul"], "subject": "Point", "body": "Bonjour Paul", "cc": "dropped"}` + "\n```"
	}
	query := newConversation(t, r, "conversation-action", "Écris un mail à Paul pour faire le point", allActions)
	query.Stream = true
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
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

func TestQueryWritesANoteFromTheConversation(t *testing.T) {
	r := newRAGTest(t)
	var writing rag.LLMCall
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "create_note", "needs_documents": false}`
		}
		writing = call
		return "# Déploiement\n\n- Le déploiement passe par la CI."
	}
	query := newConversation(t, r, "conversation-note", "Résume cette conversation dans une note", allActions)
	query.Stream = true
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	published := events()
	assert.Equal(t, []string{"delta", "action", "done"}, objects(published))
	assert.Equal(t, "# Déploiement\n\n- Le déploiement passe par la CI.", deltas(published),
		"the note is the answer, written as it comes")
	assert.Equal(t, "", writing.Step(), "the note is written freely, not in a JSON")
	assert.Contains(t, writing.Prompt(), "write a note")
	assert.Contains(t, writing.Prompt(), "Last user message:\nRésume cette conversation dans une note")

	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "# Déploiement\n\n- Le déploiement passe par la CI.", answer.Content)
	require.NotNil(t, answer.Action)
	assert.Equal(t, "create_note", answer.Action.Name)
	assert.Equal(t, map[string]interface{}{"title": "Déploiement"}, answer.Action.Params)
}

func TestQueryWritesADocumentFromTheDocuments(t *testing.T) {
	r := newRAGTest(t)
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), "write a document") {
			rag.WriteCompletion(w, "# Rapport Atlas\n\n## Avancement\nLe projet avance.")
			return
		}
		rag.WriteCompletion(w, "fake answer")
	}
	r.fake.LLM = func(call rag.LLMCall) string {
		return `{"intent": "create_document", "needs_documents": true}`
	}
	query := newConversation(t, r, "conversation-document", "Écris un rapport sur le projet Atlas à partir de mes fichiers", allActions)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	published := events()
	assert.Equal(t, []string{"delta", "action", "done"}, objects(published))
	assert.NotContains(t, deltas(published), "fake answer", "the plain answer is not given")
	queries := completions(t, r.fake, false)
	assert.Contains(t, queries[len(queries)-1], "from the user's documents only",
		"the document is written by openRAG from the documents")

	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "# Rapport Atlas\n\n## Avancement\nLe projet avance.", answer.Content)
	require.NotNil(t, answer.Action)
	assert.Equal(t, map[string]interface{}{"title": "Rapport Atlas"}, answer.Action.Params)
}

func TestQueryWritesFromTheDocumentsTheyCover(t *testing.T) {
	r := newRAGTest(t)
	var searched string
	r.fake.Search = func(text string) bool {
		searched = text
		return true
	}
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), "write a document") {
			rag.WriteCompletion(w, "# Compte rendu du comité Atlas\n\nLe basculement de Lyon est reporté.")
			return
		}
		rag.WriteCompletion(w, "fake answer")
	}
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			// The router misses that the documents are needed
			return `{"intent": "create_document", "needs_documents": false}`
		}
		t.Errorf("the document is not written without the documents")
		return "# Compte rendu inventé"
	}
	query := newConversation(t, r, "conversation-covered", "Fais un compte rendu Word du comité Atlas", allActions)
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	assert.Equal(t, "Fais un compte rendu Word du comité Atlas", searched)
	for _, req := range r.fake.Rec.All() {
		if strings.HasPrefix(req.Path, "/search/partition/") {
			assert.Equal(t, "/search/partition/"+r.inst.Domain, req.Path)
			assert.Equal(t, "3", req.Query.Get("top_k"))
			assert.Equal(t, "0.6", req.Query.Get("similarity_threshold"))
		}
	}
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "# Compte rendu du comité Atlas\n\nLe basculement de Lyon est reporté.", answer.Content)
	require.NotNil(t, answer.Action)
	assert.Equal(t, "Compte rendu du comité Atlas", answer.Action.Params["title"])
}

func TestQueryUsesTheDocumentsWhenTheSearchFails(t *testing.T) {
	r := newRAGTest(t)
	r.fake.Fail = func(method, path string) int {
		if strings.HasPrefix(path, "/search/") {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		if strings.Contains(string(body), "write a note") {
			rag.WriteCompletion(w, "# Télétravail\n\n- 3 jours par semaine")
			return
		}
		rag.WriteCompletion(w, "fake answer")
	}
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "create_note", "needs_documents": false}`
		}
		t.Errorf("the note is not written without the documents")
		return "# Télétravail inventé"
	}
	query := newConversation(t, r, "conversation-search-fails", "Fais une note sur la politique de télétravail", allActions)
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "# Télétravail\n\n- 3 jours par semaine", answer.Content)
	require.NotNil(t, answer.Action)
}

func TestQueryDoesNotSearchTheDocumentsForAMeeting(t *testing.T) {
	r := newRAGTest(t)
	config.GetConfig().MeetServers = map[string]config.MeetServer{
		config.DefaultInstanceContext: {URL: "https://meet.example.net", ClientID: "app-1", ClientSecret: "s3cr3t"},
	}
	t.Cleanup(func() { config.GetConfig().MeetServers = nil })
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "start_meeting", "needs_documents": false}`
		}
		return `{"title": "Point Atlas", "attendees": []}`
	}
	query := newConversation(t, r, "conversation-meeting", "Lance une visio sur le projet Atlas", append(allActions, "start_meeting"))
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	assert.Equal(t, 0, r.fake.Rec.Count(http.MethodGet, "/search/partition/"+r.inst.Domain))
	answer := lastMessage(t, r, query.DocID)
	require.NotNil(t, answer.Action)
	assert.Equal(t, "start_meeting", answer.Action.Name)
	assert.Equal(t, "Point Atlas", answer.Action.Params["title"])
}

func TestQueryProposesNothingWhenTheDocumentsDoNotCoverTheSubject(t *testing.T) {
	r := newRAGTest(t)
	r.fake.RAG = func(w http.ResponseWriter, req *http.Request) {
		rag.WriteCompletion(w, "Les documents ne couvrent pas le projet Atlas.")
	}
	r.fake.LLM = func(call rag.LLMCall) string {
		return `{"intent": "create_document", "needs_documents": true}`
	}
	query := newConversation(t, r, "conversation-not-covered", "Écris un rapport sur le projet Atlas à partir de mes fichiers", allActions)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "Les documents ne couvrent pas le projet Atlas.", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryProposesAnActionAfterTheAnswer(t *testing.T) {
	r := newRAGTest(t)
	var fill rag.LLMCall
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "draft_email", "needs_documents": true}`
		}
		fill = call
		return `{"to": [], "subject": "Atlas", "body": "fake answer, as an email"}`
	}
	query := newConversation(t, r, "conversation-action-after", "Envoie à l'équipe un mail sur l'avancement du projet Atlas", allActions)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "action", "done"}, objects(events()))
	assert.Equal(t, "draft_email", fill.Step())
	assert.Contains(t, fill.Prompt(), "What the assistant found in the user's documents:\nfake answer")

	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	require.NotNil(t, answer.Action)
	assert.Equal(t, "Atlas", answer.Action.Params["subject"])
}

func TestQueryAnswersWhenTheRouterFails(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string { return "I think it is a note." }
	query := newConversation(t, r, "conversation-router-fails", "Fais-en une note", allActions)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryAnswersWhenTheActionCannotBePrepared(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "draft_email", "needs_documents": false}`
		}
		return `{"to": [], "subject": "", "body": ""}`
	}
	query := newConversation(t, r, "conversation-fill-fails", "Écris un mail à Paul", allActions)
	events := subscribeChatEvents(t, r)

	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))
	assert.Equal(t, []string{"delta", "done"}, objects(events()))
	answer := lastMessage(t, r, query.DocID)
	assert.Equal(t, "fake answer", answer.Content)
	assert.Nil(t, answer.Action)
}

func TestQueryKeepsTheOutcomeOfAnAction(t *testing.T) {
	r := newRAGTest(t)
	r.fake.LLM = func(call rag.LLMCall) string {
		if call.Step() == "route" {
			return `{"intent": "create_note", "needs_documents": false}`
		}
		return "# Courses\n\n- lait"
	}
	query := newConversation(t, r, "conversation-outcome", "Fais-en une note", allActions)
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

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
	require.NoError(t, rag.Query(context.Background(), r.inst, rag.TestingLogger(), query))

	require.NoError(t, couchdb.GetDoc(r.inst, consts.ChatConversations, query.DocID, &chat))
	proposed := chat.Messages[1].Action
	require.NotNil(t, proposed)
	assert.Equal(t, "done", proposed.Status)
	assert.Equal(t, "https://notes.example.net/#/n/123", proposed.URL)
}
