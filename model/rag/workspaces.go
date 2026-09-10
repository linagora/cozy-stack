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
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// rootWorkspaceName is the display name of the workspace of the root folder.
const rootWorkspaceName = "Drive"

// listWorkspaces returns the ids of the workspaces openRAG holds for the
// instance: workspace ids, not folder ids (see workspaceIDForDir). A 404
// means the partition does not exist yet (it is created with the first
// workspace): the instance simply has no workspace. Every other failure is
// retryable, so the caller keeps its checkpoint instead of diffing on a
// partial view.
func listWorkspaces(server config.RAGServer, domain string) ([]string, error) {
	res, err := callRAG(server, http.MethodGet, nil, fmt.Sprintf("/partition/%s/workspaces", domain), echo.MIMEApplicationJSON)
	if err != nil {
		return nil, retryable(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if res.StatusCode >= 300 {
		return nil, retryable(fmt.Errorf("GET workspaces status code: %d", res.StatusCode))
	}
	var body struct {
		Workspaces []struct {
			ID string `json:"workspace_id"`
		} `json:"workspaces"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(body.Workspaces))
	for _, ws := range body.Workspaces {
		ids = append(ids, ws.ID)
	}
	return ids, nil
}

// workspaceDisplayName is the name openRAG shows for a folder's workspace.
func workspaceDisplayName(dirID, fullpath string) string {
	if dirID == consts.RootDirID {
		return rootWorkspaceName
	}
	return path.Base(fullpath)
}

// reconcileWorkspaces aligns the openRAG workspaces of the partition with the
// knowledge base folders. A folder without workspace gets one, and a
// reconcile job is then pushed for its subtree; a workspace without folder
// loses its files (deleted when no membership remains) and is deleted.
// Creation failures are only logged (the next run retries); removal failures
// are returned so the job reports them, the workspace staying for the next
// run.
//
// pushReconcile is nil when the caller is itself the reconcile job of the
// folder own: the invariant is then that such a job neither pushes reconcile
// jobs, nor creates or removes any workspace but own. Creating another
// folder's workspace would strand that folder: nothing pushes its reconcile
// job any more, its files did not change so the feed carries nothing about
// them, and checkWorkspace already reports it as indexed.
func reconcileWorkspaces(inst *instance.Instance, logger logger.Logger, server config.RAGServer, sc *scopes, own string, pushReconcile func(dirID string) error) error {
	existing, err := listWorkspaces(server, inst.Domain)
	if err != nil {
		return err
	}
	// The diff works on folder ids: what openRAG lists are workspace ids.
	toCreate, toRemove := diffWorkspaces(sc.folders, dirIDsForWorkspaces(existing))
	if pushReconcile == nil {
		// A reconcile job only makes sure its own workspace is there, so its
		// uploads do not 404; the other folders, and the removals, are left
		// to a feed job.
		toRemove = nil
		toCreate = slices.DeleteFunc(toCreate, func(dirID string) bool { return dirID != own })
	}
	for _, dirID := range toCreate {
		// The workspace first: the reconcile job uploads into it, and a
		// creation that keeps failing (openRAG refusing the id, say) would
		// otherwise have every run push a whole-subtree job for nothing.
		workspaceID := workspaceIDForDir(dirID)
		name := workspaceDisplayName(dirID, sc.folders[dirID])
		if err := ensureWorkspaceExists(server, inst.Domain, workspaceID, name, logger); err != nil {
			logger.Warnf("cannot create the workspace of folder %s: %s", dirID, err)
			continue
		}
		if pushReconcile == nil {
			continue
		}
		if err := pushReconcile(dirID); err != nil {
			logger.Warnf("cannot push the reconcile job of folder %s: %s", dirID, err)
			// Roll back: an empty workspace claims the folder is indexed
			// (checkWorkspace succeeds) while nothing walks its subtree any
			// more. Deleting it makes the next run retry both.
			if err := deleteWorkspace(server, inst.Domain, workspaceID); err != nil {
				logger.Warnf("cannot delete the workspace %s after a failed push: %s", workspaceID, err)
			}
		}
	}
	var errj error
	for _, dirID := range toRemove {
		if err := removeWorkspace(inst, logger, server, workspaceIDForDir(dirID)); err != nil {
			logger.Warnf("cannot remove the workspace of folder %s: %s", dirID, err)
			errj = errors.Join(errj, err)
		}
	}
	return errj
}

// removeWorkspace detaches the live files of the folder from its workspace
// (deleting from openRAG those that belong to no other workspace), then
// deletes the workspace. It takes a workspace id; the folder it stands for
// is resolved with dirIDForWorkspace. A folder gone from the VFS only loses
// its workspace: its files were deleted through the changes feed.
func removeWorkspace(inst *instance.Instance, logger logger.Logger, server config.RAGServer, workspaceID string) error {
	dirID := dirIDForWorkspace(workspaceID)
	dir, err := inst.VFS().DirByID(dirID)
	switch {
	case errors.Is(err, os.ErrNotExist):
		logger.Infof("workspace %s matches no folder: deleting it", workspaceID)
	case err != nil:
		return retryable(err)
	default:
		if err := detachFolderFiles(inst, logger, dir, workspaceID); err != nil {
			return err
		}
	}
	return deleteWorkspace(server, inst.Domain, workspaceID)
}

// detachFolderFiles detaches every live file of the folder's subtree from
// the workspace, and reports the files that could not be.
func detachFolderFiles(inst *instance.Instance, logger logger.Logger, dir *vfs.DirDoc, workspaceID string) error {
	var errj error
	err := vfs.WalkAlreadyLocked(inst.VFS(), dir, func(_ string, _ *vfs.DirDoc, file *vfs.FileDoc, err error) error {
		if err != nil {
			return err
		}
		if file == nil || file.Trashed {
			return nil
		}
		if err := detachFile(inst, file.DocID, workspaceID, logger); err != nil {
			errj = errors.Join(errj, err)
		}
		return nil
	})
	return errors.Join(err, errj)
}
