package rag

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// ErrWorkspaceMissing is returned by checkWorkspace when the knowledge base
// folder has no workspace on openRAG, i.e. no scoped rag-index trigger has
// run for it yet.
var ErrWorkspaceMissing = errors.New("the knowledge base folder is not indexed: no rag-index trigger for it")

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
// created by the scoped rag-index trigger of the folder.
func checkWorkspace(inst *instance.Instance, dirID string) error {
	exists, err := workspaceExists(inst.RAGServer(), inst.Domain, dirID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrWorkspaceMissing
	}
	return nil
}
