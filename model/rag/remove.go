// model/rag/remove.go
package rag

import (
	"errors"
	"os"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
)

// removeFolder handles the "remove" action: the folder is no longer a
// knowledge base. Its files are detached from the workspace (and deleted
// from openRAG when nothing else claims them), then the workspace and the
// trigger's checkpoint are dropped. Idempotent.
func removeFolder(inst *instance.Instance, logger logger.Logger, dirID string) error {
	server := inst.RAGServer()
	if server.URL == "" {
		return errors.New("no RAG server configured")
	}
	global, err := globalTriggerExists(inst)
	if err != nil {
		return err
	}

	fs := inst.VFS()
	_, err = fs.DirByID(dirID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil {
		var errj error
		err = vfs.WalkByID(fs, dirID, func(_ string, _ *vfs.DirDoc, file *vfs.FileDoc, err error) error {
			if err != nil {
				return err
			}
			if file == nil || file.Trashed {
				return nil
			}
			if err := detachFile(inst, file.DocID, dirID, global, logger); err != nil {
				errj = errors.Join(errj, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if errj != nil {
			// Keep the workspace so a retry detaches the remaining files.
			return errj
		}
	} else {
		logger.Warnf("knowledge base folder %s is gone: dropping its workspace only", dirID)
	}

	if err := deleteWorkspace(server, inst.Domain, dirID); err != nil {
		return err
	}
	return deleteCheckpoint(inst, consts.Files, IndexMessage{Doctype: consts.Files, DirID: dirID}.checkpointID())
}
