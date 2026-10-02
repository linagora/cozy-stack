package ai

import (
	"testing"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
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
