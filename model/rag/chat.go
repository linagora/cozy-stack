package rag

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

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
}

type ChatConversation struct {
	DocID        string                  `json:"_id"`
	DocRev       string                  `json:"_rev,omitempty"`
	Messages     []ChatMessage           `json:"messages"`
	CozyMetadata *metadata.CozyMetadata  `json:"cozyMetadata"`
	Rels         jsonapi.RelationshipMap `json:"relationships,omitempty"`
}

type ChatMessage struct {
	ID            string    `json:"id"`
	Role          string    `json:"role"`
	Content       string    `json:"content"`
	Sources       []Source  `json:"sources,omitempty"`
	AttachmentIDs []string  `json:"attachmentIDs,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

const (
	UserRole      = "user"
	AssistantRole = "assistant"
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
	if c.Rels != nil {
		cloned.Rels = make(jsonapi.RelationshipMap, len(c.Rels))
		for k, v := range c.Rels {
			cloned.Rels[k] = v
		}
	}
	return &cloned
}
func (c *ChatConversation) Included() []jsonapi.Object             { return nil }
func (c *ChatConversation) Relationships() jsonapi.RelationshipMap { return c.Rels }
func (c *ChatConversation) Links() *jsonapi.LinksList              { return nil }

var _ jsonapi.Object = (*ChatConversation)(nil)

type QueryMessage struct {
	Task          string   `json:"task"`
	DocID         string   `json:"doc_id"`
	Stream        bool     `json:"stream"`
	WebSearch     bool     `json:"websearch"`
	AttachmentIDs []string `json:"attachmentIDs,omitempty"`
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

func getSources(event map[string]interface{}) ([]Source, error) {
	extraStr, ok := event["extra"].(string)
	if !ok {
		return nil, nil
	}

	var extra map[string]interface{}
	err := json.Unmarshal([]byte(extraStr), &extra)
	if err != nil {
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
			subject, _ := src["email.subject"].(string)
			datetime, _ := src["datetime"].(string)
			emailPreview, _ := src["email.preview"].(string)
			relationshipID, _ := src["relationship_id"].(string)
			parentID, _ := src["parent_id"].(string)
			doctype, _ := src["doctype"].(string)
			fileID, _ := src["file_id"].(string)
			fileName, _ := src["filename"].(string)
			page := 0
			if p, ok := src["page"].(float64); ok {
				page = int(p)
			}
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
	// The account's "login" field stores the LLM model name, e.g. "Mistral-Small-3.2-24B-Instruct-2506"
	// While "password" stores the API key
	var model, apiKey string
	if acc.Basic != nil {
		if acc.Basic.EncryptedCredentials != "" {
			model, apiKey, _ = account.DecryptCredentials(acc.Basic.EncryptedCredentials)
		} else {
			model, apiKey = acc.Basic.Login, acc.Basic.Password
		}
	}
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

func Query(inst *instance.Instance, logger logger.Logger, query QueryMessage) error {
	var chat ChatConversation
	err := couchdb.GetDoc(inst, consts.ChatConversations, query.DocID, &chat)
	if err != nil {
		return err
	}

	type RAGMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	chatHistory := make([]RAGMessage, 0, len(chat.Messages))
	for _, msg := range chat.Messages {
		chatHistory = append(chatHistory, RAGMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
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
	if dirID := assistant.knowledgeBaseDirID(logger); dirID != "" {
		if err := ensureWorkspace(inst, logger, dirID); err != nil {
			logger.Warnf("cannot ensure RAG workspace %s: %s", dirID, err)
			// A folder-scoped assistant must never answer from the whole
			// instance: surface the error to the client and stop.
			publishError(inst, msg.ID, err)
			return err
		}
		metadata["workspace"] = dirID
	}
	payload := map[string]interface{}{
		"model":       fmt.Sprintf("ragondin-%s", inst.Domain),
		"messages":    chatHistory,
		"stream":      query.Stream,
		"metadata":    metadata,
		"temperature": Temperature,
		"top_p":       TopP,
		"logprobs":    LogProbs,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := CallRAGQuery(inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
	if err != nil {
		publishError(inst, msg.ID, err)
		return err
	}
	if res.StatusCode == http.StatusNotFound {
		res.Body.Close()
		checkRes, err := CallRAGQuery(inst, http.MethodGet, nil, fmt.Sprintf("/partition/%s", inst.Domain), echo.MIMEApplicationJSON)
		if err != nil {
			publishError(inst, msg.ID, err)
			return err
		}
		checkRes.Body.Close()
		if checkRes.StatusCode == http.StatusNotFound {
			logger.Warnf("RAG partition not found, attempting creation")
			createRAGPartition(inst.RAGServer(), inst.Domain, logger)
			res, err = CallRAGQuery(inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
			if err != nil {
				publishError(inst, msg.ID, err)
				return err
			}
		}
	}
	defer res.Body.Close()

	if res.StatusCode != 200 {
		ragErr := fmt.Errorf("POST status code: %d", res.StatusCode)
		publishError(inst, msg.ID, ragErr)
		return ragErr
	}
	var completion string
	var sources []Source

	if query.Stream {
		completion, sources, err = handleStreamResponse(inst, msg, res.Body)
	} else {
		completion, sources, err = handleNonStreamResponse(inst, msg, res.Body)
	}
	if err != nil {
		// Send error event to client
		publishError(inst, msg.ID, err)
		return err
	}

	uuidv7, _ := uuid.NewV7()
	answer := ChatMessage{
		ID:        uuidv7.String(),
		Role:      AssistantRole,
		Content:   completion,
		Sources:   sources,
		CreatedAt: time.Now().UTC(),
	}
	chat.Messages = append(chat.Messages, answer)
	return couchdb.UpdateDoc(inst, &chat)
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
	go realtime.GetHub().Publish(inst, realtime.EventCreate, &doc, nil)
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

func handleStreamResponse(inst *instance.Instance, msg ChatMessage, body io.Reader) (string, []Source, error) {
	position := 0
	var completion string
	var sources []Source
	var sseErr error

	// Realtime messages are sent to the client during the response stream
	// When the stream is finished, the whole answer is saved in the CouchDB document
	err := foreachSSE(body, func(event map[string]interface{}) {
		// See https://platform.openai.com/docs/api-reference/chat-streaming/streaming#chat-streaming
		if event["object"] == "chat.completion.chunk" {
			choices, ok := event["choices"].([]interface{})
			if !ok || len(choices) < 1 {
				return
			}
			choice := choices[0].(map[string]interface{}) // Only one choice is possible for now

			if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
				go publishDone(inst, msg.ID)
			} else if delta, ok := choice["delta"].(map[string]interface{}); ok {
				// The content is progressively reveived through a delta stream
				content, ok := delta["content"].(string)
				if !ok {
					return
				}
				go publishDelta(inst, msg.ID, content, position)
				completion += content
				position++

				if event["extra"].(string) != "" && sources == nil {
					// Sources are included in all delta messages, but should be sent once
					sources, sseErr = getSources(event)
					if sseErr != nil {
						return
					}
					if sources != nil {
						go publishSources(inst, msg.ID, sources)
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

func handleNonStreamResponse(inst *instance.Instance, msg ChatMessage, body io.Reader) (string, []Source, error) {
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

	publishDelta(inst, msg.ID, completion, 0)
	if sources != nil {
		publishSources(inst, msg.ID, sources)
	}
	publishDone(inst, msg.ID)

	return completion, sources, nil
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
	},
}

// callRAG is the instance-free part of CallRAGQuery, split out so the openRAG
// HTTP mechanics can be unit-tested against an httptest server.
func callRAG(server config.RAGServer, method string, payload []byte, path string, contentType string) (*http.Response, error) {
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
	req, err := http.NewRequest(method, u.String(), body)
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
