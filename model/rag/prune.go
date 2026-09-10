package rag

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"slices"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// ErrUnknownFolder is returned by Reconcile when the given folder is in no
// assistant's knowledge base: there is nothing to index in it.
var ErrUnknownFolder = errors.New("the folder is in no assistant's knowledge base")

// PruneResult summarizes a Prune run.
type PruneResult struct {
	FilesScanned      int `json:"files_scanned"`
	FilesDeleted      int `json:"files_deleted"`
	WorkspacesDeleted int `json:"workspaces_deleted"`
}

// claimsFile tells whether a knowledge base folder claims the file: it must
// be live in the VFS, with a parent directory inside such a folder.
func (s *scopes) claimsFile(fs vfs.VFS, fileID string) (bool, error) {
	file, err := fs.FileByID(fileID)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if file.Trashed {
		return false, nil
	}
	parentPath, err := s.dirs.path(fs, file.DirID)
	if errors.Is(err, os.ErrNotExist) {
		// The parent directory is gone: the file is orphaned.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(s.desiredFor(parentPath)) > 0, nil
}

// listPartitionFiles returns the ids of the files openRAG holds for the
// instance. openRAG answers with links to each file; the id is the last
// path segment of the link.
func listPartitionFiles(server config.RAGServer, domain string) ([]string, error) {
	res, err := callRAG(server, http.MethodGet, nil, fmt.Sprintf("/partition/%s/", domain), echo.MIMEApplicationJSON)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if err := statusError("GET partition", res.StatusCode); err != nil {
		return nil, err
	}
	var body struct {
		Files []struct {
			Link string `json:"link"`
		} `json:"files"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Files))
	for _, f := range body.Files {
		if id := path.Base(f.Link); id != "" && id != "." && id != "/" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Prune deletes from openRAG what no knowledge base folder claims any more:
// the files gone or trashed in the VFS, the files outside every knowledge
// base folder, and the workspaces of the folders that are in no knowledge
// base. The index status of a deleted file is dropped too.
func Prune(inst *instance.Instance, logger logger.Logger) (PruneResult, error) {
	var result PruneResult
	server := inst.RAGServer()
	if server.URL == "" {
		return result, errors.New("no RAG server configured")
	}
	sc, err := loadScopes(inst, logger)
	if err != nil {
		return result, err
	}
	workspaces, err := listWorkspaces(server, inst.Domain)
	if err != nil {
		return result, err
	}
	// diffWorkspaces works on folder ids; what openRAG holds and what the
	// cleanup below deletes are workspace ids.
	_, staleFolders := diffWorkspaces(sc.folders, dirIDsForWorkspaces(workspaces))
	stale := workspaceIDsForDirs(staleFolders)
	ids, err := listPartitionFiles(server, inst.Domain)
	if err != nil {
		return result, err
	}
	fs := inst.VFS()
	for _, id := range ids {
		result.FilesScanned++
		keep, err := sc.claimsFile(fs, id)
		if err != nil {
			return result, err
		}
		if keep {
			// openRAG deletes the files a workspace deletion leaves without
			// any workspace: a kept file must leave the stale workspaces
			// first, or the cleanup below would take it along.
			if len(stale) > 0 {
				if err := removeMemberships(server, inst.Domain, id, stale); err != nil {
					return result, err
				}
			}
			continue
		}
		logger.Infof("prune: deleting unclaimed file %s from openRAG", id)
		if err := deleteFromRAG(inst, id); err != nil {
			return result, err
		}
		result.FilesDeleted++
	}
	for _, id := range stale {
		logger.Infof("prune: deleting workspace %s of a folder in no knowledge base", id)
		if err := deleteWorkspace(server, inst.Domain, id); err != nil {
			return result, err
		}
		result.WorkspacesDeleted++
	}
	return result, nil
}

// removeMemberships takes the file out of those of the given workspaces it
// is a member of.
func removeMemberships(server config.RAGServer, domain, fileID string, workspaces []string) error {
	current, found, err := fileWorkspaces(server, domain, fileID)
	if err != nil || !found {
		return err
	}
	for _, ws := range current {
		if !slices.Contains(workspaces, ws) {
			continue
		}
		if err := removeMembership(server, domain, ws, fileID); err != nil {
			return err
		}
	}
	return nil
}

// resetCheckpoint deletes the checkpoint under the lock the rag-index jobs
// take, so a batch already running cannot save its own LastSeq over the
// reset.
func resetCheckpoint(inst *instance.Instance) error {
	mu := config.Lock().LongOperation(inst, indexLockName)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()
	return deleteCheckpoint(inst, consts.Files, checkpointDocID)
}

// Reset drops the checkpoint and launches a rag-index job, forcing a full
// re-index from the beginning of the changes feed.
func Reset(inst *instance.Instance) error {
	if err := resetCheckpoint(inst); err != nil {
		return err
	}
	msg, err := job.NewMessage(IndexMessage{Doctype: consts.Files})
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: workerType,
		Message:    msg,
		Manual:     true,
	})
	return err
}

// Reconcile pushes a reconcile job for the knowledge base folder, or one per
// knowledge base folder when dirID is empty. It returns the number of jobs
// pushed.
func Reconcile(inst *instance.Instance, dirID string) (int, error) {
	sc, err := loadScopes(inst, inst.Logger().WithNamespace("rag"))
	if err != nil {
		return 0, err
	}
	folders := sc.folderIDs()
	if dirID != "" {
		if _, ok := sc.folders[dirID]; !ok {
			return 0, ErrUnknownFolder
		}
		folders = []string{dirID}
	}
	n := 0
	for _, id := range folders {
		// pushReconcileJob on purpose, not the pushReconcile variable: the
		// variable only exists so the tests of Index can record the pushes,
		// while TestReconcile exercises the real job push.
		if err := pushReconcileJob(inst, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
