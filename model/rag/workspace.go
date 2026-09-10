package rag

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// ErrWorkspaceMissing is returned by checkWorkspace when the knowledge base
// folder has no workspace on openRAG: no rag-index job has reconciled it
// yet.
var ErrWorkspaceMissing = errors.New("the knowledge base folder is not indexed yet")

// rootWorkspaceID is the openRAG workspace id of the root folder, the
// knowledge base of a whole-Drive assistant.
const rootWorkspaceID = "io-cozy-files-root-dir"

// wellKnownWorkspaceIDs maps the well-known folder ids of the VFS to the
// workspace ids openRAG accepts. openRAG only takes ids matching
// ^[A-Za-z0-9_-]+$ (422 otherwise), and these are the only folder ids with
// dots: every other one is a hexadecimal CouchDB id, used as is. A user can
// pick any of these directories as a knowledge base ("Shared with me" and
// "Shared drives" are folders of their Drive), so all of them are mapped,
// not just the root.
var wellKnownWorkspaceIDs = map[string]string{
	consts.RootDirID:           rootWorkspaceID,
	consts.TrashDirID:          "io-cozy-files-trash-dir",
	consts.SharedWithMeDirID:   "io-cozy-files-shared-with-me-dir",
	consts.NoLongerSharedDirID: "io-cozy-files-no-longer-shared-dir",
	consts.SharedDrivesDirID:   "io-cozy-files-shared-drives-dir",
}

// wellKnownDirIDs is wellKnownWorkspaceIDs read the other way round.
var wellKnownDirIDs = invertWorkspaceIDs(wellKnownWorkspaceIDs)

func invertWorkspaceIDs(table map[string]string) map[string]string {
	inverse := make(map[string]string, len(table))
	for dirID, workspaceID := range table {
		inverse[workspaceID] = dirID
	}
	return inverse
}

// workspaceIDForDir maps a knowledge base folder id to the id of its openRAG
// workspace.
func workspaceIDForDir(dirID string) string {
	if id, ok := wellKnownWorkspaceIDs[dirID]; ok {
		return id
	}
	return dirID
}

// workspaceIDsForDirs maps folder ids to workspace ids, keeping the order.
func workspaceIDsForDirs(dirIDs []string) []string {
	ids := make([]string, len(dirIDs))
	for i, dirID := range dirIDs {
		ids[i] = workspaceIDForDir(dirID)
	}
	return ids
}

// dirIDForWorkspace is the inverse of workspaceIDForDir: the folder id an
// openRAG workspace id stands for.
func dirIDForWorkspace(workspaceID string) string {
	if dirID, ok := wellKnownDirIDs[workspaceID]; ok {
		return dirID
	}
	return workspaceID
}

// dirIDsForWorkspaces maps workspace ids to folder ids, keeping the order.
func dirIDsForWorkspaces(workspaceIDs []string) []string {
	ids := make([]string, len(workspaceIDs))
	for i, id := range workspaceIDs {
		ids[i] = dirIDForWorkspace(id)
	}
	return ids
}

// workspaceExists tells whether the workspace exists on the openRAG server.
func workspaceExists(server config.RAGServer, domain, id string) (bool, error) {
	res, err := callRAG(server, http.MethodGet, nil, fmt.Sprintf("/partition/%s/workspaces/%s", domain, url.PathEscape(id)), echo.MIMEApplicationJSON)
	if err != nil {
		return false, retryable(err)
	}
	res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err := statusError("GET workspace", res.StatusCode); err != nil {
		return false, err
	}
	return true, nil
}

// ensureWorkspaceExists creates the workspace (and the partition if needed)
// when it is missing. No file is attached here: the indexing attaches them.
func ensureWorkspaceExists(server config.RAGServer, domain, id, displayName string, logger logger.Logger) error {
	exists, err := workspaceExists(server, domain, id)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	createRAGPartition(server, domain, logger)
	body, err := json.Marshal(map[string]interface{}{
		"workspace_id": id,
		"display_name": displayName,
	})
	if err != nil {
		return err
	}
	res, err := callRAG(server, http.MethodPost, body, fmt.Sprintf("/partition/%s/workspaces", domain), echo.MIMEApplicationJSON)
	if err != nil {
		return retryable(err)
	}
	res.Body.Close()
	return statusError("POST workspace", res.StatusCode, http.StatusConflict)
}

// deleteWorkspace removes the workspace from openRAG. A 404 is fine.
func deleteWorkspace(server config.RAGServer, domain, id string) error {
	res, err := callRAG(server, http.MethodDelete, nil, fmt.Sprintf("/partition/%s/workspaces/%s", domain, url.PathEscape(id)), echo.MIMEApplicationJSON)
	if err != nil {
		return retryable(err)
	}
	res.Body.Close()
	return statusError("DELETE workspace", res.StatusCode, http.StatusNotFound)
}

// checkWorkspace is the chat-time check: the workspace must already exist,
// created by the rag-index job when it reconciled the folder. It takes a
// workspace id (see workspaceIDForDir), not a folder id.
func checkWorkspace(inst *instance.Instance, workspaceID string) error {
	exists, err := workspaceExists(inst.RAGServer(), inst.Domain, workspaceID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrWorkspaceMissing
	}
	return nil
}
