package vfs

import (
	"errors"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// EnsureReferencedDir returns the active directory with the reference,
// or creates it at the root, reusing an existing directory with the given name.
func EnsureReferencedDir(db prefixer.Prefixer, fs VFS, ref couchdb.DocReference, dirname, createdOn string) (*DirDoc, error) {
	key := []string{ref.Type, ref.ID}
	end := []string{ref.Type, ref.ID, couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    key,
		EndKey:      end,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(db, couchdb.FilesReferencedByView, req, &res)
	if err != nil {
		return nil, err
	}

	// assume one directory per reference; revisit selection if duplicates need support.
	if len(res.Rows) > 0 {
		dir, err := fs.DirByID(res.Rows[0].ID)
		if err != nil {
			return nil, err
		}
		if dir.Fullpath != TrashDirName && !strings.HasPrefix(dir.Fullpath, TrashDirName+"/") {
			return dir, nil
		}
	}

	dir, err := NewDirDocWithPath(dirname, consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	dir.AddReferencedBy(ref)
	dir.CozyMetadata = NewCozyMetadata(createdOn)
	if err = fs.CreateDir(dir); errors.Is(err, os.ErrExist) {
		dir, err = fs.DirByPath(dir.Fullpath)
		if err != nil {
			return nil, err
		}
		if containsDocReference(dir.ReferencedBy, ref) {
			return dir, nil
		}
		olddoc := dir.Clone().(*DirDoc)
		dir.AddReferencedBy(ref)
		err = fs.UpdateDirDoc(olddoc, dir)
	}
	if err != nil {
		return nil, err
	}
	return dir, nil
}
