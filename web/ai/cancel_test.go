package ai

import (
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCancelChat(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{})
	_, token := setup.GetTestClient(consts.ChatConversations)
	_, filesToken := setup.GetTestClient(consts.Files)

	ts := setup.GetTestServer("/ai", Routes)
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	chat := rag.ChatConversation{
		DocID:        "conversation-to-cancel",
		Messages:     []rag.ChatMessage{{ID: "waiting-message", Role: rag.UserRole, Content: "Bonjour", CreatedAt: time.Now()}},
		CozyMetadata: metadata.New(),
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, &chat))
	key := "rag-chat-cancel:" + inst.Domain + ":" + chat.DocID
	t.Cleanup(func() { config.GetConfig().CacheStorage.Clear(key) })

	t.Run("without the permission of the assistant", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		e.POST("/ai/chat/conversations/"+chat.DocID+"/cancel").
			WithHeader("Authorization", "Bearer "+filesToken).
			Expect().Status(403)
	})

	t.Run("an unknown conversation", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		e.POST("/ai/chat/conversations/unknown/cancel").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(404)
	})

	t.Run("asks the query of the waiting message to stop", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)
		e.POST("/ai/chat/conversations/"+chat.DocID+"/cancel").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(204)
		id, ok := config.GetConfig().CacheStorage.Get(key)
		require.True(t, ok)
		assert.Equal(t, "waiting-message", string(id))
	})
}
