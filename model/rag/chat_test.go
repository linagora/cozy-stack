package rag

import (
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/pkg/config/config"
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
