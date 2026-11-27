package files

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// AddNotSynchronizedOn is the echo.handler for adding not_synchronized_on to
// a directory
// POST /files/:file-id/relationships/not_synchronized_on
func AddNotSynchronizedOn(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")
	dir, err := instance.VFS().DirByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if err = middlewares.AllowVFS(c, permission.PATCH, dir); err != nil {
		return err
	}

	references, err := jsonapi.BindRelations(c.Request())
	if err != nil {
		return WrapVfsError(err)
	}

	dir.AddNotSynchronizedOn(references...)
	UpdateDirCozyMetadata(c, dir)
	if err = couchdb.UpdateDoc(instance, dir); err != nil {
		return WrapVfsError(err)
	}

	refs := dir.NotSynchronizedOn
	count := len(refs)
	meta := jsonapi.Meta{Rev: dir.Rev(), Count: &count}
	return jsonapi.DataRelations(c, http.StatusOK, refs, &meta, nil, nil)
}

// RemoveNotSynchronizedOn is the echo.handler for removing not_synchronized_on to
// a directory
// DELETE /files/:file-id/relationships/not_synchronized_on
func RemoveNotSynchronizedOn(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")
	dir, err := instance.VFS().DirByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	if err = middlewares.AllowVFS(c, permission.PATCH, dir); err != nil {
		return err
	}

	references, err := jsonapi.BindRelationsToRemove(c.Request())
	if err != nil {
		return WrapVfsError(err)
	}

	dir.RemoveNotSynchronizedOn(references...)
	UpdateDirCozyMetadata(c, dir)
	if err = couchdb.UpdateDoc(instance, dir); err != nil {
		return WrapVfsError(err)
	}

	refs := dir.NotSynchronizedOn
	count := len(refs)
	meta := jsonapi.Meta{Rev: dir.Rev(), Count: &count}
	return jsonapi.DataRelations(c, http.StatusOK, refs, &meta, nil, nil)
}

// ListNotSynchronizedOn list all directories not synchronized on a device
// GET /data/:type/:id/relationships/not_synchronizing
// Beware, this is actually used in the web/data Routes
func ListNotSynchronizedOn(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	id := getDocID(c)
	includeDocs := c.QueryParam("include") == "files"

	if err := middlewares.AllowTypeAndID(c, permission.GET, doctype, id); err != nil {
		if middlewares.AllowWholeType(c, permission.GET, consts.Files) != nil {
			return err
		}
	}

	cursor, err := jsonapi.ExtractPaginationCursor(c, defaultRefsPerPage, maxRefsPerPage)
	if err != nil {
		return err
	}

	start := []string{doctype, id}
	end := []string{doctype, id, couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    start,
		EndKey:      end,
		IncludeDocs: includeDocs,
	}
	cursor.ApplyTo(req)

	var res couchdb.ViewResponse
	if err := couchdb.ExecView(instance, couchdb.DirNotSynchronizedOnView, req, &res); err != nil {
		return err
	}

	cursor.UpdateFrom(&res)
	links := &jsonapi.LinksList{}
	if cursor.HasMore() {
		params, err2 := jsonapi.PaginationCursorToParams(cursor)
		if err2 != nil {
			return err2
		}
		links.Next = fmt.Sprintf("%s?%s",
			c.Request().URL.Path, params.Encode())
	}

	meta := &jsonapi.Meta{Count: &res.Total}

	refs := make([]couchdb.DocReference, len(res.Rows))
	var docs []jsonapi.Object
	if includeDocs {
		docs = make([]jsonapi.Object, len(res.Rows))
	}

	for i, row := range res.Rows {
		refs[i] = couchdb.DocReference{
			ID:   row.ID,
			Type: consts.Files,
		}

		if includeDocs {
			docs[i], err = rawMessageToObject(instance, row.Doc)
			if err != nil {
				return err
			}
		}
	}

	return jsonapi.DataRelations(c, http.StatusOK, refs, meta, links, docs)
}

// AddBulkNotSynchronizedOn add some not_synchronized_on for a device
// POST /data/:type/:id/relationships/not_synchronizing
// Beware, this is actually used in the web/data Routes
func AddBulkNotSynchronizedOn(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	id := getDocID(c)

	references, err := jsonapi.BindRelations(c.Request())
	if err != nil {
		return WrapVfsError(err)
	}

	docRef := couchdb.DocReference{
		Type: doctype,
		ID:   id,
	}

	if err = middlewares.AllowTypeAndID(c, permission.PUT, doctype, id); err != nil {
		if middlewares.AllowWholeType(c, permission.PATCH, consts.Files) != nil {
			return err
		}
	}

	docs := make([]interface{}, len(references))
	oldDocs := make([]interface{}, len(references))

	for i, ref := range references {
		dir, _, err := instance.VFS().DirOrFileByID(ref.ID)
		if err != nil {
			return WrapVfsError(err)
		}
		if dir == nil {
			return jsonapi.BadRequest(errors.New("Cannot add not_synchronized_on on files"))
		}
		oldDocs[i] = dir.Clone()
		dir.AddNotSynchronizedOn(docRef)
		UpdateDirCozyMetadata(c, dir)
		docs[i] = dir
	}

	// Use bulk update for better performances
	err = couchdb.BulkUpdateDocs(instance, consts.Files, docs, oldDocs)
	if err != nil {
		return WrapVfsError(err)
	}
	return c.NoContent(204)
}

// RemoveBulkNotSynchronizedOn removes some not_synchronized_on for several
// directories.
// DELETE /data/:type/:id/relationships/not_synchronizing
// Beware, this is actually used in the web/data Routes
func RemoveBulkNotSynchronizedOn(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	doctype := c.Param("doctype")
	id := getDocID(c)

	references, err := jsonapi.BindRelations(c.Request())
	if err != nil {
		return WrapVfsError(err)
	}

	docRef := couchdb.DocReference{
		Type: doctype,
		ID:   id,
	}

	if err := middlewares.AllowTypeAndID(c, permission.DELETE, doctype, id); err != nil {
		if middlewares.AllowWholeType(c, permission.PATCH, consts.Files) != nil {
			return err
		}
	}

	docs := make([]interface{}, len(references))
	oldDocs := make([]interface{}, len(references))

	for i, ref := range references {
		dir, _, err := instance.VFS().DirOrFileByID(ref.ID)
		if err != nil {
			return WrapVfsError(err)
		}
		if dir == nil {
			return jsonapi.BadRequest(errors.New("Cannot add not_synchronized_on on files"))
		}
		oldDocs[i] = dir.Clone()
		dir.RemoveNotSynchronizedOn(docRef)
		UpdateDirCozyMetadata(c, dir)
		docs[i] = dir
	}

	// Use bulk update for better performances
	err = couchdb.BulkUpdateDocs(instance, consts.Files, docs, oldDocs)
	if err != nil {
		return WrapVfsError(err)
	}
	return c.NoContent(204)
}

// NotSynchronizedOnRoutes adds the /data/:doctype/:docid/relationships/not_synchronizing routes.
func NotSynchronizedOnRoutes(router *echo.Group) {
	router.GET("/:docid/relationships/not_synchronizing", ListNotSynchronizedOn)
	router.POST("/:docid/relationships/not_synchronizing", AddBulkNotSynchronizedOn)
	router.DELETE("/:docid/relationships/not_synchronizing", RemoveBulkNotSynchronizedOn)
}
