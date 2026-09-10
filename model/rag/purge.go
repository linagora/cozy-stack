package rag

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// Purge deletes everything openRAG holds for the instance: its workspaces,
// its files and the partition itself. The index statuses are dropped, and
// so is the rag-index checkpoint, so that the next run indexes the whole
// instance again from the beginning of the changes feed.
func Purge(inst *instance.Instance, logger logger.Logger) error {
	server := inst.RAGServer()
	if server.URL == "" {
		return errors.New("no RAG server configured")
	}
	// Workspaces are deleted one by one rather than left to the partition
	// deletion, so that the result does not depend on openRAG cascading it.
	workspaces, err := listWorkspaces(server, inst.Domain)
	if err != nil {
		return err
	}
	for _, id := range workspaces {
		if err := deleteWorkspace(server, inst.Domain, id); err != nil {
			return err
		}
	}
	logger.Infof("purge: deleting the openRAG partition (%d workspaces dropped)", len(workspaces))
	if err := deletePartition(server, inst.Domain); err != nil {
		return err
	}
	if err := couchdb.DeleteDB(inst, consts.ChatRAG); err != nil && !couchdb.IsNotFoundError(err) {
		return err
	}
	return resetCheckpoint(inst)
}

// deletePartition removes the instance's partition from openRAG, with every
// file in it. A missing partition is fine.
func deletePartition(server config.RAGServer, domain string) error {
	res, err := callRAG(server, http.MethodDelete, nil, fmt.Sprintf("/partition/%s", domain), echo.MIMEApplicationJSON)
	if err != nil {
		return err
	}
	res.Body.Close()
	return statusError("DELETE partition", res.StatusCode, http.StatusNotFound)
}
