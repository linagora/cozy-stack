package rag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/labstack/echo/v4"
)

// The chat router lets the assistant propose an action of the client instead
// of an answer: the LLM picks an intent in a closed list, then the params are
// filled as the answer would have been. See docs/ai.md.

// searchIntent is the router's answer for a message that is not an action:
// the normal answer.
const searchIntent = "search"

// factsMissing is the field the LLM alone fills with the params: a schema
// forces it to fill them, and without a way to say that the conversation
// does not give the facts they need, it invents them.
const factsMissing = "facts_missing"

// errFactsMissing tells that the action cannot be made from the conversation.
var errFactsMissing = errors.New("the conversation does not give the facts the action needs")

// routerTimeout bounds the decision of the router, which holds back the
// answer. A reasoning LLM, as qwen3.8, thinks before it decides: from 1 to
// 16 seconds measured, and more when its gateway is slow. A variable so that
// tests can shorten it.
var routerTimeout = 45 * time.Second

// actionNameRegexp is the rule of the name of a JSON schema in the OpenAI
// API: the name of an action names the schema of its params.
var actionNameRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ActionDefinition is an action the client can run, as it describes it.
type ActionDefinition struct {
	Name string `json:"name"`
	// Description tells the router what the action does, and when to pick it.
	Description string `json:"description"`
	// Examples are messages for which the router picks the action.
	Examples []string `json:"examples,omitempty"`
	// Parameters is the JSON schema of the params the LLM fills, like the
	// parameters of a tool for function calling. Without it, the action has
	// no params.
	Parameters *ParametersSchema `json:"parameters,omitempty"`
	// Instructions tell the LLM how to fill the params.
	Instructions string `json:"instructions,omitempty"`
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
	names := map[string]bool{}
	for _, a := range actions {
		// "Search" next to "search" would confuse the router
		if !actionNameRegexp.MatchString(a.Name) || strings.EqualFold(a.Name, searchIntent) {
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
	if strings.TrimSpace(a.Description) == "" {
		return errors.New("the description is empty")
	}
	for _, ex := range a.Examples {
		if strings.TrimSpace(ex) == "" {
			return errors.New("an example is empty")
		}
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
	for _, name := range p.paramNames() {
		prop := p.Properties[name]
		if name == factsMissing {
			return fmt.Errorf("param %s is reserved", factsMissing)
		}
		switch {
		case prop.Type == "string" && prop.Items == nil:
		case prop.Type == "array" && prop.Items != nil && prop.Items.Type == "string":
		default:
			return fmt.Errorf("param %s must be a string or a list of strings", name)
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

// decide asks the router what to do with the message: the name of the action
// to propose, "" for a search. On any failure, the message is a search.
func (q *chatQuery) decide(ctx context.Context, actions []ActionDefinition) string {
	ctx, cancel := context.WithTimeout(ctx, routerTimeout)
	defer cancel()
	started := time.Now()
	intent, err := route(ctx, q.inst, q.messages, actions, q.override())
	if err != nil {
		q.logger.Warnf("chat router failed, answering as a search: %s", err)
		return ""
	}
	q.logger.Infof("chat router: intent=%s in %s", intent, time.Since(started))
	if intent == searchIntent {
		return ""
	}
	return intent
}

// route asks the LLM whether the last message of the conversation is a
// search or one of the actions.
func route(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}) (string, error) {
	intent, err := routeWithSchema(ctx, inst, messages, actions, override)
	if err != nil {
		return "", err
	}
	if intent != searchIntent && actionFor(actions, intent) == nil {
		return "", fmt.Errorf("unknown intent %q", intent)
	}
	return intent, nil
}

// routeWithSchema asks the LLM for a JSON object with the intent, in a closed
// list.
func routeWithSchema(ctx context.Context, inst *instance.Instance, messages []ragMessage, actions []ActionDefinition, override map[string]interface{}) (string, error) {
	intents := []string{searchIntent}
	for _, a := range actions {
		intents = append(intents, a.Name)
	}
	schema := map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"intent": map[string]interface{}{"type": "string", "enum": intents},
		},
		"required":             []string{"intent"},
		"additionalProperties": false,
	}
	var decision struct {
		Intent string `json:"intent"`
	}
	err := completeJSON(ctx, inst, routerPrompt(actions), transcript(lastExchange(messages)), "route", schema, override, &decision)
	return decision.Intent, err
}

func routerPrompt(actions []ActionDefinition) string {
	return renderPrompt("router_schema.txt", map[string]interface{}{
		"Actions":  actions,
		"Examples": routerExamples(actions),
	})
}

// routerExample is an example of the router prompt.
type routerExample struct {
	Message string
	Intent  string
}

// routerExamples are the examples of the router prompt: two searches, then
// the examples of the actions.
func routerExamples(actions []ActionDefinition) []routerExample {
	examples := []routerExample{
		{Message: "What does our contract with Acme say about penalties?", Intent: searchIntent},
		{Message: "Comment je partage un dossier ?", Intent: searchIntent},
	}
	for _, a := range actions {
		for _, ex := range a.Examples {
			examples = append(examples, routerExample{Message: ex, Intent: a.Name})
		}
	}
	return examples
}

// turns are the user and assistant messages of the conversation.
func turns(messages []ragMessage) []ragMessage {
	var out []ragMessage
	for _, msg := range messages {
		if msg.Role == UserRole || msg.Role == AssistantRole {
			out = append(out, msg)
		}
	}
	return out
}

// lastExchange is what the router reads: the last user message, and the
// exchange before it, which a message like "make a note of it" refers to.
func lastExchange(messages []ragMessage) []ragMessage {
	all := turns(messages)
	users := 0
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Role == UserRole {
			if users++; users == 2 {
				return all[i:]
			}
		}
	}
	return all
}

// transcript writes the user and assistant turns for the LLM alone, each
// quoted so that a document quoted in an answer cannot pass for a turn.
func transcript(messages []ragMessage) string {
	all := turns(messages)
	if len(all) == 0 {
		return ""
	}
	var b strings.Builder
	if len(all) > 1 {
		b.WriteString("Conversation so far:\n")
		for _, turn := range all[:len(all)-1] {
			fmt.Fprintf(&b, "%s: %s\n", turn.Role, strconv.Quote(strings.TrimSpace(turn.Content)))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Last user message: %s\n", strconv.Quote(strings.TrimSpace(all[len(all)-1].Content)))
	return b.String()
}

// truncate cuts a text quoted in an error message.
func truncate(s string, maxChars int) string {
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > maxChars {
		return string(runes[:maxChars]) + " [...]"
	}
	return s
}

// oneline joins the lines of a text, for a text that goes in a list or a
// sentence of a prompt.
func oneline(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// actionPrompt is the instructions to fill the params of an action, with the
// user's documents or from the conversation only.
func actionPrompt(a *ActionDefinition, fromDocuments bool, now time.Time) string {
	var params []promptParam
	for _, name := range a.Parameters.paramNames() {
		prop := a.Parameters.Properties[name]
		kind := "string"
		if prop.Type == "array" {
			kind = "list of strings"
		}
		params = append(params, promptParam{
			Name:        name,
			Kind:        kind,
			Required:    a.Parameters.isRequired(name),
			Description: prop.Description,
		})
	}
	return renderPrompt("action.txt", map[string]interface{}{
		"Action":        a,
		"FromDocuments": fromDocuments,
		"Today":         promptDate(now),
		"Params":        params,
	})
}

// promptParam is a parameter of an action in the fill prompt.
type promptParam struct {
	Name        string
	Kind        string
	Description string
	Required    bool
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

// jsonSchemaFormat is the response_format of a completion that must follow
// the schema.
func jsonSchemaFormat(name string, schema map[string]interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type": "json_schema",
		"json_schema": map[string]interface{}{
			"name":   name,
			"schema": schema,
			"strict": true,
		},
	}
}

// fillRequest fills the params with the documents: the conversation with the
// fill instructions as a leading system message, and the schema of the params
// as response_format. openRAG retrieves the documents as for an answer.
func fillRequest(inst *instance.Instance, a *ActionDefinition, messages []ragMessage, metadata map[string]interface{}, now time.Time) map[string]interface{} {
	return map[string]interface{}{
		"model":           fmt.Sprintf("ragondin-%s", inst.Domain),
		"messages":        withInstructions(messages, actionPrompt(a, true, now)),
		"stream":          false,
		"temperature":     0,
		"metadata":        metadata,
		"response_format": jsonSchemaFormat(a.Name, paramsSchema(a)),
	}
}

// fillAction asks for the params of the action: openRAG with the user's
// documents, which gives the sources it used too, or the LLM alone from the
// conversation when the client asks for an answer without the documents.
func (q *chatQuery) fillAction(ctx context.Context, a *ActionDefinition) (*ChatAction, []Source, error) {
	if a.Parameters == nil || len(a.Parameters.Properties) == 0 {
		// Nothing to fill: the action is ready
		return &ChatAction{Name: a.Name, Params: map[string]interface{}{}}, nil, nil
	}
	var params map[string]interface{}
	var sources []Source
	var err error
	if q.direct {
		schema := paramsSchema(a)
		schema["properties"].(map[string]interface{})[factsMissing] = map[string]interface{}{"type": "boolean"}
		schema["required"] = append([]string{factsMissing}, schema["required"].([]string)...)
		err = completeJSON(ctx, q.inst, actionPrompt(a, false, q.now), transcript(q.messages), a.Name, schema, q.override(), &params)
		if missing, _ := params[factsMissing].(bool); err == nil && missing {
			err = errFactsMissing
		}
	} else {
		var body []byte
		var msg *completionMessage
		if body, err = json.Marshal(fillRequest(q.inst, a, q.messages, q.metadata, q.now)); err == nil {
			var res *http.Response
			if res, err = q.postCompletion(ctx, body); err == nil {
				msg, sources, err = decodeCompletionWithSources(res)
			}
		}
		if err == nil {
			err = decodeJSONObject(msg.Content, &params)
		}
	}
	if err != nil {
		return nil, nil, err
	}
	params, err = checkParams(a, params, userText(q.messages))
	if err != nil {
		return nil, nil, err
	}
	return &ChatAction{Name: a.Name, Params: params}, sources, nil
}

// decodeCompletionWithSources reads the message of a completion of openRAG
// and the sources it gives with it, and closes the body.
func decodeCompletionWithSources(res *http.Response) (*completionMessage, []Source, error) {
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil, nil, fmt.Errorf("POST status code: %d", res.StatusCode)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, nil, err
	}
	var completion struct {
		Choices []struct {
			Message completionMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, nil, err
	}
	if len(completion.Choices) == 0 {
		return nil, nil, errors.New("no completion")
	}
	var event map[string]interface{}
	_ = json.Unmarshal(raw, &event)
	// The sources are a bonus: params without them are still the action's
	sources, _ := getSources(event)
	return &completion.Choices[0].Message, sources, nil
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
	return oneline(b.String())
}

// userWrote returns the value as the user wrote it, when it is in the text
// of the user as whole words, whatever the case: "Paul" is not written in
// "Paulette", nor "claire@example.com" in "marie-claire@example.com".
func userWrote(written, value string) (string, bool) {
	text, want := []rune(written), []rune(oneline(value))
	if len(want) == 0 {
		return "", false
	}
	email := strings.ContainsRune(value, '@')
	for i := 0; i+len(want) <= len(text); i++ {
		found := text[i : i+len(want)]
		if equalFold(found, want) && !joinsWord(text, i, email, true) && !joinsWord(text, i+len(want), email, false) {
			return string(found), true
		}
	}
	return "", false
}

// equalFold tells whether the runes are the same whatever the case, with
// the simple folding of Unicode: a look-alike like "İ" is not "i".
func equalFold(a, b []rune) bool {
	for i := range a {
		if a[i] != b[i] && !foldsTo(a[i], b[i]) {
			return false
		}
	}
	return true
}

func foldsTo(a, b rune) bool {
	for r := unicode.SimpleFold(a); r != a; r = unicode.SimpleFold(r) {
		if r == b {
			return true
		}
	}
	return false
}

// joinsWord tells whether the text continues the word found at i, to its left
// or right, dots included: letters, digits, the punctuation of names, and for
// an email the specials of a local part (left) or of a domain (right).
func joinsWord(text []rune, i int, email, left bool) bool {
	step := 1
	if left {
		step, i = -1, i-1
	}
	for i >= 0 && i < len(text) && text[i] == '.' {
		i += step
	}
	r := runeAt(text, i)
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		return true
	}
	switch {
	case email && left:
		return strings.ContainsRune("!#$%&'*+-/=?^_`{|}~@", r)
	case email:
		return r == '-' || r == '_'
	}
	return strings.ContainsRune("@-_+", r)
}

func runeAt(text []rune, i int) rune {
	if i < 0 || i >= len(text) {
		return 0
	}
	return text[i]
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
			seen := map[string]bool{}
			values, _ := raw[name].([]interface{})
			for _, value := range values {
				s, ok := value.(string)
				if !ok {
					continue
				}
				s = checkValue(prop, s, written)
				if s == "" || seen[s] {
					continue
				}
				seen[s] = true
				list = append(list, s)
			}
			if required && len(list) == 0 {
				return nil, fmt.Errorf("missing %s", name)
			}
			params[name] = list
			continue
		}
		s, _ := raw[name].(string)
		s = checkValue(prop, s, written)
		if required && s == "" {
			return nil, fmt.Errorf("missing %s", name)
		}
		params[name] = s
	}
	return params, nil
}

// checkValue trims a value, and keeps a user-written one as the user wrote
// it: a word, without the dot ending a sentence.
func checkValue(prop ParamSchema, value, written string) string {
	value = strings.TrimSpace(value)
	if !prop.UserWritten || value == "" {
		return value
	}
	value = strings.TrimRight(value, ".")
	if !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
		return ""
	}
	wrote, _ := userWrote(written, value)
	return wrote
}

// describeAction is how a proposed action appears in the history sent to the
// LLM, so that a follow-up message ("change the title") has its context.
func describeAction(action *ChatAction) string {
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(action.Params)
	return fmt.Sprintf("(Proposed to the user: %s %s)", action.Name, strings.TrimSpace(raw.String()))
}

// completeJSON asks the LLM behind openRAG, without retrieval, for a JSON
// object matching the schema, and decodes it in out.
func completeJSON(ctx context.Context, inst *instance.Instance, system, user, name string, schema map[string]interface{}, override map[string]interface{}, out interface{}) error {
	// Without a model, openRAG sends the messages to its LLM as they are,
	// with no retrieval and no answer prompt.
	payload := map[string]interface{}{
		"messages": []ragMessage{
			{Role: SystemRole, Content: system},
			{Role: UserRole, Content: user},
		},
		"stream":          false,
		"temperature":     0,
		"response_format": jsonSchemaFormat(name, schema),
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

// completionMessage is the message of a completion of the LLM.
type completionMessage struct {
	Content string `json:"content"`
}

// complete sends a completion request to openRAG, and returns its message.
func complete(ctx context.Context, inst *instance.Instance, payload map[string]interface{}) (*completionMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	res, err := CallRAGQueryContext(ctx, inst, http.MethodPost, body, "v1/chat/completions", echo.MIMEApplicationJSON)
	if err != nil {
		return nil, err
	}
	return decodeCompletion(res)
}

// decodeCompletion reads the message of a completion, and closes the body.
func decodeCompletion(res *http.Response) (*completionMessage, error) {
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
		return fmt.Errorf("no JSON object in %q", truncate(content, 80))
	}
	return json.Unmarshal([]byte(content[start:end+1]), out)
}
