package ai

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatRejectsInvalidMessages(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})
	_, token := setup.GetTestClient(consts.ChatConversations + " " + consts.Files)

	ts := setup.GetTestServer("/ai", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	// The attached files are read from the documents
	e := testutils.CreateTestClient(t, ts.URL)
	e.POST("/ai/chat/conversations/conversation-attachments-without-documents").
		WithHeader("Authorization", "Bearer "+token).
		WithJSON(map[string]interface{}{
			"q":             "Summarize this file",
			"documents":     false,
			"attachmentIDs": []string{"a-file"},
		}).
		Expect().Status(400)

	var chat rag.ChatConversation
	err := couchdb.GetDoc(inst, consts.ChatConversations, "conversation-attachments-without-documents", &chat)
	assert.True(t, couchdb.IsNotFoundError(err), "the message is not saved")
}

func TestChatRejectsInvalidActions(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})
	_, token := setup.GetTestClient(consts.ChatConversations + " " + consts.Files)

	ts := setup.GetTestServer("/ai", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	e := testutils.CreateTestClient(t, ts.URL)
	e.POST("/ai/chat/conversations/conversation-invalid-actions").
		WithHeader("Authorization", "Bearer "+token).
		WithJSON(map[string]interface{}{
			"q": "Crée une tâche pour demain",
			"actions": []map[string]interface{}{{
				"name":        "create_task",
				"description": "create a task in the user's tasks",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{"due": map[string]interface{}{"type": "object"}},
				},
			}},
		}).
		Expect().Status(400)

	var chat rag.ChatConversation
	err := couchdb.GetDoc(inst, consts.ChatConversations, "conversation-invalid-actions", &chat)
	assert.True(t, couchdb.IsNotFoundError(err), "the message is not saved")
}

// ragQueries receives the messages of the rag-query jobs: the worker of the
// stack is not in this test binary, a dummy one records them.
var ragQueries = make(chan rag.QueryMessage, 8)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "rag-query",
		Concurrency: 1,
		WorkerFunc: func(ctx *job.TaskContext) error {
			var query rag.QueryMessage
			if err := ctx.UnmarshalMessage(&query); err != nil {
				return err
			}
			ragQueries <- query
			return nil
		},
	})
}

func TestChatAcceptsActions(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})
	_, token := setup.GetTestClient(consts.ChatConversations + " " + consts.Files)

	ts := setup.GetTestServer("/ai", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	raw, err := os.ReadFile("../../model/rag/testdata/chat_actions.json")
	require.NoError(t, err)
	var actions []map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &actions))

	e := testutils.CreateTestClient(t, ts.URL)
	e.POST("/ai/chat/conversations/conversation-with-actions").
		WithHeader("Authorization", "Bearer "+token).
		WithJSON(map[string]interface{}{"q": "Fais-en une note", "actions": actions}).
		Expect().Status(202)

	var chat rag.ChatConversation
	require.NoError(t, couchdb.GetDoc(inst, consts.ChatConversations, "conversation-with-actions", &chat))
	require.Len(t, chat.Messages, 1)
	assert.Equal(t, "Fais-en une note", chat.Messages[0].Content)

	select {
	case query := <-ragQueries:
		assert.Equal(t, "conversation-with-actions", query.DocID)
		require.Len(t, query.Actions, 3, "the query job carries the actions")
		assert.Equal(t, "create_note", query.Actions[0].Name)
		assert.Equal(t, "string", query.Actions[0].Parameters.Properties["content"].Type)
		assert.True(t, query.Actions[2].Parameters.Properties["to"].UserWritten)
	case <-time.After(5 * time.Second):
		t.Fatal("no rag-query job")
	}

	// The reserved name is refused through the API as well
	e.POST("/ai/chat/conversations/conversation-search-action").
		WithHeader("Authorization", "Bearer "+token).
		WithJSON(map[string]interface{}{
			"q":       "Cherche",
			"actions": []map[string]interface{}{{"name": "search", "description": "d"}},
		}).
		Expect().Status(400).Body().Contains(`invalid action name \"search\"`)
}
