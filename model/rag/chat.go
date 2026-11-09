package rag

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
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
	"github.com/cozy/cozy-stack/pkg/utils"
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
	// Tools are the definitions of the actions the client can run, given to
	// the LLM as its tools.
	Tools []ToolDefinition `json:"tools,omitempty"`
	// Deprecated: the former name of Tools, still read for the clients
	// deployed with it. Ignored when Tools is given.
	Actions []ToolDefinition `json:"actions,omitempty"`
	// Result, instead of q, is the result of the action proposed by an
	// answer, once the client has run it: the LLM goes on from it.
	Result *ActionResult `json:"result,omitempty"`
}

// ActionResult is the result of an action the client ran: the ID of the
// answer that proposed it, whether it worked, and what it returned.
type ActionResult struct {
	MessageID string `json:"messageID"`
	OK        bool   `json:"ok"`
	Content   string `json:"content,omitempty"`
}

// maxResultChars bounds the content of a result kept in the conversation:
const maxResultChars = 8000

// ErrResultNotAwaited is returned for a result whose message has proposed
// no action, or whose result has already been given.
var ErrResultNotAwaited = errors.New("the message has proposed no action, or its result is already given")

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
	if p.Result != nil {
		if p.Query != "" || len(p.AttachmentIDs) > 0 {
			return errors.New("result cannot be used with q or attachmentIDs")
		}
		if p.Result.MessageID == "" {
			return errors.New("result.messageID is required")
		}
	}
	if p.directLLM() && len(p.AttachmentIDs) > 0 {
		return errors.New("attachmentIDs cannot be used without the documents")
	}
	if n := utf8.RuneCountInString(p.Instructions); n > maxInstructionsChars {
		return fmt.Errorf("instructions too long: %d characters, at most %d", n, maxInstructionsChars)
	}
	return ValidateTools(p.tools())
}

// tools are the definitions of the actions, under their current or their
// former name.
func (p ChatPayload) tools() []ToolDefinition {
	if len(p.Tools) > 0 {
		return p.Tools
	}
	return p.Actions
}

type ChatConversation struct {
	DocID        string                  `json:"_id"`
	DocRev       string                  `json:"_rev,omitempty"`
	Messages     []ChatMessage           `json:"messages"`
	CozyMetadata *metadata.CozyMetadata  `json:"cozyMetadata"`
	Rels         jsonapi.RelationshipMap `json:"relationships,omitempty"`
	// Attributes are the fields a client writes on the conversation that the
	// stack does not use, like the name the user gives it: kept as they are
	// when the stack saves the conversation.
	Attributes map[string]json.RawMessage `json:"-"`
}

// chatConversationFields are the fields of a conversation the stack reads
// and writes itself.
var chatConversationFields = []string{"_id", "_rev", "messages", "cozyMetadata", "relationships"}

// isChatConversationField tells whether a key is one of the stack: compared
// without case, as encoding/json matches the keys of a struct.
func isChatConversationField(key string, _ json.RawMessage) bool {
	return slices.ContainsFunc(chatConversationFields, func(field string) bool {
		return strings.EqualFold(field, key)
	})
}

// chatConversationJSON has the fields of a conversation, without its
// methods.
type chatConversationJSON ChatConversation

func (c *ChatConversation) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, (*chatConversationJSON)(c)); err != nil {
		return err
	}
	var attributes map[string]json.RawMessage
	if err := json.Unmarshal(data, &attributes); err != nil {
		return err
	}
	maps.DeleteFunc(attributes, isChatConversationField)
	c.Attributes = attributes
	return nil
}

func (c *ChatConversation) MarshalJSON() ([]byte, error) {
	data, err := json.Marshal((*chatConversationJSON)(c))
	if err != nil {
		return nil, err
	}
	// The fields of the stack win over attributes of the same name
	attributes := maps.Clone(c.Attributes)
	maps.DeleteFunc(attributes, isChatConversationField)
	if len(attributes) == 0 {
		return data, nil
	}
	extra, err := json.Marshal(attributes)
	if err != nil {
		return nil, err
	}
	// Both are JSON objects: join them without decoding the messages again
	data = append(data[:len(data)-1], ',')
	return append(data, extra[1:]...), nil
}

type ChatMessage struct {
	ID            string      `json:"id"`
	Role          string      `json:"role"`
	Content       string      `json:"content"`
	Reasoning     string      `json:"reasoning,omitempty"`
	Sources       []Source    `json:"sources,omitempty"`
	AttachmentIDs []string    `json:"attachmentIDs,omitempty"`
	Action        *ChatAction `json:"action,omitempty"`
	// Result links a tool message to the answer whose action it ran; the
	// content of the result is the content of the message.
	Result    *ActionResult `json:"result,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
}

const (
	UserRole      = "user"
	AssistantRole = "assistant"
	SystemRole    = "system"
	ToolRole      = "tool"
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
	cloned.Attributes = maps.Clone(c.Attributes)
	return &cloned
}
func (c *ChatConversation) Included() []jsonapi.Object             { return nil }
func (c *ChatConversation) Relationships() jsonapi.RelationshipMap { return c.Rels }
func (c *ChatConversation) Links() *jsonapi.LinksList              { return nil }

var _ jsonapi.Object = (*ChatConversation)(nil)

type QueryMessage struct {
	Task          string           `json:"task"`
	DocID         string           `json:"doc_id"`
	Stream        bool             `json:"stream"`
	WebSearch     bool             `json:"websearch"`
	AttachmentIDs []string         `json:"attachmentIDs,omitempty"`
	Tools         []ToolDefinition `json:"tools,omitempty"`
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
	// Deprecated: replaced by Relationships.KnowledgeBase, still read for the
	// assistants saved by older versions of the app.
	KnowledgeBase []knowledgeBaseEntry `json:"knowledgeBase,omitempty"`
	Prompt        string               `json:"prompt,omitempty"`
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
	KnowledgeBase struct {
		Data []struct {
			ID   string `json:"_id"`
			Type string `json:"_type"`
		} `json:"data"`
	} `json:"knowledgeBase"`
}

func (a *chatAssistant) ID() string         { return a.DocID }
func (a *chatAssistant) Rev() string        { return a.DocRev }
func (a *chatAssistant) DocType() string    { return consts.ChatAssistants }
func (a *chatAssistant) SetID(id string)    { a.DocID = id }
func (a *chatAssistant) SetRev(rev string)  { a.DocRev = rev }
func (a *chatAssistant) Clone() couchdb.Doc { c := *a; return &c }

// knowledgeBaseFolders returns the ids of the assistant's knowledge base
// folders: from the relationships when they have one, else from the
// deprecated knowledgeBase attribute.
func (a *chatAssistant) knowledgeBaseFolders() []string {
	var ids []string
	for _, ref := range a.Relationships.KnowledgeBase.Data {
		if ref.Type == consts.Files && ref.ID != "" {
			ids = append(ids, ref.ID)
		}
	}
	if len(ids) > 0 {
		return ids
	}
	for _, entry := range a.KnowledgeBase {
		if entry.Doctype == consts.Files && entry.DirID != "" {
			ids = append(ids, entry.DirID)
		}
	}
	return ids
}

// knowledgeBaseDirID returns the Drive folder scoping the assistant's
// retrieval, or "" when the assistant has no knowledge base. Only a single
// folder per assistant is supported: extra folders are ignored,
// with a warning so the truncation is at least visible in the logs.
func (a *chatAssistant) knowledgeBaseDirID(logger logger.Logger) string {
	if a == nil {
		return ""
	}
	dirID := ""
	for _, id := range a.knowledgeBaseFolders() {
		if dirID == "" {
			dirID = id
		} else if id != dirID {
			logger.Warnf("assistant %s: multiple knowledge base folders are not supported, ignoring %s", a.DocID, id)
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
	uuidv7, _ := uuid.NewV7()
	msg := ChatMessage{
		ID:            uuidv7.String(),
		Role:          UserRole,
		Content:       payload.Query,
		AttachmentIDs: payload.AttachmentIDs,
		CreatedAt:     time.Now().UTC(),
	}
	if payload.Result != nil {
		msg.Role = ToolRole
		msg.Content = truncate(payload.Result.Content, maxResultChars)
		msg.Result = &ActionResult{MessageID: payload.Result.MessageID, OK: payload.Result.OK}
	}
	// As for the answer, the conversation is read again on a conflict: the
	// client writes it too, e.g. to rename or pin it.
	var chat ChatConversation
	err := utils.RetryWithBackoff(context.Background(), utils.RetryOptions{
		Attempts:    3,
		ShouldRetry: couchdb.IsConflictError,
	}, func() error {
		chat = ChatConversation{}
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
			return err
		} else {
			chat.CozyMetadata.UpdatedAt = time.Now().UTC()
		}
		if msg.Result != nil && !awaitsResult(chat.Messages, msg.Result.MessageID) {
			return ErrResultNotAwaited
		}
		chat.Messages = append(chat.Messages, msg)
		if chat.DocRev == "" {
			return couchdb.CreateNamedDocWithDB(inst, &chat)
		}
		return updateConversation(inst, &chat)
	})
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
		Tools:         payload.tools(),
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

// awaitsResult tells whether the message is an answer that proposed an
// action, with no result yet.
func awaitsResult(messages []ChatMessage, msgID string) bool {
	proposed := false
	for _, msg := range messages {
		if msg.ID == msgID {
			proposed = msg.Role == AssistantRole && msg.Action != nil
		}
		if msg.Result != nil && msg.Result.MessageID == msgID {
			return false
		}
	}
	return proposed
}

// maxActionSteps bounds the actions run in a row for a message of the user,
// so that the LLM cannot propose actions without end.
const maxActionSteps = 5

// actionSteps counts the results of the actions given since the last message
// of the user.
func actionSteps(messages []ChatMessage) int {
	steps := 0
	for i := len(messages) - 1; i >= 0 && messages[i].Role != UserRole; i-- {
		if messages[i].Role == ToolRole {
			steps++
		}
	}
	return steps
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

// ragMessage is one entry of the messages array sent to openRAG, as in the
// OpenAI API: an assistant message may carry the tool call of its action, and
// a tool message answers one.
type ragMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// MarshalJSON sends a result whose action is not in the conversation, like
// an answer that was never saved, as a user message: a tool message must
// answer a tool call, or the LLM refuses the conversation.
func (m ragMessage) MarshalJSON() ([]byte, error) {
	type plain ragMessage
	if m.Role == ToolRole && m.ToolCallID == "" {
		m.Role = UserRole
		m.Content = "(Result of an action, given by the app, not written by the user:)\n\n" + m.Content
	}
	return json.Marshal(plain(m))
}

// ragMessages builds the messages sent to openRAG: the assistant's prompt as
// a leading system message (openRAG pins it as a custom instruction), then
// the conversation turns. The prompt is read on every query so an edit of the
// assistant applies at once; the leading system messages an older stack saved
// in the conversation are skipped, so the current prompt replaces them. A
// question without an answer, stopped by the user or failed, is skipped too:
// the LLM would answer it along with the next one.
//
// An action with a result is a tool call answered by a tool message. An
// action without one is described in the content of its message: a tool
// call must have its answer.
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
	// The messages of the actions that got their result
	answered := map[string]bool{}
	for _, msg := range turns {
		if msg.Result != nil {
			answered[msg.Result.MessageID] = true
		}
	}
	// The ids of their tool calls, met before their results
	callIDs := map[string]string{}
	for i, msg := range turns {
		if msg.Role == UserRole && i+1 < len(turns) && turns[i+1].Role == UserRole {
			continue
		}
		out := ragMessage{Role: msg.Role, Content: msg.Content}
		if msg.Action != nil && answered[msg.ID] {
			call := toolCallOf(msg)
			callIDs[msg.ID] = call.ID
			out.ToolCalls = []toolCall{call}
		} else if msg.Action != nil {
			out.Content = strings.TrimSpace(describeAction(msg.Action) + "\n\n" + msg.Content)
		}
		if msg.Result != nil {
			out.ToolCallID = callIDs[msg.Result.MessageID]
			out.Content = resultContent(msg.Result.OK, msg.Content)
		}
		messages = append(messages, out)
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

// withTools gives the LLM the actions as its tools. One action per message:
// of several calls, the first is kept, the LLM asks the others again after
// its result. The single_tool_call option of the RAG server asks the LLM for
// one call at a time; off by default, as some gateways refuse the parameter.
func withTools(payload map[string]interface{}, defs []ToolDefinition, server config.RAGServer) {
	if len(defs) == 0 {
		return
	}
	payload["tools"] = tools(defs)
	payload["tool_choice"] = "auto"
	if server.SingleToolCall {
		payload["parallel_tool_calls"] = false
	}
}

// toolsPrompt is the system message that goes with the tools: when to call
// one, and how to fill its arguments.
func toolsPrompt(fromDocuments bool, now time.Time) string {
	return renderPrompt("tools.txt", map[string]interface{}{
		"FromDocuments": fromDocuments,
		"Today":         promptDate(now),
	})
}

// chatQuery is a query of the assistant: the message it answers, what the
// LLM is asked with, and how the answer is given.
type chatQuery struct {
	inst           *instance.Instance
	logger         logger.Logger
	conversationID string
	// msg is the user message the query answers.
	msg ChatMessage
	// messages is the conversation sent to the LLM.
	messages []ragMessage
	// metadata goes with every query to openRAG for an answer from the
	// documents.
	metadata map[string]interface{}
	stream   bool
	// direct tells that the client asks for an answer without the documents.
	direct bool
	now    time.Time
	// watcher tells whether the user has cancelled the query.
	watcher *cancelWatcher
}

func Query(ctx context.Context, inst *instance.Instance, logger logger.Logger, query QueryMessage) error {
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
	// Everything the query asks openRAG is stopped when the user cancels it
	ctx, cancelQuery, watcher := watchCancel(ctx, inst, chat.DocID, msg.ID)
	defer cancelQuery()
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
	q := &chatQuery{
		inst:           inst,
		logger:         logger,
		conversationID: chat.DocID,
		msg:            msg,
		messages:       withInstructions(ragMessages(&chat, assistant), query.Instructions),
		metadata:       metadata,
		stream:         query.Stream,
		direct:         query.DirectLLM,
		now:            time.Now().UTC(),
		watcher:        watcher,
	}
	// After the last step, the LLM answers from the results it has
	if len(query.Tools) == 0 || actionSteps(chat.Messages) >= maxActionSteps {
		return q.answer(ctx)
	}
	return q.answerWithTools(ctx, query.Tools)
}

// body is the body of the query: from the documents with openRAG, or from
// the LLM behind it when the client asks for an answer without them. With
// actions, the LLM gets them as its tools, with the rules of their use.
func (q *chatQuery) body(defs []ToolDefinition) ([]byte, error) {
	messages := q.messages
	if len(defs) > 0 {
		messages = withInstructions(messages, toolsPrompt(!q.direct, q.now))
	}
	payload := ragPayload(q.inst, messages, q.stream, q.metadata)
	if q.direct {
		payload = directLLMPayload(messages, q.stream, q.metadata)
	}
	withTools(payload, defs, q.inst.RAGServer())
	return json.Marshal(payload)
}

// answer asks openRAG for the answer to the message, published as it comes,
// and saves it.
func (q *chatQuery) answer(ctx context.Context) error {
	body, err := q.body(nil)
	if err != nil {
		return q.fail(err)
	}
	answer, err := q.askRAG(ctx, body)
	if err != nil {
		return q.fail(err)
	}
	return q.finish(answer, nil)
}

// answerWithTools answers the message, or proposes the action the LLM calls.
// A call the stack cannot make an action of is dropped: the message is
// answered without the tools, unless the LLM wrote a text with its call.
func (q *chatQuery) answerWithTools(ctx context.Context, defs []ToolDefinition) error {
	body, err := q.body(defs)
	if err != nil {
		return q.fail(err)
	}
	answer, err := q.askRAG(ctx, body)
	if err != nil {
		return q.fail(err)
	}
	if len(answer.ToolCalls) == 0 {
		return q.finish(answer, nil)
	}
	action, err := actionFromCall(defs, answer.ToolCalls[0], userText(q.messages))
	if err != nil {
		q.logger.Warnf("chat tools: %s, answering the message instead", err)
		if answer.Content != "" {
			return q.finish(answer, nil)
		}
		return q.answer(ctx)
	}
	return q.finish(answer, action)
}

// finish ends the query: the answer is saved, then the action proposed, if
// any, is published before the `done` event. Saved first, so that a result
// the client posts on the `action` event finds its message.
func (q *chatQuery) finish(answer ragAnswer, action *ChatAction) error {
	if q.watcher.Cancelled() {
		return endCancelled(q.inst, q.logger, q.msg)
	}
	id := newMessageID()
	err := saveAnswer(q.inst, q.conversationID, q.msg.ID, ChatMessage{
		ID:        id,
		Role:      AssistantRole,
		Content:   answer.Content,
		Reasoning: answer.Reasoning,
		Sources:   answer.Sources,
		Action:    action,
		CreatedAt: time.Now().UTC(),
	})
	if action != nil {
		publishAction(q.inst, q.msg.ID, id, action)
	}
	publishDone(q.inst, q.msg.ID)
	return err
}

// fail ends the query on an error, told to the client, unless it comes from
// the user cancelling it.
func (q *chatQuery) fail(err error) error {
	if q.watcher.Cancelled() {
		return endCancelled(q.inst, q.logger, q.msg)
	}
	publishError(q.inst, q.msg.ID, err)
	return err
}

// ragPayload is the body of a query to openRAG for an answer from the
// documents.
func ragPayload(inst *instance.Instance, messages []ragMessage, stream bool, metadata map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"model":       fmt.Sprintf("ragondin-%s", inst.Domain),
		"messages":    messages,
		"stream":      stream,
		"metadata":    metadata,
		"temperature": Temperature,
		"top_p":       TopP,
		"logprobs":    LogProbs,
	}
}

// ragAnswer is the answer of openRAG to a query: its content, the reasoning
// of the LLM, its sources, and the tools it calls instead of answering.
type ragAnswer struct {
	Content   string
	Reasoning string
	Sources   []Source
	ToolCalls []toolCall
}

// askRAG asks openRAG for an answer, published on the realtime as it comes.
func (q *chatQuery) askRAG(ctx context.Context, body []byte) (ragAnswer, error) {
	res, err := q.postCompletion(ctx, body)
	if err != nil {
		return ragAnswer{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return ragAnswer{}, fmt.Errorf("POST status code: %d", res.StatusCode)
	}
	if q.stream {
		return handleStreamResponse(q.inst, q.msg, res.Body)
	}
	return handleNonStreamResponse(q.inst, q.msg, res.Body)
}

// postCompletion asks openRAG for a completion. An instance that has never
// chatted nor indexed a file has no partition on openRAG: it is created on a
// 404, and the completion asked again.
func (q *chatQuery) postCompletion(ctx context.Context, body []byte) (*http.Response, error) {
	inst := q.inst
	res, err := CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
	if err != nil || res.StatusCode != http.StatusNotFound {
		return res, err
	}
	res.Body.Close()
	checkRes, err := CallRAGQueryContext(ctx, inst, http.MethodGet, nil, fmt.Sprintf("/partition/%s", inst.Domain), echo.MIMEApplicationJSON)
	if err != nil {
		return nil, err
	}
	checkRes.Body.Close()
	if checkRes.StatusCode != http.StatusNotFound {
		return nil, fmt.Errorf("POST status code: %d", http.StatusNotFound)
	}
	q.logger.Warnf("RAG partition not found, attempting creation")
	createRAGPartition(inst.RAGServer(), inst.Domain, q.logger)
	return CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
}

func newMessageID() string {
	uuidv7, _ := uuid.NewV7()
	return uuidv7.String()
}

// saveAnswer saves the answer after the message it answers. The conversation
// is read again: the client may have written the outcome of an action on it
// since the query started, and may still do so before the answer is saved.
func saveAnswer(inst *instance.Instance, conversationID, msgID string, answer ChatMessage) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var chat ChatConversation
		if err = couchdb.GetDoc(inst, consts.ChatConversations, conversationID, &chat); err != nil {
			return err
		}
		chat.Messages = insertAfter(chat.Messages, msgID, answer)
		if err = updateConversation(inst, &chat); !couchdb.IsConflictError(err) {
			return err
		}
	}
	return err
}

// updateConversation is a variable so that tests can write the conversation
// while the stack saves a message or an answer.
var updateConversation = func(inst *instance.Instance, chat *ChatConversation) error {
	return couchdb.UpdateDoc(inst, chat)
}

// insertAfter inserts the answer after the message it answers, or at the end
// when the message is not in the conversation.
func insertAfter(messages []ChatMessage, msgID string, answer ChatMessage) []ChatMessage {
	i := len(messages)
	for j, msg := range messages {
		if msg.ID == msgID {
			i = j + 1
			break
		}
	}
	out := make([]ChatMessage, 0, len(messages)+1)
	out = append(out, messages[:i]...)
	out = append(out, answer)
	return append(out, messages[i:]...)
}

// endCancelled ends a query the user has cancelled: the client has stopped
// listening, the others are told it is over, and nothing is saved.
func endCancelled(inst *instance.Instance, logger logger.Logger, msg ChatMessage) error {
	logger.Infof("chat query cancelled by the user")
	publishDone(inst, msg.ID)
	return nil
}

// publishDelta sends a chunk of the answer (`delta`) or of the model's
// reasoning (`reasoning`).
func publishDelta(inst *instance.Instance, msgID string, object string, content string, position int) {
	doc := couchdb.JSONDoc{
		Type: consts.ChatEvents,
		M: map[string]interface{}{
			"_id":      msgID,
			"object":   object,
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

// handleStreamResponse publishes the answer streamed by openRAG, and returns
// it with its sources and the tool calls of the LLM. The `done` event is
// left to the caller, which may send an action before it.
func handleStreamResponse(inst *instance.Instance, msg ChatMessage, body io.Reader) (ragAnswer, error) {
	position := 0
	reasoningPosition := 0
	var completion, reasoning string
	var sources []Source
	var calls []toolCall
	var sseErr error

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
				// openRAG may give the sources with the last chunk only
				if last, err := getSources(event); sources == nil && (completion != "" || len(calls) > 0) && err == nil && last != nil {
					sources = last
					publishSources(inst, msg.ID, sources)
				}
				return
			} else if delta, ok := choice["delta"].(map[string]interface{}); ok {
				if chunk, _ := delta["reasoning_content"].(string); chunk != "" {
					publishDelta(inst, msg.ID, "reasoning", chunk, reasoningPosition)
					reasoning += chunk
					reasoningPosition++
				}
				calls = addToolCallDeltas(calls, delta["tool_calls"])
				// The content is progressively received through a delta stream.
				// A stream opens with an empty delta: nothing to publish.
				content, ok := delta["content"].(string)
				if !ok || content == "" {
					return
				}
				publishDelta(inst, msg.ID, "delta", content, position)
				completion += content
				position++

				if sources == nil {
					// Sources are included in all delta messages, but should be sent once
					sources, sseErr = getSources(event)
					if sseErr != nil {
						return
					}
					if sources != nil {
						publishSources(inst, msg.ID, sources)
					}
				}
			}
		}
	})

	if err != nil {
		return ragAnswer{}, err
	}
	if sseErr != nil {
		return ragAnswer{}, sseErr
	}
	return ragAnswer{Content: completion, Reasoning: reasoning, Sources: sources, ToolCalls: calls}, nil
}

// addToolCallDeltas adds the tool_calls of a delta to the calls, as the
// OpenAI API streams them: each has the index of its call, its id and name
// in its first part, and a piece of its arguments.
func addToolCallDeltas(calls []toolCall, raw interface{}) []toolCall {
	parts, _ := raw.([]interface{})
	for _, p := range parts {
		part, _ := p.(map[string]interface{})
		index := len(calls)
		if i, ok := part["index"].(float64); ok && i >= 0 {
			index = int(i)
		}
		for len(calls) <= index {
			calls = append(calls, toolCall{})
		}
		call := &calls[index]
		if id, _ := part["id"].(string); id != "" {
			call.ID = id
		}
		if fn, ok := part["function"].(map[string]interface{}); ok {
			if name, _ := fn["name"].(string); name != "" && call.Function.Name == "" {
				call.Function.Name = name
			}
			args, _ := fn["arguments"].(string)
			call.Function.Arguments += args
		}
	}
	return calls
}

// handleNonStreamResponse is handleStreamResponse for an answer that openRAG
// sends in one piece.
func handleNonStreamResponse(inst *instance.Instance, msg ChatMessage, body io.Reader) (ragAnswer, error) {
	var event map[string]interface{}
	if err := json.NewDecoder(body).Decode(&event); err != nil {
		return ragAnswer{}, err
	}

	var completion string
	var calls []toolCall
	if choices, ok := event["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if message, ok := choice["message"].(map[string]interface{}); ok {
				completion, _ = message["content"].(string)
				if raw, err := json.Marshal(message["tool_calls"]); err == nil {
					_ = json.Unmarshal(raw, &calls)
				}
			}
		}
	}
	if completion == "" && len(calls) == 0 {
		return ragAnswer{}, errors.New("invalid RAG response: no completion content")
	}

	sources, err := getSources(event)
	if err != nil {
		return ragAnswer{}, err
	}

	if completion != "" {
		publishDelta(inst, msg.ID, "delta", completion, 0)
	}
	if sources != nil {
		publishSources(inst, msg.ID, sources)
	}
	return ragAnswer{Content: completion, Sources: sources, ToolCalls: calls}, nil
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
