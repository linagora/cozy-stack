package files

import (
	"net/http"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func lockVFS(inst *instance.Instance) func() {
	mu := config.Lock().ReadWrite(inst, "vfs")
	_ = mu.Lock()
	return mu.Unlock
}

func validateClientReferences(refs ...couchdb.DocReference) error {
	for _, ref := range refs {
		if _, reserved := vfs.MagicDirs[ref.ID]; reserved && ref.Type == consts.Apps {
			return jsonapi.Errorf(http.StatusBadRequest, "Magic folder reference %s is managed by the stack", ref.ID)
		}
	}
	return nil
}

// AddReferencedHandler is the echo.handler for adding referenced_by to
// a file
// POST /files/:file-id/relationships/referenced_by
func AddReferencedHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")
	fs := instance.VFS()
	dir, file, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.PATCH, dir, file)
	if err != nil {
		return err
	}

	references, err := jsonapi.BindRelations(c.Request())
	if err != nil {
		return WrapVfsError(err)
	}
	if err := validateClientReferences(references...); err != nil {
		return err
	}

	var oldFile *vfs.FileDoc
	if file != nil {
		oldFile = file.Clone().(*vfs.FileDoc)
		// Ensure the fullpaths are filled to realtime
		_, _ = file.Path(fs)
		_, _ = oldFile.Path(fs)
	}
	defer lockVFS(instance)()

	var newRev string
	var refs []couchdb.DocReference
	if dir != nil {
		dir.AddReferencedBy(references...)
		UpdateDirCozyMetadata(c, dir)
		err = couchdb.UpdateDoc(instance, dir)
		newRev = dir.Rev()
		refs = dir.ReferencedBy
	} else {
		file.AddReferencedBy(references...)
		UpdateFileCozyMetadata(c, file, false)
		err = couchdb.UpdateDocWithOld(instance, file, oldFile)
		newRev = file.Rev()
		refs = file.ReferencedBy
	}

	if err != nil {
		return WrapVfsError(err)
	}

	count := len(refs)
	meta := jsonapi.Meta{Rev: newRev, Count: &count}

	return jsonapi.DataRelations(c, http.StatusOK, refs, &meta, nil, nil)
}

// RemoveReferencedHandler is the echo.handler for removing referenced_by to
// a file
// DELETE /files/:file-id/relationships/referenced_by
func RemoveReferencedHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)

	fileID := c.Param("file-id")
	fs := instance.VFS()
	dir, file, err := fs.DirOrFileByID(fileID)
	if err != nil {
		return WrapVfsError(err)
	}

	err = checkPerm(c, permission.DELETE, nil, file)
	if err != nil {
		return err
	}

	references, err := jsonapi.BindRelations(c.Request())
	if err != nil {
		return WrapVfsError(err)
	}

	var oldFile *vfs.FileDoc
	if file != nil {
		oldFile = file.Clone().(*vfs.FileDoc)
		// Ensure the fullpaths are filled to realtime
		_, _ = file.Path(fs)
		_, _ = oldFile.Path(fs)
	}
	defer lockVFS(instance)()

	var newRev string
	var refs []couchdb.DocReference
	if dir != nil {
		dir.RemoveReferencedBy(references...)
		UpdateDirCozyMetadata(c, dir)
		err = couchdb.UpdateDoc(instance, dir)
		newRev = dir.Rev()
		refs = dir.ReferencedBy
	} else {
		file.RemoveReferencedBy(references...)
		UpdateFileCozyMetadata(c, file, false)
		err = couchdb.UpdateDocWithOld(instance, file, oldFile)
		newRev = file.Rev()
		refs = file.ReferencedBy
	}

	if err != nil {
		return WrapVfsError(err)
	}

	count := len(refs)
	meta := jsonapi.Meta{Rev: newRev, Count: &count}

	return jsonapi.DataRelations(c, http.StatusOK, refs, &meta, nil, nil)
}
