package office

import (
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/office"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// Open returns the parameters to open an office document.
func Open(c echo.Context) error {
	return OpenWithReadOnly(c, c.QueryParam("ReadOnly") == "true")
}

// OpenWithReadOnly is like Open, with the read-only flag given explicitly
// instead of read from the query.
func OpenWithReadOnly(c echo.Context, readOnly bool) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("file-id")
	open, err := sharing.OpenOffice(inst, fileID, files.ActorInstance(c))
	if err != nil {
		return wrapError(err)
	}
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	memberIndex, _ := strconv.Atoi(c.QueryParam("MemberIndex"))

	// If a directory is shared by link and contains an office document, the
	// document can be opened with the same sharecode as the directory. The
	// sharecode is also used to identify the member that previews a sharing.
	if pdoc.Type == permission.TypeShareByLink ||
		pdoc.Type == permission.TypeSharePreview ||
		pdoc.Type == permission.TypeShareInteract {
		code := middlewares.GetRequestToken(c)
		open.AddShareByLinkCode(code)
		if !readOnly {
			readOnly = true
			for _, perm := range pdoc.Permissions {
				if perm.Type == consts.Files && !perm.Verbs.ReadOnly() {
					readOnly = false
				}
			}
		}
	}

	sharingID := c.QueryParam("SharingID") // Cozy to Cozy sharing
	if err := open.CheckPermission(pdoc, sharingID); err != nil {
		return middlewares.ErrForbidden
	}

	doc, err := open.GetResult(memberIndex, readOnly)
	if err != nil {
		return wrapError(err)
	}

	return jsonapi.Data(c, http.StatusOK, doc, nil)
}

func FileByKey(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	file, err := office.EnsureFileForKey(inst, c.Param("key"))
	if err != nil {
		return wrapError(err)
	}
	if err := middlewares.AllowVFS(c, permission.GET, file); err != nil {
		return err
	}
	doc := files.NewFile(file, inst, nil)
	return jsonapi.Data(c, http.StatusOK, doc, nil)
}

// Callback is the handler for OnlyOffice callback requests.
// Cf https://api.onlyoffice.com/editors/callback
func Callback(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	var params office.CallbackParameters
	if err := c.Bind(&params); err != nil {
		inst.Logger().WithNamespace("office").
			Warnf("Cannot bind callback parameters: %s", err)
		return c.JSON(http.StatusBadRequest, echo.Map{"error": "Invalid request"})
	}
	header := c.Request().Header.Get(echo.HeaderAuthorization)
	params.Token = strings.TrimPrefix(header, "Bearer ")

	if err := office.Callback(inst, params); err != nil {
		inst.Logger().WithNamespace("office").
			Infof("Error on the callback: %s", err)
		code := http.StatusInternalServerError
		if httpError, ok := err.(*echo.HTTPError); ok {
			code = httpError.Code
		}
		return c.JSON(code, echo.Map{"error": err.Error()})
	}
	return c.JSON(http.StatusOK, echo.Map{"error": 0})
}

// Routes sets the routing for the collaborative edition of office documents.
func Routes(router *echo.Group) {
	router.GET("/:file-id/open", Open)
	router.POST("/keys/:key", FileByKey)
	router.POST("/callback", Callback)
}

func wrapError(err error) *jsonapi.Error {
	switch err {
	case office.ErrNoServer, office.ErrInvalidFile, sharing.ErrCannotOpenFile:
		return jsonapi.NotFound(err)
	case office.ErrInternalServerError:
		return jsonapi.InternalServerError(err)
	case office.ErrInvalidKey:
		return jsonapi.NotFound(err)
	case os.ErrNotExist, vfs.ErrParentDoesNotExist, vfs.ErrParentInTrash:
		return jsonapi.NotFound(err)
	case sharing.ErrMemberNotFound:
		return jsonapi.NotFound(err)
	}
	return jsonapi.InternalServerError(err)
}
