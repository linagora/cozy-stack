package rag

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/labstack/echo/v4"
)

// The chat router lets the assistant propose an action instead of a plain
// answer from the documents. The stack knows nothing of the actions but what
// the client sends with the message: their names and descriptions, and how
// to produce them. The LLM behind openRAG is small: it is only asked to pick
// one intent in a closed list, then either to write the content of the
// action, which is the answer itself, or to fill the params of its JSON
// schema. The action is never run by the stack: the client shows it to the
// user, who edits and confirms it.

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
	// searchThreshold is passed to the search of openRAG, the threshold of
	// its retrieval for the chat. It only filters the semantic results: in
	// hybrid search, the keyword results are kept whatever the threshold.
	searchThreshold = "0.6"
	// writingHistoryChars bounds the conversation given to write a content
	// or fill params: the context of a small LLM is short.
	writingHistoryChars = 12000
	// defaultContentTokens bounds a written content without max_tokens.
	defaultContentTokens = 1024
)

// The limits of the action definitions: they go into the prompts of a small
// LLM.
const (
	maxActions          = 10
	maxExamples         = 5
	maxParams           = 10
	maxDescriptionChars = 1000
	maxShortTextChars   = 300
	maxContentTokens    = 4096
)

var actionNameRegexp = regexp.MustCompile(`^[a-z][a-z0-9_]{0,39}$`)
var paramNameRegexp = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,39}$`)

// ActionDefinition is an action the client can run, as it describes it.
type ActionDefinition struct {
	Name string `json:"name"`
	// Description tells the router what the action does, and when to pick it.
	Description string          `json:"description"`
	Examples    []ActionExample `json:"examples,omitempty"`
	// Content, when set, makes the stack write the content of the action as
	// the answer, in Markdown starting with its "# " title line. The action
	// then gets the title as its only param.
	Content *ActionContent `json:"content,omitempty"`
	// Parameters is the JSON schema of the params the LLM fills, for an
	// action without content, like the parameters of a tool for function
	// calling.
	Parameters *ParametersSchema `json:"parameters,omitempty"`
	// Instructions tell the LLM how to write the content or fill the params.
	Instructions string `json:"instructions,omitempty"`
}

// ActionExample is a message for which the router picks the action.
type ActionExample struct {
	Message        string `json:"message"`
	NeedsDocuments bool   `json:"needs_documents"`
}

// ActionContent describes the content written for an action.
type ActionContent struct {
	MaxTokens int `json:"max_tokens,omitempty"`
}

// ParametersSchema is the JSON schema of the params of an action: an object whose
// properties are strings or lists of strings.
type ParametersSchema struct {
	Type       string                 `json:"type"`
	Properties map[string]ParamSchema `json:"properties"`
	Required   []string               `json:"required,omitempty"`
}

// ParamSchema is the JSON schema of a param.
type ParamSchema struct {
	Type        string       `json:"type"`
	Items       *ItemsSchema `json:"items,omitempty"`
	Description string       `json:"description,omitempty"`
	// UserWritten keeps only the values the user wrote in the conversation,
	// so that the content of a document cannot add one, like a recipient.
	UserWritten bool `json:"x-user-written,omitempty"`
}

// ItemsSchema is the JSON schema of the items of a list param.
type ItemsSchema struct {
	Type string `json:"type"`
}

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

// ValidateActions checks the action definitions a client sends.
func ValidateActions(actions []ActionDefinition) error {
	if len(actions) > maxActions {
		return fmt.Errorf("too many actions: %d, at most %d", len(actions), maxActions)
	}
	names := map[string]bool{}
	for _, a := range actions {
		if !actionNameRegexp.MatchString(a.Name) || a.Name == searchIntent {
			return fmt.Errorf("invalid action name %q", a.Name)
		}
		if names[a.Name] {
			return fmt.Errorf("duplicate action %q", a.Name)
		}
		names[a.Name] = true
		if err := a.validate(); err != nil {
			return fmt.Errorf("action %s: %w", a.Name, err)
		}
	}
	return nil
}

func (a ActionDefinition) validate() error {
	if strings.TrimSpace(a.Description) == "" || len([]rune(a.Description)) > maxDescriptionChars {
		return fmt.Errorf("the description must have 1 to %d characters", maxDescriptionChars)
	}
	if len([]rune(a.Instructions)) > maxDescriptionChars {
		return fmt.Errorf("the instructions must have at most %d characters", maxDescriptionChars)
	}
	if len(a.Examples) > maxExamples {
		return fmt.Errorf("at most %d examples", maxExamples)
	}
	for _, ex := range a.Examples {
		if strings.TrimSpace(ex.Message) == "" || len([]rune(ex.Message)) > maxShortTextChars {
			return fmt.Errorf("an example must have 1 to %d characters", maxShortTextChars)
		}
	}
	if (a.Content == nil) == (a.Parameters == nil) {
		return errors.New("an action has either a content or parameters")
	}
	if a.Content != nil && (a.Content.MaxTokens < 0 || a.Content.MaxTokens > maxContentTokens) {
		return fmt.Errorf("max_tokens must be at most %d", maxContentTokens)
	}
	if a.Parameters != nil {
		return a.Parameters.validate()
	}
	return nil
}

func (p ParametersSchema) validate() error {
	if p.Type != "object" {
		return errors.New("parameters must be an object schema")
	}
	if len(p.Properties) == 0 || len(p.Properties) > maxParams {
		return fmt.Errorf("parameters must have 1 to %d properties", maxParams)
	}
	for name, prop := range p.Properties {
		if !paramNameRegexp.MatchString(name) {
			return fmt.Errorf("invalid param name %q", name)
		}
		switch {
		case prop.Type == "string" && prop.Items == nil:
		case prop.Type == "array" && prop.Items != nil && prop.Items.Type == "string":
		default:
			return fmt.Errorf("param %s must be a string or a list of strings", name)
		}
		if len([]rune(prop.Description)) > maxShortTextChars {
			return fmt.Errorf("the description of param %s must have at most %d characters", name, maxShortTextChars)
		}
	}
	for _, name := range p.Required {
		if _, ok := p.Properties[name]; !ok {
			return fmt.Errorf("required param %q is not a property", name)
		}
	}
	return nil
}

// paramNames returns the names of the params, in a stable order.
func (p ParametersSchema) paramNames() []string {
	names := make([]string, 0, len(p.Properties))
	for name := range p.Properties {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (p ParametersSchema) isRequired(name string) bool {
	for _, r := range p.Required {
		if r == name {
			return true
		}
	}
	return false
}

func actionFor(actions []ActionDefinition, name string) *ActionDefinition {
	for i := range actions {
		if actions[i].Name == name {
			return &actions[i]
		}
	}
	return nil
}

type routeDecision struct {
	Intent         string `json:"intent"`
	NeedsDocuments bool   `json:"needs_documents"`
	// confidence and documents are the confidence of the intent and the
	// probability that the documents are needed, for a JEV decision model.
	confidence, documents float64
}

// action is the name of the action the router picked, "" for a search.
func (d routeDecision) action() string {
	if d.Intent == searchIntent {
		return ""
	}
	return d.Intent
}

// keepsAnswer tells whether the answer from the documents started with the
// router is the one to give. A content is written with its own instructions,
// and the params of an action that does not need the documents are filled
// from the conversation: the answer is not needed. Params that need the
// documents are filled from the answer, which comes first.
func keepsAnswer(actions []ActionDefinition, d routeDecision) bool {
	a := actionFor(actions, d.action())
	return a == nil || (a.Content == nil && d.NeedsDocuments)
}

func routerPrompt(actions []ActionDefinition) string {
	var b strings.Builder
	b.WriteString("You are the router of the Twake assistant. Read the last user message of the conversation and decide what to do with it.\n\n")
	b.WriteString("Answer with a JSON object: {\"intent\": \"...\", \"needs_documents\": true or false}\n\n")
	b.WriteString("intent is one of:\n")
	b.WriteString("- \"search\": answer the message from the user's documents. This is the default: questions, requests for information, ")
	b.WriteString("summaries or explanations given in the chat, small talk, and questions about HOW to do something.\n")
	for _, a := range actions {
		fmt.Fprintf(&b, "- %q: %s\n", a.Name, strings.TrimSpace(a.Description))
	}
	b.WriteString("Choose an action only when the user explicitly asks the assistant to do it now. When in doubt, choose \"search\".\n")
	b.WriteString("\nneeds_documents is true when the action may need information from the user's documents: anything about their organization, ")
	b.WriteString("its rules and policies, their projects, clients, colleagues, meetings or files, even when the user does not say \"from my files\". ")
	b.WriteString("It is false for \"search\", for an action made from the conversation or from the message itself, ")
	b.WriteString("and for a general subject that does not depend on the user's organization, like general best practices.\n\n")
	b.WriteString("Examples:\n")
	for _, ex := range routerExamples(actions) {
		decision, _ := json.Marshal(routeDecision{Intent: ex.intent, NeedsDocuments: ex.NeedsDocuments})
		fmt.Fprintf(&b, "User: %q\n%s\n", ex.Message, decision)
	}
	return b.String()
}

type routerExample struct {
	ActionExample
	intent string
}

// routerExamples are the examples of the router prompt: two searches, then
// the examples of the actions.
func routerExamples(actions []ActionDefinition) []routerExample {
	examples := []routerExample{
		{ActionExample{Message: "What does our contract with Acme say about penalties?"}, searchIntent},
		{ActionExample{Message: "Comment je partage un dossier ?"}, searchIntent},
	}
	for _, a := range actions {
		for _, ex := range a.Examples {
			examples = append(examples, routerExample{ex, a.Name})
		}
	}
	return examples
}

// routerToolsPrompt is the router prompt when the LLM calls a tool: the
// descriptions of the actions are in the tools.
func routerToolsPrompt(actions []ActionDefinition) string {
	var b strings.Builder
	b.WriteString("You are the router of the Twake assistant. Read the last user message of the conversation and call the one tool that handles it.\n\n")
	b.WriteString("\"search\" is the default: questions, requests for information, summaries or explanations given in the chat, small talk, ")
	b.WriteString("and questions about HOW to do something. ")
	b.WriteString("Call another tool only when the user explicitly asks the assistant to do it now. When in doubt, call \"search\".\n")
	b.WriteString("\nThe needs_documents argument of the other tools is true when the action may need information from the user's documents: ")
	b.WriteString("anything about their organization, its rules and policies, their projects, clients, colleagues, meetings or files, ")
	b.WriteString("even when the user does not say \"from my files\". ")
	b.WriteString("It is false for an action made from the conversation or from the message itself, ")
	b.WriteString("and for a general subject that does not depend on the user's organization, like general best practices.\n\n")
	b.WriteString("Examples:\n")
	for _, ex := range routerExamples(actions) {
		if ex.intent == searchIntent {
			fmt.Fprintf(&b, "User: %q\nsearch()\n", ex.Message)
		} else {
			fmt.Fprintf(&b, "User: %q\n%s({\"needs_documents\":%t})\n", ex.Message, ex.intent, ex.NeedsDocuments)
		}
	}
	return b.String()
}

// transcript writes the conversation for the router and the LLM: the
// assistant prompt is left out, and the long messages are cut, the context
// of a small LLM is short. The last user message is kept whole up to
// maxChars, then the previous turns are taken from the most recent while
// they fit in maxTotal characters.
func transcript(messages []ragMessage, maxChars, maxTotal int) string {
	previous, last, ok := recentTurns(messages, maxChars, maxTotal)
	if !ok {
		return ""
	}
	var b strings.Builder
	if len(previous) > 0 {
		b.WriteString("Conversation so far:\n")
		for _, turn := range previous {
			fmt.Fprintf(&b, "%s: %s\n", turn.Role, turn.Content)
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Last user message:\n%s\n", last)
	return b.String()
}

// recentTurns returns the user and assistant messages of the conversation,
// cut like the transcript: the last message, then the previous turns that fit.
func recentTurns(messages []ragMessage, maxChars, maxTotal int) (previous []ragMessage, last string, ok bool) {
	var turns []ragMessage
	for _, msg := range messages {
		if msg.Role == UserRole || msg.Role == AssistantRole {
			turns = append(turns, msg)
		}
	}
	if len(turns) == 0 {
		return nil, "", false
	}
	last = truncate(turns[len(turns)-1].Content, maxChars)
	budget := maxTotal - len([]rune(last))
	for i := len(turns) - 2; i >= 0; i-- {
		turn := ragMessage{Role: turns[i].Role, Content: truncate(turns[i].Content, maxChars)}
		budget -= len([]rune(fmt.Sprintf("%s: %s\n", turn.Role, turn.Content)))
		if budget < 0 {
			break
		}
		previous = append([]ragMessage{turn}, previous...)
	}
	return previous, last, true
}

func truncate(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars]) + " [...]"
}

// routerTools is the router mode of the config where the LLM calls a tool,
// like function calling, instead of answering a JSON object.
const routerTools = "tools"

// routerJEV is the router mode of the config for a decision model of the JEV
// family (System One): it answers typed questions with probabilities, a
// choice for the intent and a yes or no for the documents, without
// generating text.
const routerJEV = "jev"

// routerJEVGate is the JEV mode with a third question, whether the user
// explicitly asks the assistant to do something now: when not, the message
// is a search, whatever the choice of the intent.
const routerJEVGate = "jev-gate"

// routeQuery asks the LLM whether the last message of the conversation is a
// search or one of the actions. On any failure, it answers a search.
func routeQuery(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}) (routeDecision, error) {
	return route(ctx, inst, inst.RAGServer().Router, messages, actions, override)
}

func route(ctx context.Context, inst *instance.Instance, mode string, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}) (routeDecision, error) {
	search := routeDecision{Intent: searchIntent}
	ctx, cancel := context.WithTimeout(ctx, routerTimeout)
	defer cancel()

	var decision routeDecision
	var err error
	switch mode {
	case routerTools:
		decision, err = routeWithTools(ctx, inst, messages, actions, override)
	case routerJEV, routerJEVGate:
		decision, err = routeWithJEV(ctx, inst, messages, actions, override, mode == routerJEVGate)
	default:
		decision, err = routeWithSchema(ctx, inst, messages, actions, override)
	}
	if err != nil {
		return search, err
	}
	if decision.Intent == searchIntent {
		decision.NeedsDocuments = false
		return decision, nil
	}
	if actionFor(actions, decision.Intent) == nil {
		return search, fmt.Errorf("unknown intent %q", decision.Intent)
	}
	return decision, nil
}

// routeWithSchema asks the LLM for a JSON object with the intent, in a closed
// list, and needs_documents.
func routeWithSchema(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}) (routeDecision, error) {
	intents := []string{searchIntent}
	for _, a := range actions {
		intents = append(intents, a.Name)
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
	return decision, err
}

// routeWithTools asks the LLM to call one tool: "search" without argument,
// or an action with its needs_documents argument.
func routeWithTools(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}) (routeDecision, error) {
	tools := []map[string]interface{}{
		functionTool(searchIntent, "answer the message from the user's documents", map[string]interface{}{
			"type": "object", "properties": map[string]interface{}{},
		}),
	}
	for _, a := range actions {
		tools = append(tools, functionTool(a.Name, strings.TrimSpace(a.Description), map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"needs_documents": map[string]interface{}{
					"type":        "boolean",
					"description": "whether the action may need information from the user's documents",
				},
			},
			"required":             []string{"needs_documents"},
			"additionalProperties": false,
		}))
	}
	payload := map[string]interface{}{
		"messages": []ragMessage{
			{Role: SystemRole, Content: routerToolsPrompt(actions)},
			{Role: UserRole, Content: transcript(messages, 600, 4000)},
		},
		"stream":      false,
		"temperature": 0,
		"max_tokens":  32,
		"tools":       tools,
		"tool_choice": "required",
	}
	if override != nil {
		payload["metadata"] = map[string]interface{}{"llm_override": override}
	}
	msg, err := complete(ctx, inst, payload)
	if err != nil {
		return routeDecision{}, err
	}
	if len(msg.ToolCalls) == 0 {
		return routeDecision{}, fmt.Errorf("no tool call in %q", truncate(msg.Content, 200))
	}
	call := msg.ToolCalls[0].Function
	decision := routeDecision{Intent: call.Name}
	if strings.TrimSpace(call.Arguments) != "" {
		var args struct {
			NeedsDocuments bool `json:"needs_documents"`
		}
		if err := decodeJSONObject(call.Arguments, &args); err != nil {
			return routeDecision{}, err
		}
		decision.NeedsDocuments = args.NeedsDocuments
	}
	return decision, nil
}

// routeWithJEV asks a JEV decision model two questions on the conversation:
// the intent, as a choice between the search and the actions, and whether
// the action needs the user's documents.
func routeWithJEV(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}, gate bool) (routeDecision, error) {
	previous, last, ok := recentTurns(messages, 600, 4000)
	if !ok {
		return routeDecision{}, errors.New("no message")
	}
	conversation := make([]map[string]string, len(previous))
	for i, turn := range previous {
		conversation[i] = map[string]string{"role": turn.Role, "content": turn.Content}
	}

	intents := map[string]interface{}{}
	var searches, withDocuments, withoutDocuments []string
	for _, ex := range routerExamples(actions) {
		switch {
		case ex.intent == searchIntent:
			searches = append(searches, ex.Message)
		case ex.NeedsDocuments:
			withDocuments = append(withDocuments, ex.Message)
		default:
			withoutDocuments = append(withoutDocuments, ex.Message)
		}
	}
	intents[searchIntent] = map[string]interface{}{
		"description": "answer the message from the user's documents. This is the default: questions, requests for information, " +
			"summaries or explanations given in the chat, small talk, and questions about HOW to do something.",
		"examples": searches,
	}
	for _, a := range actions {
		criterion := map[string]interface{}{"description": strings.TrimSpace(a.Description)}
		var examples []string
		for _, ex := range a.Examples {
			examples = append(examples, ex.Message)
		}
		if len(examples) > 0 {
			criterion["examples"] = examples
		}
		intents[a.Name] = criterion
	}
	request := map[string]interface{}{
		"state": map[string]interface{}{
			"conversation":      conversation,
			"last_user_message": last,
		},
		"questions": map[string]interface{}{
			"intent": map[string]interface{}{
				"type": "choice",
				"instructions": "What should the assistant of the user do with the last user message? " +
					"Choose an action only when the user explicitly asks the assistant to do it now. When in doubt, choose search.",
				"criteria": intents,
			},
			"needs_documents": map[string]interface{}{
				"type":         "noul",
				"instructions": "Does what the user asks in the last message need information from the user's documents?",
				"criteria": map[string]interface{}{
					"true": map[string]interface{}{
						"description": "anything about their organization, its rules and policies, their projects, clients, colleagues, " +
							"meetings or files, even when the user does not say \"from my files\"",
						"examples": withDocuments,
					},
					"false": map[string]interface{}{
						"description": "made from the conversation or from the message itself, or a general subject that does not " +
							"depend on the user's organization, like general best practices",
						"examples": withoutDocuments,
					},
				},
			},
		},
	}
	if gate {
		questions := request["questions"].(map[string]interface{})
		questions["explicit_request"] = map[string]interface{}{
			"type": "noul",
			"instructions": "In the last message, does the user explicitly ask the assistant to do something now, " +
				"like writing, creating, preparing or sending something?",
			"criteria": map[string]interface{}{
				"true": "a request or an order to the assistant, even politely phrased as a question (\"Peux-tu écrire…\")",
				"false": "a question to get information, a question about HOW to do something or about what the assistant can do, " +
					"a remark, a thank you, or small talk",
			},
		}
	}
	content, err := json.Marshal(request)
	if err != nil {
		return routeDecision{}, err
	}
	// The request is the content of the last message, the way the JEV
	// models are served behind an OpenAI-compatible API.
	payload := map[string]interface{}{
		"messages": []ragMessage{{Role: UserRole, Content: string(content)}},
		"stream":   false,
	}
	if override != nil {
		payload["metadata"] = map[string]interface{}{"llm_override": override}
	}
	msg, err := complete(ctx, inst, payload)
	if err != nil {
		return routeDecision{}, err
	}
	var answer struct {
		Answers struct {
			Intent struct {
				Choice     string  `json:"choice"`
				Confidence float64 `json:"confidence"`
			} `json:"intent"`
			NeedsDocuments struct {
				Noul float64 `json:"noul"`
			} `json:"needs_documents"`
			ExplicitRequest *struct {
				Noul float64 `json:"noul"`
			} `json:"explicit_request"`
		} `json:"answers"`
	}
	if err := decodeJSONObject(msg.Content, &answer); err != nil {
		return routeDecision{}, err
	}
	if gate {
		if answer.Answers.ExplicitRequest == nil {
			return routeDecision{}, errors.New("no answer to explicit_request")
		}
		if answer.Answers.ExplicitRequest.Noul < 0.5 {
			return routeDecision{Intent: searchIntent, confidence: answer.Answers.ExplicitRequest.Noul}, nil
		}
	}
	return routeDecision{
		Intent:         answer.Answers.Intent.Choice,
		NeedsDocuments: answer.Answers.NeedsDocuments.Noul >= 0.5,
		confidence:     answer.Answers.Intent.Confidence,
		documents:      answer.Answers.NeedsDocuments.Noul,
	}, nil
}

func functionTool(name, description string, parameters map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        name,
			"description": description,
			"parameters":  parameters,
		},
	}
}

// checkDocuments makes sure that an action uses the user's documents when
// they are relevant to the message, whatever the router said: a small LLM
// makes up the facts of the user's organization when it writes without them.
func checkDocuments(ctx context.Context, inst *instance.Instance, d routeDecision, message, workspace string) (routeDecision, error) {
	if d.action() == "" || d.NeedsDocuments {
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

// writingPrompt is the instructions to write the content of an action, from
// the user's documents or from the conversation and what the LLM knows of a
// general subject.
func writingPrompt(a *ActionDefinition, fromDocuments bool, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The user asks for this action: %s\n", strings.TrimSpace(a.Description))
	fmt.Fprintf(&b, "Today is %s.\n\n", now.Format("Monday, January 2, 2006"))
	b.WriteString("Write the content of the action itself, in full:\n")
	b.WriteString("- In Markdown, in the language of the user.\n")
	b.WriteString("- Start with one line \"# \" followed by its title, nothing before it.\n")
	if instructions := strings.TrimSpace(a.Instructions); instructions != "" {
		fmt.Fprintf(&b, "- %s\n", instructions)
	}
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

func contentTokens(a *ActionDefinition) int {
	if a.Content != nil && a.Content.MaxTokens > 0 {
		return a.Content.MaxTokens
	}
	return defaultContentTokens
}

// writingRequest is the body of the call that writes the content of an
// action without the documents: the LLM behind openRAG is asked directly,
// with the conversation as material.
func writingRequest(a *ActionDefinition, messages []ragMessage, stream bool, override map[string]interface{}, now time.Time) ([]byte, error) {
	payload := map[string]interface{}{
		"messages": []ragMessage{
			{Role: SystemRole, Content: writingPrompt(a, false, now)},
			{Role: UserRole, Content: transcript(messages, 4000, writingHistoryChars)},
		},
		"stream":      stream,
		"temperature": Temperature,
		"max_tokens":  contentTokens(a),
	}
	if override != nil {
		payload["metadata"] = map[string]interface{}{"llm_override": override}
	}
	return json.Marshal(payload)
}

// withWritingInstructions adds the writing instructions to the messages of a
// query to openRAG, after the prompt of the assistant: openRAG takes the
// leading system messages as custom instructions.
func withWritingInstructions(messages []ragMessage, a *ActionDefinition, now time.Time) []ragMessage {
	instructions := ragMessage{Role: SystemRole, Content: writingPrompt(a, true, now)}
	i := 0
	for i < len(messages) && messages[i].Role == SystemRole {
		i++
	}
	out := make([]ragMessage, 0, len(messages)+1)
	out = append(out, messages[:i]...)
	out = append(out, instructions)
	return append(out, messages[i:]...)
}

// contentTitle returns the title of a written content: its first heading,
// which the writing instructions ask for. A content without it is not the
// content of the action, like an answer saying the documents do not cover
// the subject, and the action is not proposed.
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

func fillPrompt(a *ActionDefinition, now time.Time) string {
	var b strings.Builder
	b.WriteString("You prepare an action that the user asked for. The user will review it before it runs.\n")
	fmt.Fprintf(&b, "Today is %s.\n\n", now.Format("Monday, January 2, 2006"))
	fmt.Fprintf(&b, "Action %q: %s\n", a.Name, strings.TrimSpace(a.Description))
	if instructions := strings.TrimSpace(a.Instructions); instructions != "" {
		fmt.Fprintf(&b, "%s\n", instructions)
	}
	b.WriteString("\nAnswer with a JSON object with these fields:\n")
	for _, name := range a.Parameters.paramNames() {
		prop := a.Parameters.Properties[name]
		kind := "string"
		if prop.Type == "array" {
			kind = "list of strings"
		}
		required := ""
		if a.Parameters.isRequired(name) {
			required = ", required"
		}
		fmt.Fprintf(&b, "- %q (%s%s): %s\n", name, kind, required, strings.TrimSpace(prop.Description))
	}
	b.WriteString("Use \"\" (or [] for a list) for a field you cannot fill. ")
	b.WriteString("Never invent facts, names or email addresses: use only what is in the conversation. ")
	b.WriteString("Write in the language of the user.\n")
	return b.String()
}

// paramsSchema is the schema the answer of the LLM must follow: every field
// is asked for, "" or [] when unknown, as a small model follows a fixed
// shape better than optional fields.
func paramsSchema(a *ActionDefinition) map[string]interface{} {
	properties := map[string]interface{}{}
	names := a.Parameters.paramNames()
	for _, name := range names {
		if a.Parameters.Properties[name].Type == "array" {
			properties[name] = map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}
		} else {
			properties[name] = map[string]interface{}{"type": "string"}
		}
	}
	return map[string]interface{}{
		"type":                 "object",
		"properties":           properties,
		"required":             names,
		"additionalProperties": false,
	}
}

// fillAction asks the LLM for the params of the action. answer is the answer
// from the documents when the action needs it, "" otherwise.
func fillAction(ctx context.Context, inst *instance.Instance, a *ActionDefinition, messages []ragMessage, answer string, override map[string]interface{}, now time.Time) (*ChatAction, error) {
	ctx, cancel := context.WithTimeout(ctx, fillTimeout)
	defer cancel()

	user := transcript(messages, 4000, writingHistoryChars)
	if answer != "" {
		user += "\nWhat the assistant found in the user's documents:\n" + truncate(answer, 4000) + "\n"
	}
	var params map[string]interface{}
	if err := completeJSON(ctx, inst, fillPrompt(a, now), user, a.Name, paramsSchema(a), 0, override, &params); err != nil {
		return nil, err
	}
	params, err := checkParams(a, params, userText(messages))
	if err != nil {
		return nil, err
	}
	return &ChatAction{Name: a.Name, Params: params}, nil
}

// userText is what the user wrote in the conversation, the only place the
// values of a user-written param can come from.
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

// checkParams keeps the params of the action with the types of its schema,
// drops the values of the user-written params the user did not write, and
// checks the required ones.
func checkParams(a *ActionDefinition, raw map[string]interface{}, written string) (map[string]interface{}, error) {
	params := map[string]interface{}{}
	for _, name := range a.Parameters.paramNames() {
		prop := a.Parameters.Properties[name]
		required := a.Parameters.isRequired(name)
		if prop.Type == "array" {
			list := []string{}
			values, _ := raw[name].([]interface{})
			for _, value := range values {
				s, ok := value.(string)
				s = strings.TrimSpace(s)
				if !ok || s == "" {
					continue
				}
				if prop.UserWritten && !strings.Contains(written, strings.ToLower(s)) {
					continue
				}
				list = append(list, s)
			}
			if required && len(list) == 0 {
				return nil, fmt.Errorf("missing %s", name)
			}
			params[name] = list
			continue
		}
		s, _ := raw[name].(string)
		s = strings.TrimSpace(s)
		if prop.UserWritten && !strings.Contains(written, strings.ToLower(s)) {
			s = ""
		}
		if required && s == "" {
			return nil, fmt.Errorf("missing %s", name)
		}
		params[name] = s
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
	msg, err := complete(ctx, inst, payload)
	if err != nil {
		return err
	}
	return decodeJSONObject(msg.Content, out)
}

// completionMessage is the message of a completion of the LLM: a text, or
// tool calls.
type completionMessage struct {
	Content   string `json:"content"`
	ToolCalls []struct {
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

// complete sends a completion request to the LLM behind openRAG, without
// retrieval, and returns its message.
func complete(ctx context.Context, inst *instance.Instance, payload map[string]interface{}) (*completionMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	res, err := CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, fmt.Errorf("POST status code: %d", res.StatusCode)
	}
	var completion struct {
		Choices []struct {
			Message completionMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&completion); err != nil {
		return nil, err
	}
	if len(completion.Choices) == 0 {
		return nil, errors.New("no completion")
	}
	return &completion.Choices[0].Message, nil
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
