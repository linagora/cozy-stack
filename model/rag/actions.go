package rag

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// The chat actions are the tools of the LLM: the client defines them, the
// LLM calls one instead of answering, and the client runs it once the user
// has confirmed it. See docs/ai.md.

// actionNameRegexp is the rule of the name of a function in the OpenAI API.
var actionNameRegexp = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ToolDefinition is an action the client can run, as it describes it to
// the LLM: a tool.
type ToolDefinition struct {
	Name string `json:"name"`
	// Description tells the LLM what the action does, and when to call it.
	Description string `json:"description"`
	// Examples are messages for which the LLM calls the action.
	Examples []string `json:"examples,omitempty"`
	// Parameters is the JSON schema of the params the LLM fills. Without
	// it, the action has no params.
	Parameters *ParametersSchema `json:"parameters,omitempty"`
	// Instructions tell the LLM how to fill the params.
	Instructions string `json:"instructions,omitempty"`
}

// ParametersSchema is the JSON schema of the params of an action: an object whose
// properties are strings, numbers, integers, booleans or lists of strings.
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
	// ToolCallID is the id the LLM gave to its tool call: the result of the
	// action answers it.
	ToolCallID string `json:"toolCallId,omitempty"`
	// Status and URL are written by the client once the user has handled
	// the action: "done", with the URL of what it created, or "cancelled".
	Status string `json:"status,omitempty"`
	URL    string `json:"url,omitempty"`
}

// ValidateTools checks the tool definitions a client sends.
func ValidateTools(defs []ToolDefinition) error {
	names := map[string]bool{}
	for _, a := range defs {
		if !actionNameRegexp.MatchString(a.Name) {
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

func (a ToolDefinition) validate() error {
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
		switch {
		case isScalarType(prop.Type) && prop.Items == nil:
		case prop.Type == "array" && prop.Items != nil && prop.Items.Type == "string":
		default:
			return fmt.Errorf("param %s must be a string, a number, an integer, a boolean or a list of strings", name)
		}
		if prop.UserWritten && prop.Type != "string" && prop.Type != "array" {
			return fmt.Errorf("param %s must be a string or a list of strings to be user-written", name)
		}
	}
	for _, name := range p.Required {
		if _, ok := p.Properties[name]; !ok {
			return fmt.Errorf("required param %q is not a property", name)
		}
	}
	return nil
}

func isScalarType(t string) bool {
	switch t {
	case "string", "number", "integer", "boolean":
		return true
	}
	return false
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

func toolFor(defs []ToolDefinition, name string) *ToolDefinition {
	for i := range defs {
		if defs[i].Name == name {
			return &defs[i]
		}
	}
	return nil
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
func checkParams(a *ToolDefinition, raw map[string]interface{}, written string) (map[string]interface{}, error) {
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
		if prop.Type != "string" {
			value := raw[name]
			if !hasType(prop.Type, value) {
				value = nil
			}
			if required && value == nil {
				return nil, fmt.Errorf("missing %s", name)
			}
			params[name] = value
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

// hasType tells if a value decoded from JSON has the type of the schema. The
// value is kept as decoded: a float64 integer is encoded back without a dot.
func hasType(kind string, value interface{}) bool {
	switch v := value.(type) {
	case float64:
		return kind == "number" || kind == "integer" && v == math.Trunc(v)
	case bool:
		return kind == "boolean"
	}
	return false
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
	return fmt.Sprintf("(Proposed to the user: %s %s)", action.Name, encodeParams(action.Params))
}

// encodeParams is the JSON of the params, as the LLM reads them.
func encodeParams(params map[string]interface{}) string {
	var raw bytes.Buffer
	encoder := json.NewEncoder(&raw)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(params)
	return strings.TrimSpace(raw.String())
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

// toolCall is a tool call of the LLM, as the OpenAI API gives it.
type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// tools are the definitions of the actions as the tools of the OpenAI API.
func tools(defs []ToolDefinition) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(defs))
	for _, a := range defs {
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        a.Name,
				"description": toolDescription(a),
				"parameters":  toolParameters(a),
			},
		})
	}
	return out
}

// toolDescription is the description of the tool: what the action does, how
// to fill its params, and the requests it is for.
func toolDescription(a ToolDefinition) string {
	parts := []string{strings.TrimSpace(a.Description)}
	if s := strings.TrimSpace(a.Instructions); s != "" {
		parts = append(parts, s)
	}
	if len(a.Examples) > 0 {
		quoted := make([]string, len(a.Examples))
		for i, ex := range a.Examples {
			quoted[i] = strconv.Quote(oneline(ex))
		}
		parts = append(parts, "Requests it is for: "+strings.Join(quoted, ", "))
	}
	return strings.Join(parts, "\n")
}

// userWrittenNote tells the LLM, in the description of a param, what the
// stack keeps of its value.
const userWrittenNote = "(as the user wrote it: only what the user typed in their messages)"

// toolParameters is the JSON schema of the params for the LLM, without the
// keywords of the stack.
func toolParameters(a ToolDefinition) map[string]interface{} {
	properties := map[string]interface{}{}
	schema := map[string]interface{}{"type": "object", "properties": properties}
	if a.Parameters == nil {
		return schema
	}
	for _, name := range a.Parameters.paramNames() {
		prop := a.Parameters.Properties[name]
		p := map[string]interface{}{"type": prop.Type}
		if prop.Items != nil {
			p["items"] = map[string]interface{}{"type": prop.Items.Type}
		}
		description := strings.TrimSpace(prop.Description)
		if prop.UserWritten {
			description = strings.TrimSpace(description + " " + userWrittenNote)
		}
		if description != "" {
			p["description"] = description
		}
		properties[name] = p
	}
	if len(a.Parameters.Required) > 0 {
		schema["required"] = a.Parameters.Required
	}
	return schema
}

// actionFromCall makes the action of a tool call: its params checked against
// the schema of the action, the user-written ones kept to what the user
// wrote.
func actionFromCall(defs []ToolDefinition, call toolCall, written string) (*ChatAction, error) {
	def := toolFor(defs, call.Function.Name)
	if def == nil {
		return nil, fmt.Errorf("unknown tool %q", call.Function.Name)
	}
	params := map[string]interface{}{}
	if def.Parameters != nil && len(def.Parameters.Properties) > 0 {
		raw := map[string]interface{}{}
		if args := strings.TrimSpace(call.Function.Arguments); args != "" {
			if err := decodeJSONObject(args, &raw); err != nil {
				return nil, fmt.Errorf("%s: invalid arguments: %w", def.Name, err)
			}
		}
		var err error
		if params, err = checkParams(def, raw, written); err != nil {
			return nil, fmt.Errorf("%s: %w", def.Name, err)
		}
	}
	return &ChatAction{Name: def.Name, Params: params, ToolCallID: call.ID}, nil
}

// toolCallOf is the tool call of a proposed action, for the history: the id
// the LLM gave, or one made of the message id for an action proposed before
// the ids were kept.
func toolCallOf(msg ChatMessage) toolCall {
	call := toolCall{ID: msg.Action.ToolCallID, Type: "function"}
	if call.ID == "" {
		call.ID = "call_" + msg.ID
	}
	call.Function.Name = msg.Action.Name
	call.Function.Arguments = encodeParams(msg.Action.Params)
	return call
}

// resultContent is the content of the tool message of a result: never empty,
// and an error when the app could not run the action.
func resultContent(ok bool, content string) string {
	content = strings.TrimSpace(content)
	switch {
	case ok && content == "":
		return "Done."
	case ok:
		return content
	case content == "":
		return "Error: the app could not run the action."
	}
	return "Error: " + content
}
