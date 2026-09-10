package rag

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// fileWorkspaces returns the workspaces openRAG lists for the file. found is
// false when openRAG does not know the file.
func fileWorkspaces(server config.RAGServer, domain, fileID string) ([]string, bool, error) {
	res, err := callRAG(server, http.MethodGet, nil, fmt.Sprintf("/partition/%s/files/%s/workspaces", domain, url.PathEscape(fileID)), echo.MIMEApplicationJSON)
	if err != nil {
		return nil, false, retryable(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if err := statusError("GET file workspaces", res.StatusCode); err != nil {
		return nil, false, err
	}
	var body struct {
		WorkspaceIDs []string `json:"workspace_ids"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, false, err
	}
	return body.WorkspaceIDs, true, nil
}

func addMembership(server config.RAGServer, domain, workspaceID, fileID string) error {
	body, err := json.Marshal(map[string]interface{}{"file_ids": []string{fileID}})
	if err != nil {
		return err
	}
	res, err := callRAG(server, http.MethodPost, body, fmt.Sprintf("/partition/%s/workspaces/%s/files", domain, url.PathEscape(workspaceID)), echo.MIMEApplicationJSON)
	if err != nil {
		return retryable(err)
	}
	res.Body.Close()
	return statusError("POST workspace file", res.StatusCode)
}

func removeMembership(server config.RAGServer, domain, workspaceID, fileID string) error {
	res, err := callRAG(server, http.MethodDelete, nil, fmt.Sprintf("/partition/%s/workspaces/%s/files/%s", domain, url.PathEscape(workspaceID), url.PathEscape(fileID)), echo.MIMEApplicationJSON)
	if err != nil {
		return retryable(err)
	}
	res.Body.Close()
	return statusError("DELETE workspace file", res.StatusCode, http.StatusNotFound)
}

// syncMembership makes the file's memberships equal to desired: missing
// ones are added, extra ones removed. A file openRAG does not know yet
// (asynchronous indexing) is skipped: the upload carried the memberships.
func syncMembership(server config.RAGServer, domain, fileID string, desired []string) error {
	current, found, err := fileWorkspaces(server, domain, fileID)
	if err != nil || !found {
		return err
	}
	for _, id := range desired {
		if !slices.Contains(current, id) {
			if err := addMembership(server, domain, id, fileID); err != nil {
				return err
			}
		}
	}
	for _, id := range current {
		if !slices.Contains(desired, id) {
			if err := removeMembership(server, domain, id, fileID); err != nil {
				return err
			}
		}
	}
	return nil
}

// detachFileHTTP removes the file from the workspace, then deletes it from
// openRAG when no membership remains. It reports whether the file was deleted.
func detachFileHTTP(server config.RAGServer, domain, fileID, workspaceID string, logger logger.Logger) (bool, error) {
	ids, found, err := fileWorkspaces(server, domain, fileID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	remaining := 0
	for _, id := range ids {
		if id == workspaceID {
			if err := removeMembership(server, domain, workspaceID, fileID); err != nil {
				return false, err
			}
			continue
		}
		remaining++
	}
	if remaining > 0 {
		return false, nil
	}
	logger.Debugf("file %s belongs to no knowledge base any more: deleting it from openRAG", fileID)
	if err := deleteFromRAGHTTP(server, domain, fileID); err != nil {
		return false, err
	}
	return true, nil
}

// detachFile is detachFileHTTP on the instance's RAG server, plus the index
// status cleanup when the file was deleted from openRAG.
func detachFile(inst *instance.Instance, fileID, workspaceID string, logger logger.Logger) error {
	deleted, err := detachFileHTTP(inst.RAGServer(), inst.Domain, fileID, workspaceID, logger)
	if err != nil || !deleted {
		return err
	}
	return DeleteIndexStatus(inst, fileID)
}
