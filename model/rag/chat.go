package rag

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/gofrs/uuid/v5"
	"github.com/labstack/echo/v4"
)

type ChatPayload struct {
	ChatConversationID string
	Query              string   `json:"q"`
	Stream             *bool    `json:"stream"`
	WebSearch          *bool    `json:"websearch"`
	AssistantID        string   `json:"assistantID,omitempty"`
	AttachmentIDs      []string `json:"attachmentIDs,omitempty"`
	// Documents, when false, asks for an answer of the LLM alone, without
	// the user's documents.
	Documents *bool `json:"documents,omitempty"`
	// Instructions tell the LLM how to answer in the client, e.g. that the
	// answer is put in a document as it is: a system message of the answer,
	// like the instructions of the OpenAI Responses API.
	Instructions string `json:"instructions,omitempty"`
	// Actions are the definitions of the chat actions the client can run.
	Actions []ActionDefinition `json:"actions,omitempty"`
}

// directLLM tells whether the client asks for an answer without the documents.
func (p ChatPayload) directLLM() bool {
	return p.Documents != nil && !*p.Documents
}

// maxInstructionsChars bounds the instructions of a client: they come with
// every answer, before the conversation.
const maxInstructionsChars = 2000

// Validate checks the options of a chat message: those that cannot go
// together, and the size of the instructions.
func (p ChatPayload) Validate() error {
	if p.directLLM() && len(p.AttachmentIDs) > 0 {
		return errors.New("attachmentIDs cannot be used without the documents")
	}
	if n := utf8.RuneCountInString(p.Instructions); n > maxInstructionsChars {
		return fmt.Errorf("instructions too long: %d characters, at most %d", n, maxInstructionsChars)
	}
	return ValidateActions(p.Actions)
}

type ChatConversation struct {
	DocID        string                  `json:"_id"`
	DocRev       string                  `json:"_rev,omitempty"`
	Messages     []ChatMessage           `json:"messages"`
	CozyMetadata *metadata.CozyMetadata  `json:"cozyMetadata"`
	Rels         jsonapi.RelationshipMap `json:"relationships,omitempty"`
}

type ChatMessage struct {
	ID            string      `json:"id"`
	Role          string      `json:"role"`
	Content       string      `json:"content"`
	Sources       []Source    `json:"sources,omitempty"`
	AttachmentIDs []string    `json:"attachmentIDs,omitempty"`
	Action        *ChatAction `json:"action,omitempty"`
	CreatedAt     time.Time   `json:"createdAt"`
}

const (
	UserRole      = "user"
	AssistantRole = "assistant"
	SystemRole    = "system"
	Temperature   = 0.3   // LLM parameter - Sampling temperature, lower is more deterministic, higher is more creative.
	TopP          = 1     // LLM parameter - Alternative to temperature, take the tokens with the top p probability.
	LogProbs      = false // LLM parameter - Whether to return log probabilities of the output tokens.
)

// DocTypeVersion represents the doctype version. Each time this document
// structure is modified, update this value
const DocTypeVersion = "1"

func (c *ChatConversation) ID() string        { return c.DocID }
func (c *ChatConversation) Rev() string       { return c.DocRev }
func (c *ChatConversation) DocType() string   { return consts.ChatConversations }
func (c *ChatConversation) SetID(id string)   { c.DocID = id }
func (c *ChatConversation) SetRev(rev string) { c.DocRev = rev }
func (c *ChatConversation) Clone() couchdb.Doc {
	cloned := *c
	cloned.Messages = make([]ChatMessage, len(c.Messages))
	copy(cloned.Messages, c.Messages)
	cloned.Rels = c.Rels.Clone()
	return &cloned
}
func (c *ChatConversation) Included() []jsonapi.Object             { return nil }
func (c *ChatConversation) Relationships() jsonapi.RelationshipMap { return c.Rels }
func (c *ChatConversation) Links() *jsonapi.LinksList              { return nil }

var _ jsonapi.Object = (*ChatConversation)(nil)

type QueryMessage struct {
	Task          string             `json:"task"`
	DocID         string             `json:"doc_id"`
	Stream        bool               `json:"stream"`
	WebSearch     bool               `json:"websearch"`
	AttachmentIDs []string           `json:"attachmentIDs,omitempty"`
	Actions       []ActionDefinition `json:"actions,omitempty"`
	// DirectLLM is the answer of the LLM alone, without the user's documents.
	DirectLLM bool `json:"directLLM,omitempty"`
	// Instructions are those of the client on how to answer.
	Instructions string `json:"instructions,omitempty"`
}

type Source struct {
	SourceType string `json:"sourceType"`
	// Document source fields
	ID             string `json:"id,omitempty"`
	DocType        string `json:"doctype,omitempty"`
	Filename       string `json:"filename,omitempty"`
	FileURL        string `json:"fileUrl,omitempty"`
	ChunkURL       string `json:"chunkUrl,omitempty"`
	Page           int    `json:"page,omitempty"`
	EmailPreview   string `json:"email.preview,omitempty"`
	RelationshipID string `json:"relationship_id,omitempty"`
	ParentID       string `json:"parent_id,omitempty"`
	Subject        string `json:"email.subject,omitempty"`
	Datetime       string `json:"datetime,omitempty"`
	// Web source fields
	URL     string `json:"url,omitempty"`
	Title   string `json:"title,omitempty"`
	Snippet string `json:"snippet,omitempty"`
}

type chatAssistant struct {
	DocID         string                 `json:"_id,omitempty"`
	DocRev        string                 `json:"_rev,omitempty"`
	Relationships chatAssistantRelations `json:"relationships,omitempty"`
	KnowledgeBase []knowledgeBaseEntry   `json:"knowledgeBase,omitempty"`
	Prompt        string                 `json:"prompt,omitempty"`
}

type knowledgeBaseEntry struct {
	Doctype string `json:"doctype"`
	DirID   string `json:"dirId"`
}

type chatAssistantRelations struct {
	Provider struct {
		Data struct {
			// For LLM accounts, the assistant app uses the "relationships"
			// (not "referenced_by") format, so the fields are "_id"/"_type".
			ID       string `json:"_id"`
			Type     string `json:"_type"`
			Metadata struct {
				ProviderID string `json:"providerId"`
			} `json:"metadata"`
		} `json:"data"`
	} `json:"provider"`
}

func (a *chatAssistant) ID() string         { return a.DocID }
func (a *chatAssistant) Rev() string        { return a.DocRev }
func (a *chatAssistant) DocType() string    { return consts.ChatAssistants }
func (a *chatAssistant) SetID(id string)    { a.DocID = id }
func (a *chatAssistant) SetRev(rev string)  { a.DocRev = rev }
func (a *chatAssistant) Clone() couchdb.Doc { c := *a; return &c }

// knowledgeBaseDirID returns the Drive folder scoping the assistant's
// retrieval, or "" when the assistant has no knowledge base. Only a single
// folder per assistant is supported: extra io.cozy.files entries are ignored,
// with a warning so the truncation is at least visible in the logs.
func (a *chatAssistant) knowledgeBaseDirID(logger logger.Logger) string {
	if a == nil {
		return ""
	}
	dirID := ""
	for _, entry := range a.KnowledgeBase {
		if entry.Doctype != consts.Files || entry.DirID == "" {
			continue
		}
		if dirID == "" {
			dirID = entry.DirID
		} else if entry.DirID != dirID {
			logger.Warnf("assistant %s: multiple knowledge base folders are not supported, ignoring %s", a.DocID, entry.DirID)
		}
	}
	return dirID
}

// ErrAssistantNotFound is returned when a conversation references an
// assistant that is gone (deleted, or never created). The query must fail:
// answering anyway would silently widen a possibly folder-scoped
// conversation to whole-instance retrieval.
var ErrAssistantNotFound = errors.New("assistant not found")

func Chat(inst *instance.Instance, payload ChatPayload) (*ChatConversation, error) {
	var chat ChatConversation
	err := couchdb.GetDoc(inst, consts.ChatConversations, payload.ChatConversationID, &chat)
	if couchdb.IsNotFoundError(err) {
		chat.DocID = payload.ChatConversationID
		md := metadata.New()
		md.DocTypeVersion = DocTypeVersion
		md.UpdatedAt = md.CreatedAt
		chat.CozyMetadata = md
		if payload.AssistantID != "" {
			chat.Rels = jsonapi.RelationshipMap{
				"assistant": jsonapi.Relationship{
					Data: struct {
						ID   string `json:"_id"`
						Type string `json:"_type"`
					}{
						ID:   payload.AssistantID,
						Type: consts.ChatAssistants,
					},
				},
			}
		}
	} else if err != nil {
		return nil, err
	} else {
		chat.CozyMetadata.UpdatedAt = time.Now().UTC()
	}
	uuidv7, _ := uuid.NewV7()
	msg := ChatMessage{
		ID:            uuidv7.String(),
		Role:          UserRole,
		Content:       payload.Query,
		AttachmentIDs: payload.AttachmentIDs,
		CreatedAt:     time.Now().UTC(),
	}
	chat.Messages = append(chat.Messages, msg)
	if chat.DocRev == "" {
		err = couchdb.CreateNamedDocWithDB(inst, &chat)
	} else {
		err = couchdb.UpdateDoc(inst, &chat)
	}
	if err != nil {
		return nil, err
	}
	stream := true
	if payload.Stream != nil {
		stream = *payload.Stream
	}
	websearch := false
	if payload.WebSearch != nil {
		websearch = *payload.WebSearch
	}
	query, err := job.NewMessage(&QueryMessage{
		Task:          "chat-completion",
		DocID:         chat.DocID,
		Stream:        stream,
		WebSearch:     websearch,
		AttachmentIDs: payload.AttachmentIDs,
		Actions:       payload.Actions,
		DirectLLM:     payload.directLLM(),
		Instructions:  strings.TrimSpace(payload.Instructions),
	})
	if err != nil {
		return nil, err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "rag-query",
		Message:    query,
	})
	if err != nil {
		return nil, err
	}
	return &chat, nil
}

// decodeExtra returns the extra payload of an openRAG response. It is a JSON
// object, or a JSON-encoded string for openRAG <= v2.2.0 (legacy).
func decodeExtra(raw interface{}) (map[string]interface{}, error) {
	switch extra := raw.(type) {
	case map[string]interface{}:
		return extra, nil
	case string:
		if extra == "" {
			return nil, nil
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal([]byte(extra), &decoded); err != nil {
			return nil, err
		}
		return decoded, nil
	}
	return nil, nil
}

// chunkField reads a field of the chunk metadata of a document source. It is
// nested under "chunk", or flat in the source for openRAG <= v2.2.0 (legacy).
func chunkField(src map[string]interface{}, key string) interface{} {
	if chunk, ok := src["chunk"].(map[string]interface{}); ok {
		if v, ok := chunk[key]; ok {
			return v
		}
	}
	return src[key]
}

func getSources(event map[string]interface{}) ([]Source, error) {
	extra, err := decodeExtra(event["extra"])
	if err != nil || extra == nil {
		return nil, err
	}
	sourcesRaw, ok := extra["sources"].([]interface{})
	if !ok {
		return nil, nil
	}
	var sources []Source

	for _, s := range sourcesRaw {
		src, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		sourceType, _ := src["source_type"].(string)
		if sourceType == "web" {
			urlStr, _ := src["url"].(string)
			title, _ := src["title"].(string)
			snippet, _ := src["snippet"].(string)
			sources = append(sources, Source{
				SourceType: "web",
				DocType:    "io.cozy.urls",
				URL:        urlStr,
				Title:      title,
				Snippet:    snippet,
			})
		} else {
			subject, _ := chunkField(src, "email.subject").(string)
			datetime, _ := chunkField(src, "datetime").(string)
			emailPreview, _ := chunkField(src, "email.preview").(string)
			relationshipID, _ := chunkField(src, "relationship_id").(string)
			parentID, _ := chunkField(src, "parent_id").(string)
			doctype, _ := chunkField(src, "doctype").(string)
			fileID, _ := chunkField(src, "file_id").(string)
			fileName, _ := chunkField(src, "filename").(string)
			page := 0
			if p, ok := chunkField(src, "page").(float64); ok {
				page = int(p)
			}
			// The URLs are computed by openRAG and always at the top level.
			fileURL, _ := src["file_url"].(string)
			chunkURL, _ := src["chunk_url"].(string)
			sources = append(sources, Source{
				SourceType:     "document",
				ID:             fileID,
				DocType:        doctype,
				Filename:       fileName,
				Page:           page,
				FileURL:        fileURL,
				ChunkURL:       chunkURL,
				EmailPreview:   emailPreview,
				RelationshipID: relationshipID,
				Subject:        subject,
				Datetime:       datetime,
				ParentID:       parentID,
			})
		}
	}
	return sources, nil
}

// assistantForChat loads the assistant bound to the conversation. It returns
// (nil, nil) only when the conversation has no assistant at all. An error
// means either the referenced assistant is gone (ErrAssistantNotFound) or
// its configuration could not be read (e.g. a transient CouchDB error); in
// both cases the caller MUST NOT proceed as if the conversation were
// unscoped.
func assistantForChat(inst *instance.Instance, chat *ChatConversation) (*chatAssistant, error) {
	rel, ok := chat.Rels["assistant"]
	if !ok {
		return nil, nil
	}
	relData, _ := rel.Data.(map[string]interface{})
	assistantID, _ := relData["_id"].(string)
	if assistantID == "" {
		return nil, nil
	}
	var assistant chatAssistant
	if err := couchdb.GetDoc(inst, consts.ChatAssistants, assistantID, &assistant); err != nil {
		// The conversation references an assistant that cannot be found
		// (deleted, or never existed). It may have been scoped to a
		// knowledge base: degrading to unscoped retrieval would silently
		// widen it to the whole instance, so surface an explicit error and
		// let the client deal with the dangling reference.
		if couchdb.IsNotFoundError(err) {
			return nil, ErrAssistantNotFound
		}
		return nil, err
	}
	return &assistant, nil
}

func llmModel(acc *account.Account) string {
	if model, _ := acc.Data["model"].(string); model != "" {
		return model
	}
	if acc.Basic != nil {
		return acc.Basic.Login
	}
	return ""
}

func llmAPIKey(basic *account.BasicInfo) string {
	if basic == nil {
		return ""
	}
	if basic.EncryptedCredentials == "" {
		return basic.Password
	}
	_, apiKey, err := account.DecryptCredentials(basic.EncryptedCredentials)
	if err != nil {
		return basic.Password
	}
	return apiKey
}

// buildLLMOverride returns the `metadata.llm_override` map forwarded to
// OpenRAG when the conversation is bound to an assistant that uses an
// external provider (OpenAI, Mistral, …). It returns nil to leave the
// stack's default RAG configuration in place: either no assistant is
// attached, the provider is the default "openrag", or the linked account
// could not be resolved.
func buildLLMOverride(inst *instance.Instance, assistant *chatAssistant) map[string]interface{} {
	if assistant == nil {
		return nil
	}
	provider := assistant.Relationships.Provider.Data
	if provider.ID == "" || provider.Metadata.ProviderID == "" || provider.Metadata.ProviderID == "openrag" {
		return nil
	}

	var acc account.Account
	if err := couchdb.GetDoc(inst, consts.Accounts, provider.ID, &acc); err != nil {
		return nil
	}
	model, apiKey := llmModel(&acc), llmAPIKey(acc.Basic)
	override := map[string]interface{}{}
	if model != "" {
		override["model"] = model
	}
	if apiKey != "" {
		override["api_key"] = apiKey
	}
	if baseURL, _ := acc.Data["baseUrl"].(string); baseURL != "" {
		override["base_url"] = baseURL
	}
	if len(override) == 0 {
		return nil
	}
	return override
}

// ragMessage is one entry of the messages array sent to openRAG.
type ragMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ragMessages builds the messages sent to openRAG: the assistant's prompt as
// a leading system message (openRAG pins it as a custom instruction), then
// the conversation turns. The prompt is read on every query so an edit of the
// assistant applies at once; the leading system messages an older stack saved
// in the conversation are skipped, so the current prompt replaces them.
func ragMessages(chat *ChatConversation, assistant *chatAssistant) []ragMessage {
	messages := make([]ragMessage, 0, len(chat.Messages)+1)
	if assistant != nil {
		if prompt := strings.TrimSpace(assistant.Prompt); prompt != "" {
			messages = append(messages, ragMessage{Role: SystemRole, Content: prompt})
		}
	}
	turns := chat.Messages
	for len(turns) > 0 && turns[0].Role == SystemRole {
		turns = turns[1:]
	}
	for _, msg := range turns {
		content := msg.Content
		if msg.Action != nil {
			content = strings.TrimSpace(content + "\n\n" + describeAction(msg.Action))
		}
		messages = append(messages, ragMessage{Role: msg.Role, Content: content})
	}
	return messages
}

// withInstructions adds the instructions of the client to the messages, as a
// system message after the prompt of the assistant: the LLM gets it as it is
// without the documents, and openRAG takes the leading system messages as
// custom instructions.
func withInstructions(messages []ragMessage, instructions string) []ragMessage {
	if instructions == "" {
		return messages
	}
	i := 0
	for i < len(messages) && messages[i].Role == SystemRole {
		i++
	}
	out := make([]ragMessage, 0, len(messages)+1)
	out = append(out, messages[:i]...)
	out = append(out, ragMessage{Role: SystemRole, Content: instructions})
	return append(out, messages[i:]...)
}

// directLLMPayload is the body of a chat completion without the documents.
// openRAG takes a request without a partition as its model as one for the LLM
// itself: it gets the messages as they are, with no retrieval and no system
// prompt of openRAG.
func directLLMPayload(messages []ragMessage, stream bool, metadata map[string]interface{}) map[string]interface{} {
	payload := map[string]interface{}{
		"messages":    messages,
		"stream":      stream,
		"temperature": Temperature,
		"top_p":       TopP,
		"logprobs":    LogProbs,
	}
	kept := map[string]interface{}{}
	if override, ok := metadata["llm_override"]; ok {
		kept["llm_override"] = override
	}
	if websearch, _ := metadata["websearch"].(bool); websearch {
		kept["websearch"] = true
	}
	if len(kept) > 0 {
		payload["metadata"] = kept
	}
	return payload
}

func Query(inst *instance.Instance, logger logger.Logger, query QueryMessage) error {
	var chat ChatConversation
	err := couchdb.GetDoc(inst, consts.ChatConversations, query.DocID, &chat)
	if err != nil {
		return err
	}

	metadata := map[string]interface{}{
		"websearch": query.WebSearch,
	}
	if len(query.AttachmentIDs) > 0 {
		attachments := make([]map[string]string, len(query.AttachmentIDs))
		for i, id := range query.AttachmentIDs {
			attachments[i] = map[string]string{"id": id}
		}
		metadata["attachments"] = attachments
	}
	msg := chat.Messages[len(chat.Messages)-1]
	// The answer from the documents is asked with a context: it is cancelled
	// when an action replaces it, which stops openRAG.
	ctx := context.Background()
	assistant, err := assistantForChat(inst, &chat)
	if err != nil {
		// Without the assistant we cannot know whether the conversation is
		// scoped to a knowledge base, and a folder-scoped assistant must
		// never silently answer unscoped: surface the error and stop. This
		// deliberately also fails assistants that only carry an llm_override
		// (which used to fall back to the stack's default RAG configuration
		// on such errors): scoping cannot be ruled out without the doc.
		logger.Warnf("cannot resolve assistant: %s", err)
		publishError(inst, msg.ID, err)
		return err
	}
	if override := buildLLMOverride(inst, assistant); override != nil {
		metadata["llm_override"] = override
	}
	// Without the documents, the knowledge base of the assistant is not
	// searched: its workspace does not have to be there.
	if dirID := assistant.knowledgeBaseDirID(logger); dirID != "" && !query.DirectLLM {
		workspaceID := workspaceIDForDir(dirID)
		if err := checkWorkspace(inst, workspaceID); err != nil {
			logger.Warnf("RAG workspace %s unavailable: %s", workspaceID, err)
			// A folder-scoped assistant must never answer from the whole
			// instance: surface the error to the client and stop.
			publishError(inst, msg.ID, err)
			return err
		}
		metadata["workspace"] = workspaceID
	}
	// The answer comes from the documents with openRAG, or, when the client
	// asks for an answer without them, from the LLM behind it.
	messages := withInstructions(ragMessages(&chat, assistant), query.Instructions)
	var body []byte
	if query.DirectLLM {
		body, err = json.Marshal(directLLMPayload(messages, query.Stream, metadata))
	} else {
		body, err = ragRequest(inst, messages, query.Stream, metadata, 0)
	}
	if err != nil {
		return err
	}

	actions := query.Actions
	if len(actions) == 0 {
		completion, sources, err := askRAG(ctx, inst, logger, msg, body, query.Stream, nil)
		if err != nil {
			publishError(inst, msg.ID, err)
			return err
		}
		publishDone(inst, msg.ID)
		return saveAnswer(inst, &chat, newMessageID(), completion, sources, nil)
	}

	// The router and the answer from the documents start together, so that
	// a search does not wait for the router. The router answers long before
	// the first token of the answer, which follows the query rewriting, the
	// retrieval and the reranking: the answer is held back until the router
	// has decided, and when it is not the one to give, it is cancelled,
	// which stops openRAG.
	override, _ := metadata["llm_override"].(map[string]interface{})
	ragCtx, cancelRAG := context.WithCancel(ctx)
	defer cancelRAG()
	decided := make(chan routeDecision, 1)
	go func() {
		started := time.Now()
		decision, err := routeQuery(ctx, inst, messages, actions, override)
		if err != nil {
			logger.Warnf("chat router failed, answering from the documents: %s", err)
		} else {
			logger.Infof("chat router: intent=%s needs_documents=%t in %s", decision.Intent, decision.NeedsDocuments, time.Since(started))
		}
		workspace, _ := metadata["workspace"].(string)
		checked, err := checkDocuments(ctx, inst, decision, msg.Content, workspace)
		if err != nil {
			logger.Warnf("chat router: cannot search the documents, using them: %s", err)
		} else if checked.NeedsDocuments != decision.NeedsDocuments {
			logger.Infof("chat router: the documents are relevant to the message, using them")
		}
		decision = checked
		if !keepsAnswer(actions, decision) {
			cancelRAG()
		}
		decided <- decision
	}()
	var decision *routeDecision
	waitDecision := func() routeDecision {
		if decision == nil {
			d := <-decided
			decision = &d
		}
		return *decision
	}
	answerAllowed := func() bool { return keepsAnswer(actions, waitDecision()) }

	completion, sources, err := askRAG(ragCtx, inst, logger, msg, body, query.Stream, answerAllowed)
	answerID := newMessageID()
	def := actionFor(actions, waitDecision().action())
	now := time.Now().UTC()
	var action *ChatAction
	switch {
	case def == nil:
		// A search: the answer is the one to give.

	case def.Content != nil:
		// The content of the action is written as the answer, with its own
		// instructions, and the action saves it.
		var written bool
		completion, sources, written, err = writeContent(ctx, inst, logger, msg, def, messages, query.Stream, metadata, override, waitDecision().NeedsDocuments, now)
		if err == nil && written {
			if title := contentTitle(completion); title != "" {
				action = &ChatAction{Name: def.Name, Params: map[string]interface{}{"title": title}}
			} else {
				logger.Infof("chat router: the content of %s has no title, no action proposed", def.Name)
			}
		}

	case !waitDecision().NeedsDocuments:
		action, err = fillAction(ctx, inst, def, messages, "", override, now)
		if err == nil {
			publishAction(inst, msg.ID, answerID, action)
			publishDone(inst, msg.ID)
			return saveAnswer(inst, &chat, answerID, "", nil, action)
		}
		// The user still gets an answer when the action cannot be prepared.
		logger.Warnf("chat router: cannot prepare %s, answering from the documents: %s", def.Name, err)
		action = nil
		completion, sources, err = askRAG(ctx, inst, logger, msg, body, query.Stream, nil)

	case err == nil:
		// An action that needs the documents is prepared from the answer,
		// and shown below it.
		var fillErr error
		action, fillErr = fillAction(ctx, inst, def, messages, completion, override, now)
		if fillErr != nil {
			logger.Warnf("chat router: cannot prepare %s after the answer: %s", def.Name, fillErr)
			action = nil
		}
	}
	if err != nil {
		publishError(inst, msg.ID, err)
		return err
	}
	if action != nil {
		publishAction(inst, msg.ID, answerID, action)
	}
	publishDone(inst, msg.ID)
	return saveAnswer(inst, &chat, answerID, completion, sources, action)
}

// ragRequest is the body of a query to openRAG for an answer from the
// documents. maxTokens is 0 for the default of openRAG.
func ragRequest(inst *instance.Instance, messages []ragMessage, stream bool, metadata map[string]interface{}, maxTokens int) ([]byte, error) {
	payload := map[string]interface{}{
		"model":       fmt.Sprintf("ragondin-%s", inst.Domain),
		"messages":    messages,
		"stream":      stream,
		"metadata":    metadata,
		"temperature": Temperature,
		"top_p":       TopP,
		"logprobs":    LogProbs,
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	return json.Marshal(payload)
}

// writeContent writes the content of an action as the answer, published as
// it comes: from the user's documents with openRAG, or else by the LLM from
// the conversation. written is false when it could not be written before
// anything was published, and the answer from the documents is given
// instead.
func writeContent(ctx context.Context, inst *instance.Instance, logger logger.Logger, msg ChatMessage, a *ActionDefinition, messages []ragMessage, stream bool, metadata, override map[string]interface{}, fromDocuments bool, now time.Time) (completion string, sources []Source, written bool, err error) {
	var body []byte
	if fromDocuments {
		body, err = ragRequest(inst, withWritingInstructions(messages, a, now), stream, metadata, contentTokens(a))
	} else {
		body, err = writingRequest(a, messages, stream, override, now)
	}
	if err != nil {
		return "", nil, false, err
	}
	published := false
	completion, sources, err = askRAG(ctx, inst, logger, msg, body, stream, func() bool {
		published = true
		return true
	})
	if err == nil && strings.TrimSpace(completion) != "" {
		return completion, sources, true, nil
	}
	if published || ctx.Err() != nil {
		// A part of the content has reached the user, and another answer
		// would be mixed with it, or the query is cancelled.
		return "", nil, false, err
	}
	logger.Warnf("chat router: cannot write the content of %s, answering from the documents: %v", a.Name, err)
	if body, err = ragRequest(inst, messages, stream, metadata, 0); err != nil {
		return "", nil, false, err
	}
	completion, sources, err = askRAG(ctx, inst, logger, msg, body, stream, nil)
	return completion, sources, false, err
}

// askRAG asks openRAG for the answer from the documents, publishing it on the
// realtime as it comes. allowed, when set, is called before the first event
// is published: when it returns false, nothing is published.
func askRAG(ctx context.Context, inst *instance.Instance, logger logger.Logger, msg ChatMessage, body []byte, stream bool, allowed func() bool) (string, []Source, error) {
	res, err := CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
	if err != nil {
		return "", nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		checkRes, err := CallRAGQueryContext(ctx, inst, http.MethodGet, nil, fmt.Sprintf("/partition/%s", inst.Domain), echo.MIMEApplicationJSON)
		if err != nil {
			return "", nil, err
		}
		checkRes.Body.Close()
		if checkRes.StatusCode == http.StatusNotFound {
			logger.Warnf("RAG partition not found, attempting creation")
			createRAGPartition(inst.RAGServer(), inst.Domain, logger)
			res, err = CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
			if err != nil {
				return "", nil, err
			}
		}
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		return "", nil, fmt.Errorf("POST status code: %d", res.StatusCode)
	}
	if stream {
		return handleStreamResponse(inst, msg, res.Body, allowed)
	}
	return handleNonStreamResponse(inst, msg, res.Body, allowed)
}

func newMessageID() string {
	uuidv7, _ := uuid.NewV7()
	return uuidv7.String()
}

func saveAnswer(inst *instance.Instance, chat *ChatConversation, id, completion string, sources []Source, action *ChatAction) error {
	answer := ChatMessage{
		ID:        id,
		Role:      AssistantRole,
		Content:   completion,
		Sources:   sources,
		Action:    action,
		CreatedAt: time.Now().UTC(),
	}
	chat.Messages = append(chat.Messages, answer)
	return couchdb.UpdateDoc(inst, chat)
}

func publishDelta(inst *instance.Instance, msgID string, content string, position int) {
	doc := couchdb.JSONDoc{
		Type: consts.ChatEvents,
		M: map[string]interface{}{
			"_id":      msgID,
			"object":   "delta",
			"content":  content,
			"position": position,
		},
	}
	doc.SetID(msgID)
	realtime.GetHub().Publish(inst, realtime.EventCreate, &doc, nil)
}

func publishSources(inst *instance.Instance, msgID string, sources []Source) {
	doc := couchdb.JSONDoc{
		Type: consts.ChatEvents,
		M: map[string]interface{}{
			"_id":     msgID,
			"object":  "sources",
			"content": sources,
		},
	}
	doc.SetID(msgID)
	realtime.GetHub().Publish(inst, realtime.EventCreate, &doc, nil)
}

// publishAction sends the action proposed to the user, which the client
// shows for confirmation, with the id of the assistant message it is saved
// on, where the client writes its outcome.
func publishAction(inst *instance.Instance, msgID, answerID string, action *ChatAction) {
	doc := couchdb.JSONDoc{
		Type: consts.ChatEvents,
		M: map[string]interface{}{
			"_id":        msgID,
			"object":     "action",
			"action":     action,
			"message_id": answerID,
		},
	}
	doc.SetID(msgID)
	realtime.GetHub().Publish(inst, realtime.EventCreate, &doc, nil)
}

// publishError sends the `{object:"error"}` chat event, so a client waiting
// on the realtime feed is never left hanging when the query cannot complete.
func publishError(inst *instance.Instance, msgID string, err error) {
	doc := couchdb.JSONDoc{
		Type: consts.ChatEvents,
		M: map[string]interface{}{
			"object":  "error",
			"message": err.Error(),
		},
	}
	doc.SetID(msgID)
	realtime.GetHub().Publish(inst, realtime.EventCreate, &doc, nil)
}

func publishDone(inst *instance.Instance, msgID string) {
	doc := couchdb.JSONDoc{
		Type: consts.ChatEvents,
		M: map[string]interface{}{
			"_id":    msgID,
			"object": "done",
		},
	}
	doc.SetID(msgID)
	realtime.GetHub().Publish(inst, realtime.EventCreate, &doc, nil)
}

// handleStreamResponse publishes the answer streamed by openRAG, unless
// allowed returns false, and returns it with its sources. The `done` event is
// left to the caller, which may send an action before it.
func handleStreamResponse(inst *instance.Instance, msg ChatMessage, body io.Reader, allowed func() bool) (string, []Source, error) {
	position := 0
	var completion string
	var sources []Source
	var sseErr error
	canPublish := publishGate(allowed)

	// Realtime messages are sent to the client during the response stream
	// When the stream is finished, the whole answer is saved in the CouchDB document
	// The publish calls must stay synchronous: publishing from goroutines lets
	// events reach the realtime hub out of order, and a `done` overtaking the
	// last deltas makes clients render a truncated answer.
	err := foreachSSE(body, func(event map[string]interface{}) {
		// See https://platform.openai.com/docs/api-reference/chat-streaming/streaming#chat-streaming
		if event["object"] == "chat.completion.chunk" {
			choices, ok := event["choices"].([]interface{})
			if !ok || len(choices) < 1 {
				return
			}
			choice := choices[0].(map[string]interface{}) // Only one choice is possible for now

			if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
				return
			} else if delta, ok := choice["delta"].(map[string]interface{}); ok {
				// The content is progressively reveived through a delta stream
				content, ok := delta["content"].(string)
				if !ok {
					return
				}
				if canPublish() {
					publishDelta(inst, msg.ID, content, position)
				}
				completion += content
				position++

				if sources == nil {
					// Sources are included in all delta messages, but should be sent once
					sources, sseErr = getSources(event)
					if sseErr != nil {
						return
					}
					if sources != nil && canPublish() {
						publishSources(inst, msg.ID, sources)
					}
				}
			}
		}
	})

	if err != nil {
		return "", nil, err
	}
	if sseErr != nil {
		return "", nil, sseErr
	}
	return completion, sources, nil
}

// handleNonStreamResponse is handleStreamResponse for an answer that openRAG
// sends in one piece.
func handleNonStreamResponse(inst *instance.Instance, msg ChatMessage, body io.Reader, allowed func() bool) (string, []Source, error) {
	var event map[string]interface{}
	if err := json.NewDecoder(body).Decode(&event); err != nil {
		return "", nil, err
	}

	var completion string
	if choices, ok := event["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				completion, _ = message["content"].(string)
			}
		}
	}
	if completion == "" {
		return "", nil, errors.New("invalid RAG response: no completion content")
	}

	sources, err := getSources(event)
	if err != nil {
		return "", nil, err
	}

	if publishGate(allowed)() {
		publishDelta(inst, msg.ID, completion, 0)
		if sources != nil {
			publishSources(inst, msg.ID, sources)
		}
	}
	return completion, sources, nil
}

// publishGate returns a function telling whether the events of an answer can
// be published: allowed is asked once, before the first event.
func publishGate(allowed func() bool) func() bool {
	asked, publish := allowed == nil, true
	return func() bool {
		if !asked {
			asked = true
			publish = allowed()
		}
		return publish
	}
}

// ragHTTPClient is the HTTP client used for the openRAG calls. It has no
// global timeout (chat completions may stream for minutes) but bounds
// connection establishment and the wait for response headers, so a hung
// openRAG server cannot pin a rag-query worker — or a rag-index batch and
// the per-instance lock it holds — forever.
var ragHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 5 * time.Minute,
		// A custom Transport opts out of automatic HTTP/2, which can cause
		// a feeling of buffered blocks of text for the user because of HTTP/1.1
		ForceAttemptHTTP2: true,
	},
}

// callRAG is the instance-free part of CallRAGQuery, split out so the openRAG
// HTTP mechanics can be unit-tested against an httptest server.
func callRAG(server config.RAGServer, method string, payload []byte, path string, contentType string) (*http.Response, error) {
	return callRAGContext(context.Background(), server, method, payload, path, contentType)
}

// callRAGContext is callRAG bound to a context: cancelling it closes the
// connection to openRAG, which stops the work in progress on its side.
func callRAGContext(ctx context.Context, server config.RAGServer, method string, payload []byte, path string, contentType string) (*http.Response, error) {
	if server.URL == "" {
		return nil, errors.New("no RAG server configured")
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		return nil, err
	}

	// The path is treated as already percent-encoded: callers escape their
	// dynamic segments with url.PathEscape (folder and file ids are arbitrary
	// CouchDB ids and must not be able to break out of their path segment),
	// and the escaped form must be sent as-is, without double encoding.
	unescaped, err := url.PathUnescape(path)
	if err != nil {
		return nil, err
	}
	u.Path = unescaped
	u.RawPath = path
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+server.APIKey)
	req.Header.Add("Content-Type", contentType)
	return ragHTTPClient.Do(req)
}

func CallRAGQuery(inst *instance.Instance, method string, payload []byte, path string, contentType string) (*http.Response, error) {
	return callRAG(inst.RAGServer(), method, payload, path, contentType)
}

// CallRAGQueryContext is CallRAGQuery bound to a context.
func CallRAGQueryContext(ctx context.Context, inst *instance.Instance, method string, payload []byte, path string, contentType string) (*http.Response, error) {
	return callRAGContext(ctx, inst.RAGServer(), method, payload, path, contentType)
}

// createdOrExists tells whether an openRAG create endpoint reported success,
// treating 409 (already created concurrently) as success.
func createdOrExists(statusCode int) bool {
	return (statusCode >= 200 && statusCode < 300) || statusCode == http.StatusConflict
}

// createRAGPartition creates the instance's partition on the openRAG server.
// Best-effort: failures are only logged, and a 409 (partition already
// created) is treated as success.
func createRAGPartition(server config.RAGServer, domain string, logger logger.Logger) {
	res, err := callRAG(server, http.MethodPost, nil, fmt.Sprintf("/partition/%s", domain), echo.MIMEApplicationJSON)
	if err != nil {
		logger.Warnf("Failed to create RAG partition: %s", err)
		return
	}
	res.Body.Close()
	if !createdOrExists(res.StatusCode) {
		logger.Warnf("Failed to create RAG partition, status: %d", res.StatusCode)
	}
}

func foreachSSE(r io.Reader, fn func(event map[string]interface{})) error {
	rb := bufio.NewReader(r)
	for {
		bs, err := rb.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if bytes.Equal(bs, []byte("\n")) || bytes.Equal(bs, []byte("\r\n")) {
			continue
		}
		if bytes.HasPrefix(bs, []byte(":")) {
			continue
		}
		parts := bytes.SplitN(bs, []byte(": "), 2)
		if len(parts) != 2 {
			return errors.New("invalid SSE response")
		}
		if string(parts[0]) != "data" {
			continue
		}
		data := bytes.TrimSpace(parts[1])
		if string(data) == "[DONE]" {
			break
		}
		var event map[string]interface{}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		// Check for error event from the server
		if errObj, ok := event["error"].(map[string]interface{}); ok {
			message, _ := errObj["message"].(string)
			code, _ := errObj["code"].(string)
			if message == "" {
				message = "unknown streaming error"
			}
			if code != "" {
				return fmt.Errorf("%s: %s", code, message)
			}
			return errors.New(message)
		}
		fn(event)
	}
	return nil
}
