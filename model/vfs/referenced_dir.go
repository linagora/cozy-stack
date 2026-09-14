package vfs

import (
	"strings"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

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

	if len(res.Rows) > 0 {
		dir, err := fs.DirByID(res.Rows[0].ID)
		if err != nil {
			return nil, err
		}
		if !strings.HasPrefix(dir.Fullpath, TrashDirName) {
			return dir, nil
		}
		return RestoreDir(fs, dir)
	}

	dir, err := NewDirDocWithPath(dirname, consts.RootDirID, "/", nil)
	if err != nil {
		return nil, err
	}
	dir.AddReferencedBy(ref)
	dir.CozyMetadata = NewCozyMetadata(createdOn)
	if err = fs.CreateDir(dir); err != nil {
		dir, err = fs.DirByPath(dir.Fullpath)
		if err != nil {
			return nil, err
		}
		olddoc := dir.Clone().(*DirDoc)
		dir.AddReferencedBy(ref)
		_ = fs.UpdateDirDoc(olddoc, dir)
	}
	return dir, nil
}
