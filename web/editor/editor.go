// Package editor is about the routes used to open files with editors.
package editor

import (
	"net/http"
	"os"
	"strconv"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// OpenURL is the API handler for GET /editor/:file-id/open. It returns the
// parameters to build the URL where the file can be opened by an editor.
func OpenURL(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	fileID := c.Param("file-id")
	open, err := sharing.OpenEditor(inst, fileID)
	if err != nil {
		return wrapError(err)
	}

	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	memberIndex, _ := strconv.Atoi(c.QueryParam("MemberIndex"))
	readOnly := c.QueryParam("ReadOnly") == "true"

	// If a directory is shared by link and contains the file, it can be opened
	// with the same sharecode as the directory. The sharecode is also used to
	// identify the member that previews a sharing.
	if pdoc.Type == permission.TypeShareByLink ||
		pdoc.Type == permission.TypeSharePreview ||
		pdoc.Type == permission.TypeShareInteract {
		code := middlewares.GetRequestToken(c)
		open.AddShareByLinkCode(code)
		readOnly = readOnlyForSharedOpen(pdoc, readOnly)
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

// Routes sets the routing for opening files with editors.
func Routes(router *echo.Group) {
	router.GET("/:file-id/open", OpenURL)
}

func readOnlyForSharedOpen(pdoc *permission.Permission, readOnly bool) bool {
	if readOnly {
		return true
	}
	for _, perm := range pdoc.Permissions {
		if perm.Type == consts.Files && !perm.Verbs.ReadOnly() {
			return false
		}
	}
	return true
}

func wrapError(err error) *jsonapi.Error {
	switch err {
	case sharing.ErrCannotOpenFile, sharing.ErrMemberNotFound:
		return jsonapi.NotFound(err)
	case os.ErrNotExist, vfs.ErrParentDoesNotExist, vfs.ErrParentInTrash:
		return jsonapi.NotFound(err)
	}
	return jsonapi.InternalServerError(err)
}
