package rag

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLLMModel(t *testing.T) {
	t.Run("no data and no auth", func(t *testing.T) {
		assert.Empty(t, llmModel(&account.Account{}))
	})

	t.Run("model comes from the account data", func(t *testing.T) {
		acc := &account.Account{
			Data: map[string]interface{}{"model": "gemini-3-pro"},
		}
		assert.Equal(t, "gemini-3-pro", llmModel(acc))
	})

	t.Run("data wins over a legacy login", func(t *testing.T) {
		acc := &account.Account{
			Data:  map[string]interface{}{"model": "gemini-3-pro"},
			Basic: &account.BasicInfo{Login: "gemini-2.5-flash"},
		}
		assert.Equal(t, "gemini-3-pro", llmModel(acc))
	})

	t.Run("falls back to the login on accounts written before the move", func(t *testing.T) {
		acc := &account.Account{
			Basic: &account.BasicInfo{Login: "gemini-2.5-flash"},
		}
		assert.Equal(t, "gemini-2.5-flash", llmModel(acc))
	})

	t.Run("a stale encrypted model is never used", func(t *testing.T) {
		// Editing the model without retyping the API key left the encrypted
		// blob on the previous model. It must not resurrect it.
		config.UseTestFile(t)
		encrypted, err := account.EncryptCredentials("gemini-2.5-flash", "s3cret")
		require.NoError(t, err)

		acc := &account.Account{
			Data:  map[string]interface{}{"model": "gemini-3-pro"},
			Basic: &account.BasicInfo{EncryptedCredentials: encrypted},
		}
		assert.Equal(t, "gemini-3-pro", llmModel(acc))
	})

	t.Run("no model rather than a stale one when data is empty", func(t *testing.T) {
		config.UseTestFile(t)
		encrypted, err := account.EncryptCredentials("gemini-2.5-flash", "s3cret")
		require.NoError(t, err)

		acc := &account.Account{
			Basic: &account.BasicInfo{EncryptedCredentials: encrypted},
		}
		assert.Empty(t, llmModel(acc))
	})
}

func TestLLMAPIKey(t *testing.T) {
	config.UseTestFile(t)

	t.Run("no basic auth", func(t *testing.T) {
		assert.Empty(t, llmAPIKey(nil))
	})

	t.Run("plain password when nothing is encrypted", func(t *testing.T) {
		assert.Equal(t, "s3cret", llmAPIKey(&account.BasicInfo{Password: "s3cret"}))
	})

	t.Run("api key comes from the encrypted credentials", func(t *testing.T) {
		encrypted, err := account.EncryptCredentials("", "s3cret")
		require.NoError(t, err)

		assert.Equal(t, "s3cret", llmAPIKey(&account.BasicInfo{
			EncryptedCredentials: encrypted,
		}))
	})

	t.Run("undecipherable credentials fall back to the plain password", func(t *testing.T) {
		assert.Equal(t, "s3cret", llmAPIKey(&account.BasicInfo{
			Password:             "s3cret",
			EncryptedCredentials: "not-a-valid-blob",
		}))
	})
}

func TestForeachSSE(t *testing.T) {
	t.Run("normal events are passed to callback", func(t *testing.T) {
		input := `data: {"object":"chat.completion.chunk","content":"hello"}

data: {"object":"chat.completion.chunk","content":"world"}

data: [DONE]
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.NoError(t, err)
		assert.Len(t, events, 2)
		assert.Equal(t, "hello", events[0]["content"])
		assert.Equal(t, "world", events[1]["content"])
	})

	t.Run("error event with code and message returns formatted error", func(t *testing.T) {
		input := `data: {"error":{"message":"Error while generating answer","code":"ERROR_ANSWER_GENERATION"}}
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.Error(t, err)
		assert.Equal(t, "ERROR_ANSWER_GENERATION: Error while generating answer", err.Error())
		assert.Empty(t, events)
	})

	t.Run("error event with only message returns error", func(t *testing.T) {
		input := `data: {"error":{"message":"Something went wrong"}}
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.Error(t, err)
		assert.Equal(t, "Something went wrong", err.Error())
		assert.Empty(t, events)
	})

	t.Run("error event with empty message returns unknown error", func(t *testing.T) {
		input := `data: {"error":{"message":"","code":"SOME_CODE"}}
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.Error(t, err)
		assert.Equal(t, "SOME_CODE: unknown streaming error", err.Error())
		assert.Empty(t, events)
	})

	t.Run("error event with no message field returns unknown error", func(t *testing.T) {
		input := `data: {"error":{}}
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.Error(t, err)
		assert.Equal(t, "unknown streaming error", err.Error())
		assert.Empty(t, events)
	})

	t.Run("DONE stops processing", func(t *testing.T) {
		input := `data: {"object":"first"}

data: [DONE]
data: {"object":"should not be processed"}
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.NoError(t, err)
		assert.Len(t, events, 1)
		assert.Equal(t, "first", events[0]["object"])
	})

	t.Run("invalid SSE format returns error", func(t *testing.T) {
		input := `invalid line without colon
`
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {})

		require.Error(t, err)
		assert.Equal(t, "invalid SSE response", err.Error())
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		input := `data: {invalid json}
`
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid character")
	})

	t.Run("comments are skipped", func(t *testing.T) {
		input := `: this is a comment
data: {"object":"event"}

data: [DONE]
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.NoError(t, err)
		assert.Len(t, events, 1)
	})

	t.Run("empty lines are skipped", func(t *testing.T) {
		input := `

data: {"object":"event"}


data: [DONE]
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.NoError(t, err)
		assert.Len(t, events, 1)
	})

	t.Run("non-data fields are skipped", func(t *testing.T) {
		input := `event: message
id: 123
data: {"object":"event"}

data: [DONE]
`
		var events []map[string]interface{}
		err := foreachSSE(strings.NewReader(input), func(event map[string]interface{}) {
			events = append(events, event)
		})

		require.NoError(t, err)
		assert.Len(t, events, 1)
	})
}

func TestGetSources(t *testing.T) {
	expected := []Source{
		{
			SourceType: "document",
			ID:         "file-1",
			DocType:    "io.cozy.files",
			Filename:   "report.pdf",
			Page:       3,
			FileURL:    "https://rag/static/c1",
			ChunkURL:   "https://rag/extract/c1",
		},
		{
			SourceType: "web",
			DocType:    "io.cozy.urls",
			URL:        "https://example.org",
			Title:      "Example",
			Snippet:    "snippet",
		},
	}
	web := `{"source_type": "web", "url": "https://example.org", "title": "Example", "snippet": "snippet"}`

	t.Run("extra object with the chunk metadata nested", func(t *testing.T) {
		event := parseEvent(t, `{"extra": {"sources": [
			{"source_type": "document", "rerank_score": 0.64,
			 "chunk": {"file_id": "file-1", "doctype": "io.cozy.files", "filename": "report.pdf", "page": 3},
			 "chunk_url": "https://rag/extract/c1", "file_url": "https://rag/static/c1"},
			`+web+`]}}`)
		sources, err := getSources(event)
		require.NoError(t, err)
		assert.Equal(t, expected, sources)
	})

	t.Run("legacy: extra JSON string with flat sources", func(t *testing.T) {
		extra := `{"sources": [
			{"source_type": "document", "file_id": "file-1", "doctype": "io.cozy.files", "filename": "report.pdf", "page": 3,
			 "chunk_url": "https://rag/extract/c1", "file_url": "https://rag/static/c1"},
			` + web + `]}`
		raw, err := json.Marshal(map[string]string{"extra": extra})
		require.NoError(t, err)
		sources, err := getSources(parseEvent(t, string(raw)))
		require.NoError(t, err)
		assert.Equal(t, expected, sources)
	})

	t.Run("no sources", func(t *testing.T) {
		for _, body := range []string{`{}`, `{"extra": {}}`, `{"extra": "{}"}`, `{"extra": ""}`, `{"extra": null}`} {
			sources, err := getSources(parseEvent(t, body))
			require.NoError(t, err, body)
			assert.Nil(t, sources, body)
		}
	})

	t.Run("legacy: invalid extra string", func(t *testing.T) {
		_, err := getSources(parseEvent(t, `{"extra": "not json"}`))
		require.Error(t, err)
	})
}

func TestHandleStreamResponseExtraFormats(t *testing.T) {
	config.UseTestFile(t)
	inst := &instance.Instance{Domain: "rag-stream.example.net"}
	msg := ChatMessage{ID: "msg-1"}
	source := `{"source_type": "document", "chunk": {"file_id": "file-1", "filename": "a.pdf"}}`

	for name, extras := range map[string][2]string{
		"extra object":         {`{}`, `{"sources": [` + source + `]}`},
		"legacy: extra string": {`"{}"`, strconv.Quote(`{"sources": [` + source + `]}`)},
	} {
		t.Run(name, func(t *testing.T) {
			body := `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "Hello "}}], "extra": ` + extras[0] + `}

data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "world"}}], "extra": ` + extras[1] + `}

data: {"object": "chat.completion.chunk", "choices": [{"delta": {}, "finish_reason": "stop"}], "extra": ` + extras[1] + `}

data: [DONE]
`
			answer, err := handleStreamResponse(inst, msg, strings.NewReader(body), &publishGate{})
			require.NoError(t, err)
			assert.Equal(t, "Hello world", answer.Content)
			require.Len(t, answer.Sources, 1)
			assert.Equal(t, "file-1", answer.Sources[0].ID)
			assert.Equal(t, "a.pdf", answer.Sources[0].Filename)
		})
	}
}

func parseEvent(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	var event map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(body), &event))
	return event
}

func TestRAGMessages(t *testing.T) {
	chat := &ChatConversation{Messages: []ChatMessage{
		{Role: SystemRole, Content: "prompt saved when the conversation was created"},
		{Role: UserRole, Content: "Hello"},
		{Role: AssistantRole, Content: "Hi"},
	}}
	turns := []ragMessage{
		{Role: UserRole, Content: "Hello"},
		{Role: AssistantRole, Content: "Hi"},
	}

	t.Run("without assistant", func(t *testing.T) {
		assert.Equal(t, turns, ragMessages(chat, nil))
	})

	t.Run("assistant without prompt", func(t *testing.T) {
		assert.Equal(t, turns, ragMessages(chat, &chatAssistant{}))
	})

	t.Run("assistant with a blank prompt", func(t *testing.T) {
		assert.Equal(t, turns, ragMessages(chat, &chatAssistant{Prompt: " \n"}))
	})

	t.Run("assistant with prompt", func(t *testing.T) {
		assistant := &chatAssistant{Prompt: " Be terse.\n"}
		expected := append([]ragMessage{{Role: SystemRole, Content: "Be terse."}}, turns...)
		assert.Equal(t, expected, ragMessages(chat, assistant))
	})

	t.Run("only the leading system messages are skipped", func(t *testing.T) {
		chat := &ChatConversation{Messages: []ChatMessage{
			{Role: SystemRole, Content: "saved prompt"},
			{Role: UserRole, Content: "Hello"},
			{Role: SystemRole, Content: "a note in the middle"},
		}}
		assert.Equal(t, []ragMessage{
			{Role: UserRole, Content: "Hello"},
			{Role: SystemRole, Content: "a note in the middle"},
		}, ragMessages(chat, nil))
	})
}

func TestInsertAfter(t *testing.T) {
	messages := []ChatMessage{{ID: "q1", Role: UserRole}, {ID: "a1", Role: AssistantRole}, {ID: "q2", Role: UserRole}}
	answer := ChatMessage{ID: "a2", Role: AssistantRole}

	ids := func(messages []ChatMessage) []string {
		var ids []string
		for _, msg := range messages {
			ids = append(ids, msg.ID)
		}
		return ids
	}
	assert.Equal(t, []string{"q1", "a1", "a2", "q2"}, ids(insertAfter(messages, "a1", answer)))
	assert.Equal(t, []string{"q1", "a1", "q2", "a2"}, ids(insertAfter(messages, "q2", answer)))
	assert.Equal(t, []string{"q1", "a1", "q2", "a2"}, ids(insertAfter(messages, "unknown", answer)), "appended when the message is not there")
	assert.Equal(t, []string{"q1", "a1", "q2"}, ids(messages), "the conversation is not changed in place")
}

// chatEvents returns a function giving the chat events published for the
// instance so far.
func chatEvents(t *testing.T, inst *instance.Instance) func() []map[string]interface{} {
	t.Helper()
	sub := realtime.GetHub().Subscriber(inst)
	sub.Subscribe(consts.ChatEvents)
	t.Cleanup(sub.Close)
	return func() []map[string]interface{} {
		var events []map[string]interface{}
		for {
			select {
			case e := <-sub.Channel:
				events = append(events, e.Doc.(*couchdb.JSONDoc).M)
			case <-time.After(50 * time.Millisecond):
				return events
			}
		}
	}
}

func objectsOf(events []map[string]interface{}) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i], _ = e["object"].(string)
	}
	return out
}

func TestHandleStreamResponseGate(t *testing.T) {
	config.UseTestFile(t)
	inst := &instance.Instance{Domain: "rag-gate.example.net"}
	msg := ChatMessage{ID: "msg-1"}
	source := `{"source_type": "document", "chunk": {"file_id": "file-1", "filename": "a.pdf"}}`
	// The stream opens with an empty delta, as the OpenAI API does
	body := `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"role": "assistant", "content": ""}}], "extra": {}}

data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "Hello "}}], "extra": {"sources": [` + source + `]}}

data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "world"}}], "extra": {}}

data: {"object": "chat.completion.chunk", "choices": [{"delta": {}, "finish_reason": "stop"}], "extra": {}}

data: [DONE]
`

	t.Run("the gate is asked once, for the first token", func(t *testing.T) {
		events := chatEvents(t, inst)
		asked := 0
		gate := &publishGate{allowed: func() bool { asked++; return true }}
		answer, err := handleStreamResponse(inst, msg, strings.NewReader(body), gate)
		require.NoError(t, err)
		assert.Equal(t, "Hello world", answer.Content)
		assert.Equal(t, 1, asked)
		published := events()
		require.Len(t, published, 3)
		assert.Equal(t, map[string]interface{}{"_id": "msg-1", "object": "delta", "content": "Hello ", "position": 0}, published[0],
			"the empty delta is not published and takes no position")
		assert.Equal(t, "sources", published[1]["object"])
		assert.Equal(t, map[string]interface{}{"_id": "msg-1", "object": "delta", "content": "world", "position": 1}, published[2])
	})

	t.Run("nothing is published when the gate refuses", func(t *testing.T) {
		events := chatEvents(t, inst)
		answer, err := handleStreamResponse(inst, msg, strings.NewReader(body), &publishGate{allowed: func() bool { return false }})
		require.NoError(t, err)
		assert.Equal(t, "Hello world", answer.Content, "the answer is still read")
		assert.Len(t, answer.Sources, 1)
		assert.Empty(t, events())
	})

	t.Run("an error after a published part is told", func(t *testing.T) {
		events := chatEvents(t, inst)
		broken := `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "Hel"}}], "extra": {}}

data: {"error": {"message": "LLM down", "type": "error", "code": "LLM_ERROR"}}

data: [DONE]
`
		_, err := handleStreamResponse(inst, msg, strings.NewReader(broken), &publishGate{})
		assert.EqualError(t, err, "LLM_ERROR: LLM down")
		assert.Len(t, events(), 1)
	})

	t.Run("the sources of the last chunk are published", func(t *testing.T) {
		events := chatEvents(t, inst)
		late := `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"content": "Hello"}}], "extra": {}}

data: {"object": "chat.completion.chunk", "choices": [{"delta": {}, "finish_reason": "stop"}], "extra": {"sources": [` + source + `]}}

data: [DONE]
`
		answer, err := handleStreamResponse(inst, msg, strings.NewReader(late), &publishGate{})
		require.NoError(t, err)
		assert.Len(t, answer.Sources, 1)
		assert.Equal(t, []string{"delta", "sources"}, objectsOf(events()))

		broken := strings.Replace(late, `"extra": {"sources": [`+source+`]}`, `"extra": "not json"`, 1)
		answer, err = handleStreamResponse(inst, msg, strings.NewReader(broken), &publishGate{})
		require.NoError(t, err, "a bad extra on the last chunk does not lose the answer")
		assert.Equal(t, "Hello", answer.Content)
		assert.Nil(t, answer.Sources)
	})

	t.Run("an error before any token is not published", func(t *testing.T) {
		events := chatEvents(t, inst)
		broken := `data: {"object": "chat.completion.chunk", "choices": [{"delta": {"role": "assistant", "content": ""}}], "extra": {}}

data: {"error": {"message": "LLM down", "type": "error", "code": "LLM_ERROR"}}

data: [DONE]
`
		_, err := handleStreamResponse(inst, msg, strings.NewReader(broken), &publishGate{})
		assert.Error(t, err)
		assert.Empty(t, events())
	})
}

func TestRAGRequest(t *testing.T) {
	inst := &instance.Instance{Domain: "rag-request.example.net"}
	messages := []ragMessage{{Role: UserRole, Content: "Hello"}}
	body, err := ragRequest(inst, messages, true, map[string]interface{}{"workspace": "ws-1"})
	require.NoError(t, err)
	assert.Contains(t, string(body), `"model":"ragondin-rag-request.example.net"`)
	assert.Contains(t, string(body), `"metadata":{"workspace":"ws-1"}`)
	assert.Contains(t, string(body), `"stream":true`)
	assert.NotContains(t, string(body), "max_tokens", "the default of openRAG")
}
