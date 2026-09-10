package rag

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
)

// rootPath is the full path of the root directory of the VFS.
const rootPath = "/"

// underFolder tells whether a file whose parent directory has the given full
// path is inside the folder with the given full path. Trashed content is
// never inside, even when the folder itself is in the trash. The root folder
// contains everything else.
func underFolder(parentPath, folderPath string) bool {
	if strings.HasPrefix(parentPath, vfs.TrashDirName) {
		return false
	}
	if folderPath == rootPath {
		return true
	}
	return parentPath == folderPath || strings.HasPrefix(parentPath, folderPath+"/")
}

// dirPathCache memoizes the full path of directories by id, for one run.
type dirPathCache map[string]string

// path resolves the full path of a directory. It returns os.ErrNotExist
// when the directory is gone, and any other lookup error as is: callers
// must treat the latter as "unknown", never as "out of scope".
func (c dirPathCache) path(fs vfs.VFS, dirID string) (string, error) {
	if p, ok := c[dirID]; ok {
		return p, nil
	}
	if dirID == consts.RootDirID {
		c[dirID] = rootPath
		return rootPath, nil
	}
	dir, err := fs.DirByID(dirID)
	if err != nil {
		return "", err
	}
	c[dirID] = dir.Fullpath
	return dir.Fullpath, nil
}

// scopes is the set of knowledge base folders of the assistants: what the
// rag-index job indexes. It is used from a single goroutine and is not safe
// for concurrent use.
type scopes struct {
	folders map[string]string // dir id → full path (rootPath for the root)
	dirs    dirPathCache
}

// loadScopes reads the knowledge base folders of every assistant. A folder
// that no longer exists is skipped with a warning; any other failure is
// retryable: the scopes are unknown and nothing must be deleted on a guess.
func loadScopes(inst *instance.Instance, logger logger.Logger) (*scopes, error) {
	sc := &scopes{folders: map[string]string{}, dirs: dirPathCache{}}
	err := couchdb.ForeachDocs(inst, consts.ChatAssistants, func(_ string, doc json.RawMessage) error {
		var assistant chatAssistant
		if err := json.Unmarshal(doc, &assistant); err != nil {
			return err
		}
		for _, entry := range assistant.KnowledgeBase {
			if entry.Doctype != consts.Files || entry.DirID == "" {
				continue
			}
			if _, seen := sc.folders[entry.DirID]; seen {
				continue
			}
			p, err := sc.dirs.path(inst.VFS(), entry.DirID)
			if errors.Is(err, os.ErrNotExist) {
				logger.Warnf("knowledge base folder %s of assistant %s does not exist: ignored", entry.DirID, assistant.DocID)
				continue
			}
			if err != nil {
				return err
			}
			sc.folders[entry.DirID] = p
		}
		return nil
	})
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return nil, retryable(err)
	}
	return sc, nil
}

// desiredFor returns, sorted, the ids of the knowledge base folders that
// contain a file whose parent directory has the given full path.
func (s *scopes) desiredFor(parentPath string) []string {
	var ids []string
	for dirID, folderPath := range s.folders {
		if underFolder(parentPath, folderPath) {
			ids = append(ids, dirID)
		}
	}
	slices.Sort(ids)
	return ids
}

// folderIDs returns the ids of the knowledge base folders, sorted.
func (s *scopes) folderIDs() []string {
	ids := make([]string, 0, len(s.folders))
	for id := range s.folders {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// diffWorkspaces compares the knowledge base folders with the workspaces
// openRAG holds: the folders without a workspace must get one, the
// workspaces without a folder must go. Both results are sorted.
func diffWorkspaces(folders map[string]string, existing []string) (toCreate, toRemove []string) {
	existingSet := make(map[string]bool, len(existing))
	for _, id := range existing {
		existingSet[id] = true
		if _, ok := folders[id]; !ok {
			toRemove = append(toRemove, id)
		}
	}
	for id := range folders {
		if !existingSet[id] {
			toCreate = append(toCreate, id)
		}
	}
	slices.Sort(toCreate)
	slices.Sort(toRemove)
	return toCreate, toRemove
}
