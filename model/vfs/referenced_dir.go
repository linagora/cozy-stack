package vfs

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// MagicDir is a directory found or created on demand through its reference.
type MagicDir struct {
	// Name is the translation key of the directory name.
	Name string
	// Parent is the ID of the directory, magic or not, the directory has to
	// stay in. Without it, the directory is followed wherever it is moved.
	Parent string
}

// DownloadsDirID is the magic ID of the directory gathering the files saved
// from the other applications.
const DownloadsDirID = consts.Apps + "/downloads"

// MagicDirs lists the magic directories by ID.
var MagicDirs = map[string]MagicDir{
	consts.Apps + "/" + consts.NotesSlug: {Name: "Tree Notes"},
	DownloadsDirID:                       {Name: "Tree Downloads", Parent: consts.RootDirID},
	consts.Apps + "/calendar":            {Name: "Tree Calendar", Parent: DownloadsDirID},
	consts.Apps + "/chat":                {Name: "Tree Chat", Parent: DownloadsDirID},
	consts.Apps + "/mail":                {Name: "Tree Mail", Parent: DownloadsDirID},
	consts.Apps + "/meetings":            {Name: "Tree Meeting Recordings", Parent: DownloadsDirID},
}

// withoutPlacedMagicRefs drops the references of the magic directories that
// have to stay in their parent.
func withoutPlacedMagicRefs(refs []couchdb.DocReference) []couchdb.DocReference {
	var kept []couchdb.DocReference
	for _, ref := range refs {
		if ref.Type != consts.Apps || MagicDirs[ref.ID].Parent == "" {
			kept = append(kept, ref)
		}
	}
	return kept
}

// EnsureReferencedDir returns the active directory with the reference, or
// creates it, reusing an existing directory with the given name. With a
// parent, only a directory directly inside it counts and a missing one is
// created there. Without it, the directory is found wherever it was moved and
// is created at the root.
func EnsureReferencedDir(db prefixer.Prefixer, fs VFS, ref couchdb.DocReference, parent *DirDoc, dirname, createdOn string) (*DirDoc, error) {
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

	var match *DirDoc
	for _, row := range res.Rows {
		var dir DirDoc
		if err := json.Unmarshal(row.Doc, &dir); err != nil {
			return nil, err
		}
		if dir.Type != consts.DirType || IsInTrash(dir.Fullpath) {
			continue
		}
		if parent == nil {
			return &dir, nil
		}
		if dir.DirID == parent.ID() {
			if match == nil {
				match = &dir
			}
			continue
		}
		// A legacy folder outside its parent must stop matching reference-based shares.
		updated := dir.Clone().(*DirDoc)
		updated.RemoveReferencedBy(ref)
		if err := fs.UpdateDirDoc(&dir, updated); err != nil {
			return nil, err
		}
	}
	if match != nil {
		return match, nil
	}

	parentID, parentPath := consts.RootDirID, "/"
	if parent != nil {
		parentID, parentPath = parent.ID(), parent.Fullpath
	}
	dir, err := NewDirDocWithPath(dirname, parentID, parentPath, nil)
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
