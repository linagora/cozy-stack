package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/meet"
	"github.com/labstack/echo/v4"
)

// The chat router lets the assistant propose an action of a Twake app (write
// a note or a document, start a video meeting, ...) instead of a plain answer
// from the documents. The LLM behind openRAG is small: it is only asked to
// pick one intent in a closed list, then either to write the content of a
// note or a document, which is the answer itself, or to fill the few params
// of the other actions, with a JSON schema. The action is never run by the
// stack: the client shows it to the user, who edits and confirms it.

const (
	// searchIntent is the router's answer for a message that is not an
	// action: the normal answer from the documents.
	searchIntent = "search"
	// routerTimeout bounds the wait of the answer for the router decision.
	routerTimeout = 20 * time.Second
	// fillTimeout bounds the generation of the params.
	fillTimeout = 2 * time.Minute
	// searchTimeout bounds the search of documents relevant to a message.
	searchTimeout = 10 * time.Second
	// searchThreshold is the similarity a chunk must have to be relevant,
	// the one of openRAG's retrieval for the chat.
	searchThreshold = "0.6"
	// writingHistoryChars bounds the conversation given to write a content
	// or fill params: the context of a small LLM is short.
	writingHistoryChars = 12000
)

// ChatAction is an action proposed to the user, run by the client once the
// user has confirmed it.
type ChatAction struct {
	Name   string                 `json:"name"`
	Params map[string]interface{} `json:"params"`
	// Status and URL are written by the client once the user has handled
	// the action: "done", with the URL of what it created, or "cancelled".
	Status string `json:"status,omitempty"`
	URL    string `json:"url,omitempty"`
}

type actionParam struct {
	Name        string
	Description string
	// List is true for a list of strings, false for a string.
	List     bool
	Required bool
	// Recipient marks the people an action reaches: a value is kept only
	// when the user wrote it, so that a document cannot add a recipient.
	Recipient bool
}

type routeExample struct {
	Message        string
	NeedsDocuments bool
}

// writing describes how to write the content of a note or a document.
type writing struct {
	// Kind is what is written, for the prompt: "note" or "document".
	Kind         string
	Instructions string
	MaxTokens    int
}

type actionSpec struct {
	Name string
	// Description tells the router when to pick the action.
	Description string
	Examples    []routeExample
	// Writing is set for the actions whose content is written as the
	// answer, and saved by the action: the only param is its title.
	Writing *writing
	// Grounded marks the actions whose content may come from the user's
	// documents: when the router says they do not need them, the documents
	// are searched anyway, as a small LLM would otherwise make up the facts
	// of the user's organization.
	Grounded bool
	// Instructions tell the LLM how to fill the params of the others.
	Instructions string
	Params       []actionParam
	// available, when set, tells whether the instance can run the action.
	available func(inst *instance.Instance) bool
}

var chatActions = []actionSpec{
	{
		Name: "create_note",
		Description: "write a note in the user's Notes app: a summary of the conversation, a list of tasks, " +
			"a short text on a subject.",
		Examples: []routeExample{
			{Message: "Fais-en une note", NeedsDocuments: false},
			{Message: "Résume cette conversation dans une note", NeedsDocuments: false},
			{Message: "Crée une note qui résume les documents du projet Atlas", NeedsDocuments: true},
			{Message: "Fais une note avec les points à retenir de la politique de télétravail", NeedsDocuments: true},
		},
		Writing: &writing{
			Kind: "note",
			Instructions: "A note is concise: after the title, organize the content with short \"##\" sections or lists. " +
				"Keep what matters, nothing more.",
			MaxTokens: 1024,
		},
		Grounded: true,
	},
	{
		Name: "create_document",
		Description: "write a text document (OnlyOffice, like Word) saved in the user's files: a report, " +
			"a meeting summary, an article, a document on a subject.",
		Examples: []routeExample{
			{Message: "Écris un document sur les bonnes pratiques du télétravail", NeedsDocuments: false},
			{Message: "Fais un compte rendu de cette conversation dans un document", NeedsDocuments: false},
			{Message: "Write a report on the progress of the Atlas project from my files", NeedsDocuments: true},
		},
		Writing: &writing{
			Kind: "document",
			Instructions: "A document is complete: after the title, write an introduction, then \"##\" sections " +
				"developed in full sentences, with lists where they help, and a short conclusion.",
			MaxTokens: 2048,
		},
		Grounded: true,
	},
	{
		Name:        "draft_email",
		Description: "prepare an email for the user to review and send: a reply, a message to someone, a summary to share.",
		Examples: []routeExample{
			{Message: "Rédige un mail à Paul pour lui envoyer ce résumé", NeedsDocuments: false},
			{Message: "Write an email to the team about the delivery delay", NeedsDocuments: false},
		},
		Instructions: "The body is plain text, ready to send, with a greeting and a sign-off, without a signature name. " +
			"When the user asks to send a summary or an answer of the conversation, the body contains it in full.",
		Params: []actionParam{
			{Name: "to", Description: "the recipients, as the user named them (names or email addresses)", List: true, Recipient: true},
			{Name: "subject", Description: "the subject of the email", Required: true},
			{Name: "body", Description: "the text of the email", Required: true},
		},
		Grounded: true,
	},
	{
		Name:        "start_meeting",
		Description: "start a video meeting (visio, video call) now, or create the link of a meeting room to share.",
		Examples: []routeExample{
			{Message: "Lance une visio avec Marie", NeedsDocuments: false},
			{Message: "Start a video call", NeedsDocuments: false},
		},
		Instructions: "The title is a short noun phrase starting with a capital letter, like \"Préparation du comité Atlas\", " +
			"\"\" when the user did not say what the meeting is about.",
		Params: []actionParam{
			{Name: "title", Description: "a short title saying what the meeting is about"},
			{Name: "attendees", Description: "the people to invite, as the user named them", List: true, Recipient: true},
		},
		available: meet.IsConfigured,
	},
}

// supportedActions returns the actions of the catalog the client declared it
// can run and the instance can run, in the order of the catalog. Unknown
// names are ignored.
func supportedActions(inst *instance.Instance, names []string) []actionSpec {
	var actions []actionSpec
	for _, spec := range chatActions {
		if spec.available != nil && !spec.available(inst) {
			continue
		}
		for _, name := range names {
			if name == spec.Name {
				actions = append(actions, spec)
				break
			}
		}
	}
	return actions
}

func actionSpecFor(actions []actionSpec, name string) actionSpec {
	for _, spec := range actions {
		if spec.Name == name {
			return spec
		}
	}
	return actionSpec{}
}

type routeDecision struct {
	Intent         string `json:"intent"`
	NeedsDocuments bool   `json:"needs_documents"`
}

// action is the name of the action the router picked, "" for a search.
func (d routeDecision) action() string {
	if d.Intent == searchIntent {
		return ""
	}
	return d.Intent
}

// keepsAnswer tells whether the answer from the documents started with the
// router is the one to give. A note or a document is written with its own
// instructions, and the params of an action that does not need the
// documents are filled from the conversation: the answer is not needed. An
// action that needs the documents comes after the answer it is made from.
func keepsAnswer(actions []actionSpec, d routeDecision) bool {
	spec := actionSpecFor(actions, d.action())
	return spec.Name == "" || (spec.Writing == nil && d.NeedsDocuments)
}

func routerPrompt(actions []actionSpec) string {
	var b strings.Builder
	b.WriteString("You are the router of the Twake assistant. Read the last user message of the conversation and decide what to do with it.\n\n")
	b.WriteString("Answer with a JSON object: {\"intent\": \"...\", \"needs_documents\": true or false}\n\n")
	b.WriteString("intent is one of:\n")
	b.WriteString("- \"search\": answer the message from the user's documents. This is the default: questions, requests for information, ")
	b.WriteString("summaries or explanations given in the chat, small talk, and questions about HOW to do something.\n")
	for _, spec := range actions {
		fmt.Fprintf(&b, "- %q: %s\n", spec.Name, spec.Description)
	}
	b.WriteString("Choose an action only when the user explicitly asks the assistant to do it now. When in doubt, choose \"search\".\n")
	if actionSpecFor(actions, "create_note").Name != "" && actionSpecFor(actions, "create_document").Name != "" {
		b.WriteString("A note is short and goes to Notes. Choose \"create_document\" when the user asks for a document, a doc, a report, Word or OnlyOffice.\n")
	}
	b.WriteString("\nneeds_documents is true when the action may need information from the user's documents: anything about their organization, ")
	b.WriteString("its rules and policies, their projects, clients, colleagues, meetings or files, even when the user does not say \"from my files\". ")
	b.WriteString("It is false for \"search\", for an action made from the conversation or from the message itself, ")
	b.WriteString("and for a general subject that does not depend on the user's organization, like general best practices.\n\n")
	b.WriteString("Examples:\n")
	examples := []struct {
		routeExample
		intent string
	}{
		{routeExample{Message: "What does our contract with Acme say about penalties?"}, searchIntent},
		{routeExample{Message: "Comment je partage un dossier ?"}, searchIntent},
	}
	for _, spec := range actions {
		for _, ex := range spec.Examples {
			examples = append(examples, struct {
				routeExample
				intent string
			}{ex, spec.Name})
		}
	}
	for _, ex := range examples {
		decision, _ := json.Marshal(routeDecision{Intent: ex.intent, NeedsDocuments: ex.NeedsDocuments})
		fmt.Fprintf(&b, "User: %q\n%s\n", ex.Message, decision)
	}
	return b.String()
}

// transcript writes the conversation for the router and the LLM: the
// assistant prompt is left out, and the long messages are cut, the context
// of a small LLM is short. The last user message is kept whole up to
// maxChars, then the previous turns are taken from the most recent while
// they fit in maxTotal characters.
func transcript(messages []ragMessage, maxChars, maxTotal int) string {
	var turns []ragMessage
	for _, msg := range messages {
		if msg.Role == UserRole || msg.Role == AssistantRole {
			turns = append(turns, msg)
		}
	}
	if len(turns) == 0 {
		return ""
	}
	last := truncate(turns[len(turns)-1].Content, maxChars)
	budget := maxTotal - len([]rune(last))
	var previous []string
	for i := len(turns) - 2; i >= 0; i-- {
		line := fmt.Sprintf("%s: %s\n", turns[i].Role, truncate(turns[i].Content, maxChars))
		budget -= len([]rune(line))
		if budget < 0 {
			break
		}
		previous = append([]string{line}, previous...)
	}
	var b strings.Builder
	if len(previous) > 0 {
		b.WriteString("Conversation so far:\n")
		b.WriteString(strings.Join(previous, ""))
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Last user message:\n%s\n", last)
	return b.String()
}

func truncate(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars]) + " [...]"
}

// routeQuery asks the LLM whether the last message of the conversation is a
// search or one of the actions. On any failure, it answers a search.
func routeQuery(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []actionSpec, override map[string]interface{}) (routeDecision, error) {
	search := routeDecision{Intent: searchIntent}
	ctx, cancel := context.WithTimeout(ctx, routerTimeout)
	defer cancel()

	intents := []string{searchIntent}
	for _, spec := range actions {
		intents = append(intents, spec.Name)
	}
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"intent":          map[string]interface{}{"type": "string", "enum": intents},
			"needs_documents": map[string]interface{}{"type": "boolean"},
		},
		"required":             []string{"intent", "needs_documents"},
		"additionalProperties": false,
	}
	var decision routeDecision
	err := completeJSON(ctx, inst, routerPrompt(actions), transcript(messages, 600, 4000), "route", schema, 32, override, &decision)
	if err != nil {
		return search, err
	}
	for _, intent := range intents {
		if decision.Intent == intent {
			if intent == searchIntent {
				decision.NeedsDocuments = false
			}
			return decision, nil
		}
	}
	return search, fmt.Errorf("unknown intent %q", decision.Intent)
}

// checkDocuments makes sure that an action whose content may come from the
// user's documents uses them when they are relevant to the message, whatever
// the router said: the decision does not rest on the LLM alone.
func checkDocuments(ctx context.Context, inst *instance.Instance, actions []actionSpec, d routeDecision, message, workspace string) (routeDecision, error) {
	spec := actionSpecFor(actions, d.action())
	if !spec.Grounded || d.NeedsDocuments {
		return d, nil
	}
	relevant, err := hasRelevantDocuments(ctx, inst, message, workspace)
	if err != nil || relevant {
		// Without an answer from the search, the documents are used: an
		// answer saying they do not cover the subject is better than a
		// made up one.
		d.NeedsDocuments = true
	}
	return d, err
}

// hasRelevantDocuments asks openRAG whether the user's documents, in the
// workspace of the assistant if any, have chunks relevant to the text.
func hasRelevantDocuments(ctx context.Context, inst *instance.Instance, text, workspace string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, searchTimeout)
	defer cancel()
	server := inst.RAGServer()
	if server.URL == "" {
		return false, errors.New("no RAG server configured")
	}
	query := url.Values{"text": {text}, "top_k": {"3"}, "similarity_threshold": {searchThreshold}}
	if workspace != "" {
		query.Set("workspace", workspace)
	}
	u := strings.TrimSuffix(server.URL, "/") + "/search/partition/" + url.PathEscape(inst.Domain) + "?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+server.APIKey)
	res, err := ragHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return false, fmt.Errorf("GET search status code: %d", res.StatusCode)
	}
	var found struct {
		Documents []json.RawMessage `json:"documents"`
	}
	if err := json.NewDecoder(res.Body).Decode(&found); err != nil {
		return false, err
	}
	return len(found.Documents) > 0, nil
}

// writingPrompt is the instructions to write the content of a note or a
// document, from the user's documents or from the conversation and what the
// LLM knows of a general subject.
func writingPrompt(w *writing, fromDocuments bool, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user asks you to write a %s, which will be saved in their files. ", w.Kind)
	fmt.Fprintf(&b, "Today is %s.\n\n", now.Format("Monday, January 2, 2006"))
	fmt.Fprintf(&b, "Write the %s itself, in full:\n", w.Kind)
	b.WriteString("- In Markdown, in the language of the user.\n")
	b.WriteString("- Start with one line \"# \" followed by its title, nothing before it.\n")
	fmt.Fprintf(&b, "- %s\n", w.Instructions)
	b.WriteString("- Use only headings, paragraphs, \"- \" and \"1. \" lists, **bold** and *italic*: no table, no code block, no link.\n")
	b.WriteString("- No introduction like \"Here is\", no comment after the content, no question to the user.\n")
	b.WriteString("- When the user asks to summarize or use the conversation, use only the conversation: ")
	b.WriteString("its key points, decisions, figures and open questions, in a logical order.\n")
	if fromDocuments {
		b.WriteString("- When the user gives a subject, write about it from the user's documents only.\n")
		b.WriteString("- Never invent facts, names or figures about the user, their work or their documents.\n")
	} else {
		b.WriteString("- When the user gives a general subject, write about it from what you know.\n")
		b.WriteString("- You do not know the user's organization: its rules, projects, clients and people. ")
		b.WriteString("When the content needs them and they are not in the conversation, do not invent them: ")
		b.WriteString("answer in one sentence, without a title, that you need to search the user's documents for it.\n")
	}
	return b.String()
}

// writingRequest is the body of the call that writes a note or a document
// without the documents: the LLM behind openRAG is asked directly, with the
// conversation as material.
func writingRequest(w *writing, messages []ragMessage, stream bool, override map[string]interface{}, now time.Time) ([]byte, error) {
	payload := map[string]interface{}{
		"messages": []ragMessage{
			{Role: SystemRole, Content: writingPrompt(w, false, now)},
			{Role: UserRole, Content: transcript(messages, 4000, writingHistoryChars)},
		},
		"stream":      stream,
		"temperature": Temperature,
		"max_tokens":  w.MaxTokens,
	}
	if override != nil {
		payload["metadata"] = map[string]interface{}{"llm_override": override}
	}
	return json.Marshal(payload)
}

// withWritingInstructions adds the writing instructions to the messages of a
// query to openRAG, after the prompt of the assistant: openRAG takes the
// leading system messages as custom instructions.
func withWritingInstructions(messages []ragMessage, w *writing, now time.Time) []ragMessage {
	instructions := ragMessage{Role: SystemRole, Content: writingPrompt(w, true, now)}
	i := 0
	for i < len(messages) && messages[i].Role == SystemRole {
		i++
	}
	out := make([]ragMessage, 0, len(messages)+1)
	out = append(out, messages[:i]...)
	out = append(out, instructions)
	return append(out, messages[i:]...)
}

// contentTitle returns the title of a written note or document: its first
// heading, which the writing instructions ask for. A content without it is
// not a note nor a document, like an answer saying the documents do not
// cover the subject, and the action is not proposed.
func contentTitle(content string) string {
	lines := 0
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			return strings.Trim(strings.TrimLeft(line, "#"), " *_")
		}
		lines++
		if lines == 3 {
			break
		}
	}
	return ""
}

func fillPrompt(spec actionSpec, now time.Time) string {
	var b strings.Builder
	b.WriteString("You prepare an action of a Twake app that the user asked for. The user will review it before it runs.\n")
	fmt.Fprintf(&b, "Today is %s.\n\n", now.Format("Monday, January 2, 2006"))
	fmt.Fprintf(&b, "Action %q: %s\n%s\n\n", spec.Name, spec.Description, spec.Instructions)
	b.WriteString("Answer with a JSON object with these fields:\n")
	for _, param := range spec.Params {
		kind := "string"
		if param.List {
			kind = "list of strings"
		}
		required := ""
		if param.Required {
			required = ", required"
		}
		fmt.Fprintf(&b, "- %q (%s%s): %s\n", param.Name, kind, required, param.Description)
	}
	b.WriteString("Use \"\" (or [] for a list) for a field you cannot fill. ")
	b.WriteString("Never invent facts, names or email addresses: use only what is in the conversation. ")
	b.WriteString("Write in the language of the user.\n")
	return b.String()
}

func paramsSchema(spec actionSpec) map[string]interface{} {
	properties := map[string]interface{}{}
	required := make([]string, 0, len(spec.Params))
	for _, param := range spec.Params {
		if param.List {
			properties[param.Name] = map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}
		} else {
			properties[param.Name] = map[string]interface{}{"type": "string"}
		}
		// Every field is asked for, "" when unknown: a small model follows a
		// fixed shape better than optional fields.
		required = append(required, param.Name)
	}
	return map[string]interface{}{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

// fillAction asks the LLM for the params of the action. answer is the answer
// from the documents when the action needs it, "" otherwise.
func fillAction(ctx context.Context, inst *instance.Instance, spec actionSpec, messages []ragMessage, answer string, override map[string]interface{}, now time.Time) (*ChatAction, error) {
	ctx, cancel := context.WithTimeout(ctx, fillTimeout)
	defer cancel()

	user := transcript(messages, 4000, writingHistoryChars)
	if answer != "" {
		user += "\nWhat the assistant found in the user's documents:\n" + truncate(answer, 4000) + "\n"
	}
	var params map[string]interface{}
	if err := completeJSON(ctx, inst, fillPrompt(spec, now), user, spec.Name, paramsSchema(spec), 0, override, &params); err != nil {
		return nil, err
	}
	params, err := checkParams(spec, params, userText(messages))
	if err != nil {
		return nil, err
	}
	return &ChatAction{Name: spec.Name, Params: params}, nil
}

// userText is what the user wrote in the conversation, the only place the
// recipients of an action can come from.
func userText(messages []ragMessage) string {
	var b strings.Builder
	for _, msg := range messages {
		if msg.Role == UserRole {
			b.WriteString(msg.Content)
			b.WriteString("\n")
		}
	}
	return strings.ToLower(b.String())
}

// checkParams keeps the params of the action with the expected types, drops
// the recipients the user did not write, and checks the required ones.
func checkParams(spec actionSpec, raw map[string]interface{}, written string) (map[string]interface{}, error) {
	params := map[string]interface{}{}
	for _, param := range spec.Params {
		if param.List {
			list := []string{}
			values, _ := raw[param.Name].([]interface{})
			for _, value := range values {
				s, ok := value.(string)
				s = strings.TrimSpace(s)
				if !ok || s == "" {
					continue
				}
				if param.Recipient && !strings.Contains(written, strings.ToLower(s)) {
					continue
				}
				list = append(list, s)
			}
			if param.Required && len(list) == 0 {
				return nil, fmt.Errorf("missing %s", param.Name)
			}
			params[param.Name] = list
			continue
		}
		s, _ := raw[param.Name].(string)
		s = strings.TrimSpace(s)
		if param.Recipient && !strings.Contains(written, strings.ToLower(s)) {
			s = ""
		}
		if param.Required && s == "" {
			return nil, fmt.Errorf("missing %s", param.Name)
		}
		params[param.Name] = s
	}
	return params, nil
}

// describeAction is how a proposed action appears in the history sent to the
// LLM, so that a follow-up message ("change the title") has its context.
func describeAction(action *ChatAction) string {
	params, _ := json.Marshal(action.Params)
	return fmt.Sprintf("(Proposed to the user: %s %s)", action.Name, truncate(string(params), 1000))
}

// completeJSON asks the LLM behind openRAG, without retrieval, for a JSON
// object matching the schema, and decodes it in out.
func completeJSON(ctx context.Context, inst *instance.Instance, system, user, name string, schema map[string]interface{}, maxTokens int, override map[string]interface{}, out interface{}) error {
	// Without a model, openRAG sends the messages to its LLM as they are,
	// with no retrieval and no answer prompt.
	payload := map[string]interface{}{
		"messages": []ragMessage{
			{Role: SystemRole, Content: system},
			{Role: UserRole, Content: user},
		},
		"stream":      false,
		"temperature": 0,
		"response_format": map[string]interface{}{
			"type": "json_schema",
			"json_schema": map[string]interface{}{
				"name":   name,
				"schema": schema,
				"strict": true,
			},
		},
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}
	if override != nil {
		payload["metadata"] = map[string]interface{}{"llm_override": override}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	res, err := CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return fmt.Errorf("POST status code: %d", res.StatusCode)
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&completion); err != nil {
		return err
	}
	if len(completion.Choices) == 0 {
		return errors.New("no completion")
	}
	return decodeJSONObject(completion.Choices[0].Message.Content, out)
}

// decodeJSONObject decodes the JSON object of an LLM answer, which may be
// wrapped in a Markdown code block or in some text when the LLM does not
// enforce the schema.
func decodeJSONObject(content string, out interface{}) error {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return fmt.Errorf("no JSON object in %q", truncate(content, 200))
	}
	return json.Unmarshal([]byte(content[start:end+1]), out)
}
