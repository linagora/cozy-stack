package ai

import (
	"errors"
	"io"
	"net/http"

	"github.com/cozy/cozy-stack/model/meet"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
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
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return middlewares.ErrForbidden
	}
	chat, err := rag.Chat(inst, payload)
	if err != nil {
		return jsonapi.InternalServerError(err)
	}
	return jsonapi.Data(c, http.StatusAccepted, chat, nil)
}

// CancelChat is the route for stopping the answer to the last message of a
// conversation, when the user stops it.
func CancelChat(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.POST, consts.ChatConversations); err != nil {
		return middlewares.ErrForbidden
	}
	inst := middlewares.GetInstance(c)
	err := rag.CancelChat(inst, c.Param("id"))
	if couchdb.IsNotFoundError(err) {
		return jsonapi.NotFound(err)
	}
	if err != nil {
		return jsonapi.InternalServerError(err)
	}
	return c.NoContent(http.StatusNoContent)
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

// CreateMeeting is the route the assistant uses to create a video meeting
// room, once the user has confirmed the start_meeting action.
func CreateMeeting(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.POST, consts.ChatConversations); err != nil {
		return middlewares.ErrForbidden
	}
	inst := middlewares.GetInstance(c)
	room, err := meet.CreateRoom(c.Request().Context(), inst)
	if errors.Is(err, meet.ErrNotConfigured) {
		return jsonapi.NotFound(err)
	}
	if errors.Is(err, meet.ErrNoEmail) {
		return jsonapi.PreconditionFailed("email", err)
	}
	if err != nil {
		inst.Logger().WithNamespace("ai").Warnf("cannot create a meeting room: %s", err)
		return jsonapi.NewError(http.StatusBadGateway, err.Error())
	}
	return c.JSON(http.StatusCreated, room)
}

// Routes sets the routing for the AI tasks.
func Routes(router *echo.Group) {
	router.POST("/chat/conversations/:id", Chat)
	router.POST("/chat/conversations/:id/cancel", CancelChat)
	router.POST("/v1/chat/completions", OpenAICompletion)
	router.POST("/v1/tools/execute", ExecuteTool)
	router.POST("/meetings", CreateMeeting)
	router.POST("/index/status", IndexStatus)
}
