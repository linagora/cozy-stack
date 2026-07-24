package ai

import (
	"io"
	"net/http"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// Chat is the route for asking a chat completion to AI.
func Chat(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.POST, consts.ChatConversations); err != nil {
		return middlewares.ErrForbidden
	}
	var payload rag.ChatPayload
	if err := c.Bind(&payload); err != nil {
		return err
	}
	payload.ChatConversationID = c.Param("id")
	inst := middlewares.GetInstance(c)
	for _, id := range payload.AttachmentIDs {
		fileDoc, err := inst.VFS().FileByID(id)
		if err != nil {
			return middlewares.ErrForbidden
		}
		if err := middlewares.Allow(c, permission.GET, fileDoc); err != nil {
			return middlewares.ErrForbidden
		}
	}
	if payload.AssistantID != "" {
		dirID, err := rag.AssistantKnowledgeBaseDirID(inst, payload.AssistantID)
		if err != nil {
			return jsonapi.InternalServerError(err)
		}
		if dirID != "" {
			dirDoc, err := inst.VFS().DirByID(dirID)
			if err != nil {
				return middlewares.ErrForbidden
			}
			if err := middlewares.Allow(c, permission.GET, dirDoc); err != nil {
				return middlewares.ErrForbidden
			}
		}
	}
	chat, err := rag.Chat(inst, payload)
	if err != nil {
		return jsonapi.InternalServerError(err)
	}
	return jsonapi.Data(c, http.StatusAccepted, chat, nil)
}

func callAI(c echo.Context, path string) (*http.Response, error) {
	if err := middlewares.AllowWholeType(c, permission.POST, consts.ChatConversations); err != nil {
		return nil, middlewares.ErrForbidden
	}
	if path != "v1/tools/execute" && path != "v1/chat/completions" {
		return nil, echo.NewHTTPError(http.StatusForbidden, "Invalid path")
	}
	inst := middlewares.GetInstance(c)

	body, err := io.ReadAll(c.Request().Body)
	if err != nil {
		return nil, echo.NewHTTPError(http.StatusBadRequest, "Unable to read request body")
	}
	contentType := c.Request().Header.Get("Content-Type")
	// TODO: handle streaming response
	res, err := rag.CallRAGQuery(inst, http.MethodPost, body, path, contentType)
	if err != nil {
		return nil, err
	}
	return res, nil
}

func OpenAICompletion(c echo.Context) error {
	res, err := callAI(c, "v1/chat/completions")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return c.Stream(res.StatusCode, "application/json", res.Body)
}

func ExecuteTool(c echo.Context) error {
	res, err := callAI(c, "v1/tools/execute")
	if err != nil {
		return err
	}
	defer res.Body.Close()
	return c.Stream(res.StatusCode, "application/json", res.Body)
}

// Routes sets the routing for the AI tasks.
func Routes(router *echo.Group) {
	router.POST("/chat/conversations/:id", Chat)
	router.POST("/v1/chat/completions", OpenAICompletion)
	router.POST("/v1/tools/execute", ExecuteTool)
}
