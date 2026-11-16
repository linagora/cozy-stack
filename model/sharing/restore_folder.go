package sharing

import (
	"errors"
	"os"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/vfs"
)

// ErrNoRestoreFolder is returned by RestoreFolder when a trashed item has no
// folder where it would be restored.
var ErrNoRestoreFolder = errors.New("sharing: no folder to restore the item into")

// RestoreFolder returns the folder where restoring a trashed file or folder
// writes. A trashed item is outside every sharing scope, so its access is
// resolved on this folder: the first existing ancestor on its restore path
// (the restore recreates the missing hierarchy under it via MkdirAll).
// Exactly one of dir and file must be non-nil. The other errors are those of
// the VFS.
func RestoreFolder(fs vfs.VFS, dir *vfs.DirDoc, file *vfs.FileDoc) (*vfs.DirDoc, error) {
	var restorePath, docPath string
	if dir != nil {
		restorePath = dir.RestorePath
		docPath = dir.Fullpath
	} else {
		var err error
		restorePath = file.RestorePath
		docPath, err = file.Path(fs)
		if err != nil {
			return nil, err
		}
	}

	// A file trashed with its parent hierarchy has no restore path of its
	// own: resolve it from the trashed root folder, like getRestoreDir does.
	if restorePath == "" {
		rel := strings.TrimPrefix(docPath, vfs.TrashDirName+"/")
		split := strings.Index(rel, "/")
		if split < 0 {
			// ponytail: no resolvable restore root; getRestoreDir would fall
			// back to the instance root, which is in no sharing scope.
			return nil, ErrNoRestoreFolder
		}
		root, err := fs.DirByPath(vfs.TrashDirName + "/" + rel[:split])
		if err != nil {
			return nil, err
		}
		rest := path.Dir(rel[split+1:])
		restorePath = path.Join(root.RestorePath, root.DocName, rest)
	}
	// The restore path is the folder the file or directory is restored into
	// (and the directory itself is recreated inside it): that is where the
	// write happens.
	parentPath := restorePath

	// Walk up to the first existing ancestor: the restore recreates the
	// missing hierarchy under it, so that ancestor is where the write
	// actually happens.
	for {
		ancestor, err := fs.DirByPath(parentPath)
		if err == nil {
			return ancestor, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		next := path.Dir(parentPath)
		if next == parentPath {
			return nil, ErrNoRestoreFolder
		}
		parentPath = next
	}
}
