package files

import (
	"errors"
	"os"

	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

func lookupPath(fs vfs.VFS, scope sharing.FilePathScope, p string) (*vfs.DirDoc, *vfs.FileDoc, error) {
	if scope.Drive == nil && scope.Roots == nil {
		return fs.DirOrFileByPath(p)
	}
	abs := scope.Absolute(p)
	if abs != "" {
		dir, file, err := fs.DirOrFileByPath(abs)
		if err == nil {
			return dir, file, nil
		}
		if !os.IsNotExist(err) && !errors.Is(err, vfs.ErrNonAbsolutePath) {
			logger.WithNamespace("files").Warnf("shared path lookup: %s", err)
			return nil, nil, jsonapi.InternalServerError(errors.New("shared path lookup failed"))
		}
	}
	return nil, nil, pathNotFound(scope)
}

func pathNotFound(scope sharing.FilePathScope) error {
	if scope.Drive != nil {
		return jsonapi.NotFound(errors.New("shared drive target not found"))
	}
	return jsonapi.NotFound(errors.New("link target not found"))
}

func fileFromRequestPath(c echo.Context, drive *sharing.Sharing, p string) (*vfs.FileDoc, error) {
	scope, err := filePathScope(c, drive)
	if err != nil {
		return nil, err
	}
	fs := middlewares.GetInstance(c).VFS()
	if scope.Drive == nil && scope.Roots == nil {
		doc, err := fs.FileByPath(p)
		if err != nil {
			return nil, WrapVfsError(err)
		}
		return doc, nil
	}
	_, file, err := lookupPath(fs, scope, p)
	if err == nil && file == nil {
		return nil, pathNotFound(scope)
	}
	return file, err
}

func targetID(dir *vfs.DirDoc, file *vfs.FileDoc) string {
	if dir != nil {
		return dir.ID()
	}
	return file.ID()
}
