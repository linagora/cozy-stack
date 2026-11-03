package vfs

import (
	"path"

	"github.com/cozy/cozy-stack/pkg/consts"
)

type filePatherWithCache struct {
	fs      Indexer
	cache   map[string]string   // dirID -> parentPath
	missing map[string]struct{} // dirIDs of parents known to be missing
}

// NewFilePatherWithCache creates a FilePather that will cache the calls to
// CouchDB for finding the parent directories.
func NewFilePatherWithCache(fs Indexer) FilePather {
	return &filePatherWithCache{
		fs:    fs,
		cache: make(map[string]string),
	}
}

// NewFilePatherWithDirs creates a FilePather whose cache is filled with
// parent directories already loaded by the caller, keyed by ID. A nil entry
// marks a parent known to be missing: the files under it get
// ErrParentDoesNotExist without another request to CouchDB. The parents
// absent from the map are fetched as with NewFilePatherWithCache.
func NewFilePatherWithDirs(fs Indexer, dirs map[string]*DirDoc) FilePather {
	fp := &filePatherWithCache{
		fs:      fs,
		cache:   make(map[string]string, len(dirs)),
		missing: make(map[string]struct{}),
	}
	for id, dir := range dirs {
		if dir == nil || dir.Type != consts.DirType {
			fp.missing[id] = struct{}{}
		} else {
			fp.cache[id] = dir.Fullpath
		}
	}
	return fp
}

func (fp *filePatherWithCache) FilePath(doc *FileDoc) (string, error) {
	var parentPath string
	if doc.DirID == consts.RootDirID {
		parentPath = "/"
	} else if doc.DirID == consts.TrashDirID {
		parentPath = TrashDirName
	} else if cachedPath, ok := fp.cache[doc.DirID]; ok {
		parentPath = cachedPath
	} else if _, ok := fp.missing[doc.DirID]; ok {
		return "", ErrParentDoesNotExist
	} else {
		parent, err := fp.fs.DirByID(doc.DirID)
		if err != nil {
			return "", ErrParentDoesNotExist
		}
		parentPath = parent.Fullpath
		fp.cache[doc.DirID] = parentPath
	}
	return path.Join(parentPath, doc.DocName), nil
}
