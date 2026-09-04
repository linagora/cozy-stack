// model/rag/scope.go
package rag

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
)

// scope is what a scoped rag-index job knows: the knowledge base folder it
// indexes, and whether a global trigger also claims every file of the
// instance. It is used from a single goroutine and is not safe for
// concurrent use.
type scope struct {
	dirID        string
	path         string // full path of the folder, possibly under the trash
	globalExists bool
	dirPaths     map[string]string // dir id -> full path, per-run cache
}

// newScope resolves the folder, makes sure its workspace exists on openRAG
// and looks up the global trigger. It returns (nil, nil) when the folder
// does not exist: there is nothing to index.
func newScope(inst *instance.Instance, server config.RAGServer, logger logger.Logger, dirID string) (*scope, error) {
	dir, err := inst.VFS().DirByID(dirID)
	if errors.Is(err, os.ErrNotExist) {
		logger.Warnf("knowledge base folder %s does not exist: nothing to index", dirID)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := ensureWorkspaceExists(server, inst.Domain, dirID, dir.DocName, logger); err != nil {
		return nil, err
	}
	global, err := globalTriggerExists(inst)
	if err != nil {
		return nil, err
	}
	return &scope{
		dirID:        dirID,
		path:         dir.Fullpath,
		globalExists: global,
		dirPaths:     map[string]string{dirID: dir.Fullpath},
	}, nil
}

// contains tells whether a file whose parent directory has the given full
// path is under the scope. Trashed content is never in scope, even when the
// folder itself is in the trash.
func (s *scope) contains(parentPath string) bool {
	if strings.HasPrefix(parentPath, vfs.TrashDirName) {
		return false
	}
	return parentPath == s.path || strings.HasPrefix(parentPath, s.path+"/")
}

func (s *scope) cachePath(dirID, path string) {
	s.dirPaths[dirID] = path
}

// parentPath resolves and caches the full path of a directory. The boolean
// is false when the path could not be resolved: callers must treat that as
// "unknown", never as "out of scope".
func (s *scope) parentPath(inst *instance.Instance, dirID string) (string, bool) {
	if p, ok := s.dirPaths[dirID]; ok {
		return p, true
	}
	dir, err := inst.VFS().DirByID(dirID)
	if err != nil {
		return "", false
	}
	s.dirPaths[dirID] = dir.Fullpath
	return dir.Fullpath, true
}

// globalTriggerExists tells whether the instance has a rag-index trigger
// without dir_id, i.e. one that indexes every file.
func globalTriggerExists(inst *instance.Instance) (bool, error) {
	triggers, err := job.System().GetAllTriggers(inst)
	if err != nil {
		return false, err
	}
	for _, t := range triggers {
		if isGlobalIndexTrigger(t.Infos()) {
			return true, nil
		}
	}
	return false, nil
}

func isGlobalIndexTrigger(infos *job.TriggerInfos) bool {
	if infos.WorkerType != workerType {
		return false
	}
	var msg IndexMessage
	if len(infos.Message) > 0 {
		if err := json.Unmarshal(infos.Message, &msg); err != nil {
			return false
		}
	}
	return msg.DirID == "" && msg.Action == ""
}
