package rag

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// workspaceBackfillChunkSize is the maximum number of file ids sent in a
// single workspace membership backfill request.
const workspaceBackfillChunkSize = 500

// kbContext carries, for one rag-index batch, the knowledge-base folders
// declared by assistants. Only folders that already exist as workspaces in
// openRAG are kept (uploads may only reference existing workspaces). It is
// only ever used from the single goroutine of one rag-index batch and is NOT
// safe for concurrent use.
type kbContext struct {
	folders  map[string]string // dirID -> folder path
	dirPaths map[string]string // dir_id -> path cache for the batch
}

func (kb *kbContext) empty() bool { return kb == nil || len(kb.folders) == 0 }

func (kb *kbContext) desiredWorkspaces(parentPath string) []string {
	// Trashed content never belongs to a workspace, even when the KB folder
	// itself was moved to the trash.
	if strings.HasPrefix(parentPath, vfs.TrashDirName) {
		return nil
	}
	var ids []string
	for dirID, folderPath := range kb.folders {
		if parentPath == folderPath || strings.HasPrefix(parentPath, folderPath+"/") {
			ids = append(ids, dirID)
		}
	}
	slices.Sort(ids)
	return ids
}

// desiredFor resolves the workspaces that should contain a file whose parent
// directory is dirID. The boolean is false when the parent path could not be
// resolved: callers must treat that as "unknown", never as "no membership"
// (which would incorrectly detach the file from every workspace).
func (kb *kbContext) desiredFor(inst *instance.Instance, dirID string) ([]string, bool) {
	parentPath, ok := kb.parentPath(inst, dirID)
	if !ok {
		return nil, false
	}
	return kb.desiredWorkspaces(parentPath), true
}

// loadKBContext queries the assistants and the openRAG workspace list once
// per batch. Any error yields a nil context: indexing proceeds without
// membership reconciliation (best-effort by design). Note: once the KB
// folders are known, a failure to list the existing openRAG workspaces also
// yields nil, so that reconcileMembership does not mistake "we don't know"
// for "none exist" and delete every current membership.
func loadKBContext(inst *instance.Instance, logger logger.Logger) *kbContext {
	kb := &kbContext{
		folders:  map[string]string{},
		dirPaths: map[string]string{},
	}
	err := couchdb.ForeachDocs(inst, consts.ChatAssistants, func(_ string, doc json.RawMessage) error {
		var assistant chatAssistant
		if err := json.Unmarshal(doc, &assistant); err != nil {
			return err
		}
		dirID := assistant.knowledgeBaseDirID(logger)
		if dirID == "" {
			return nil
		}
		dir, err := inst.VFS().DirByID(dirID)
		if err != nil {
			return nil
		}
		kb.folders[dirID] = dir.Fullpath
		// Files directly inside a KB folder are the common layout: seed the
		// per-batch path cache so parentPath does not re-fetch this dir.
		kb.dirPaths[dirID] = dir.Fullpath
		return nil
	})
	if err != nil {
		if !couchdb.IsNoDatabaseError(err) {
			logger.Warnf("cannot load assistants for workspace sync: %s", err)
		}
		return nil
	}
	if len(kb.folders) == 0 {
		return nil
	}
	res, err := CallRAGQuery(inst, http.MethodGet, nil, fmt.Sprintf("/partition/%s/workspaces", inst.Domain), echo.MIMEApplicationJSON)
	if err != nil {
		logger.Warnf("cannot list RAG workspaces: %s", err)
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		logger.Warnf("cannot list RAG workspaces: status code %d", res.StatusCode)
		return nil
	}
	var body struct {
		Workspaces []struct {
			WorkspaceID string `json:"workspace_id"`
		} `json:"workspaces"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		logger.Warnf("cannot decode RAG workspaces: %s", err)
		return nil
	}
	existingIDs := make([]string, 0, len(body.Workspaces))
	for _, ws := range body.Workspaces {
		existingIDs = append(existingIDs, ws.WorkspaceID)
	}
	pruneMissingWorkspaces(kb.folders, existingIDs)
	if len(kb.folders) == 0 {
		return nil
	}
	return kb
}

// pruneMissingWorkspaces removes from folders the knowledge-base folders
// whose workspace does not exist (yet) in openRAG: uploads and reconciliation
// may only reference existing workspaces (the workspace is created, lazily,
// by ensureWorkspace on the first chat query).
func pruneMissingWorkspaces(folders map[string]string, existingIDs []string) {
	existing := make(map[string]bool, len(existingIDs))
	for _, id := range existingIDs {
		existing[id] = true
	}
	for dirID := range folders {
		if !existing[dirID] {
			delete(folders, dirID)
		}
	}
}

// parentPath resolves and caches the full path of a directory. The boolean
// return value is false when the path could not be resolved (e.g. a
// transient VFS/CouchDB error): callers must treat that as "unknown", never
// as "no ancestors" (which would incorrectly detach the file from every
// knowledge-base workspace).
func (kb *kbContext) parentPath(inst *instance.Instance, dirID string) (string, bool) {
	if p, ok := kb.dirPaths[dirID]; ok {
		return p, true
	}
	dir, err := inst.VFS().DirByID(dirID)
	if err != nil {
		return "", false
	}
	kb.dirPaths[dirID] = dir.Fullpath
	return dir.Fullpath, true
}

// reconcileDirChildren aligns the workspace membership of the direct file
// children of a directory whose doc changed (typically a move or a rename).
// Direct children are enough: a moved or renamed subtree puts every
// descendant directory in the changes feed, so each affected file is seen as
// the direct child of one of the changed directories. Best-effort: errors
// are logged.
func reconcileDirChildren(inst *instance.Instance, logger logger.Logger, kb *kbContext, dirID, dirPath string) {
	if kb.empty() {
		return
	}
	// The changes feed carries the directory's fresh path: seed the cache so
	// the per-file reconciliation does not fetch it again.
	kb.dirPaths[dirID] = dirPath
	iter := inst.VFS().DirIterator(&vfs.DirDoc{DocID: dirID, Fullpath: dirPath}, nil)
	for {
		_, file, err := iter.Next()
		if errors.Is(err, vfs.ErrIteratorDone) {
			return
		}
		if err != nil {
			logger.Warnf("cannot list children of dir %s: %s", dirID, err)
			return
		}
		if file == nil || file.Trashed {
			continue
		}
		reconcileMembership(inst, logger, kb, file.DocID, dirID)
	}
}

// reconcileMembership aligns one file's workspace membership with the
// knowledge-base folders containing it. Best-effort: errors are logged.
func reconcileMembership(inst *instance.Instance, logger logger.Logger, kb *kbContext, fileID, dirID string) {
	if kb.empty() {
		return
	}
	desired, ok := kb.desiredFor(inst, dirID)
	if !ok {
		logger.Warnf("cannot resolve parent path for file %s (dir %s): skipping workspace reconciliation", fileID, dirID)
		return
	}
	reconcileMembershipHTTP(inst.RAGServer(), inst.Domain, fileID, desired, kb.folders, logger)
}

// reconcileMembershipHTTP is the instance-free part of reconcileMembership,
// split out so the diff/add/remove behavior can be unit-tested against an
// httptest server.
func reconcileMembershipHTTP(server config.RAGServer, domain, fileID string, desired []string, known map[string]string, logger logger.Logger) {
	res, err := callRAG(server, http.MethodGet, nil, fmt.Sprintf("/partition/%s/files/%s/workspaces", domain, url.PathEscape(fileID)), echo.MIMEApplicationJSON)
	if err != nil {
		logger.Warnf("workspace membership check failed for %s: %s", fileID, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		logger.Warnf("workspace membership check failed for %s: status code %d", fileID, res.StatusCode)
		return
	}
	var body struct {
		WorkspaceIDs []string `json:"workspace_ids"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		logger.Warnf("workspace membership check failed for %s: %s", fileID, err)
		return
	}
	// openRAG may host workspaces the stack does not manage: they must not
	// lose this file just because they are absent from `desired`.
	var actual []string
	for _, id := range body.WorkspaceIDs {
		if _, ok := known[id]; ok {
			actual = append(actual, id)
		}
	}
	toAdd, toRemove := diffMembership(desired, actual)
	for _, ws := range toAdd {
		status, err := postWorkspaceFiles(server, domain, ws, []string{fileID})
		if err != nil {
			logger.Warnf("workspace add failed for %s in %s: %s", fileID, ws, err)
		} else if status >= 300 {
			logger.Warnf("workspace add failed for %s in %s: status code %d", fileID, ws, status)
		}
	}
	for _, ws := range toRemove {
		r, err := callRAG(server, http.MethodDelete, nil, fmt.Sprintf("/partition/%s/workspaces/%s/files/%s", domain, url.PathEscape(ws), url.PathEscape(fileID)), echo.MIMEApplicationJSON)
		if err != nil {
			logger.Warnf("workspace remove failed for %s in %s: %s", fileID, ws, err)
			continue
		}
		r.Body.Close()
		// 404: the file is already absent from the workspace, fine.
		if r.StatusCode >= 300 && r.StatusCode != http.StatusNotFound {
			logger.Warnf("workspace remove failed for %s in %s: status code %d", fileID, ws, r.StatusCode)
		}
	}
}

// ensureWorkspace makes sure the openRAG workspace mirroring the knowledge
// base folder exists, creating it and backfilling its membership from the
// folder subtree when needed. An error means the query MUST NOT proceed
// unscoped.
func ensureWorkspace(inst *instance.Instance, logger logger.Logger, dirID string) error {
	// Steady state: the workspace exists for every message but the first
	// one. Check it outside any lock so the common path stays lock-free.
	exists, err := workspaceExists(inst.RAGServer(), inst.Domain, dirID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	// Creation path: serialize the concurrent queries racing on the same
	// folder (the rag-query worker runs several jobs in parallel): without
	// this, two first-time chats could both create and backfill the
	// workspace, and the rollback of a failed backfill could delete the
	// workspace the other query just successfully ensured. The backfill of
	// a large folder can outlive a plain lock's TTL, hence LongOperation,
	// which keeps refreshing it. ensureWorkspaceHTTP re-checks existence
	// under the lock, so the loser of the race is a no-op.
	mu := config.Lock().LongOperation(inst, "rag-workspace/"+dirID)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()
	resolve := func() (string, []string, error) {
		dir, err := inst.VFS().DirByID(dirID)
		if err != nil {
			return "", nil, fmt.Errorf("knowledge base folder %s: %w", dirID, err)
		}
		fileIDs, err := listFolderFileIDs(inst, dirID)
		if err != nil {
			return "", nil, err
		}
		return dir.DocName, fileIDs, nil
	}
	return ensureWorkspaceHTTP(inst.RAGServer(), inst.Domain, dirID, resolve, logger)
}

// ensureWorkspaceHTTP is the instance-free part of ensureWorkspace, split
// out so the create/backfill/rollback behavior can be unit-tested against an
// httptest server. `resolve` is a callback rather than precomputed values
// because it must stay lazy: the workspace already exists for every message
// but the first one, and resolving eagerly would make each of them pay a VFS
// lookup and a full folder-subtree walk for nothing.
func ensureWorkspaceHTTP(server config.RAGServer, domain, dirID string, resolve func() (displayName string, fileIDs []string, err error), logger logger.Logger) error {
	exists, err := workspaceExists(server, domain, dirID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	// The partition may not exist yet on a fresh instance (same pattern as
	// the completion 404 path in Query).
	createRAGPartition(server, domain, logger)

	displayName, fileIDs, err := resolve()
	if err != nil {
		return err
	}

	createBody, err := json.Marshal(map[string]interface{}{
		"workspace_id": dirID,
		"display_name": displayName,
	})
	if err != nil {
		return err
	}
	createRes, err := callRAG(server, http.MethodPost, createBody, fmt.Sprintf("/partition/%s/workspaces", domain), echo.MIMEApplicationJSON)
	if err != nil {
		return err
	}
	createRes.Body.Close()
	if !createdOrExists(createRes.StatusCode) {
		return fmt.Errorf("workspace creation status code: %d", createRes.StatusCode)
	}

	for chunk := range slices.Chunk(fileIDs, workspaceBackfillChunkSize) {
		if err := backfillChunk(server, domain, dirID, chunk, logger); err != nil {
			// The workspace was just created but is incomplete, and a later
			// ensureWorkspace call would see it exists (GET 200) and never
			// retry the backfill: delete it so the next call starts over.
			deleteWorkspace(server, domain, dirID, logger)
			return err
		}
	}
	return nil
}

// deleteWorkspace best-effort deletes a workspace whose membership backfill
// failed. If the deletion itself fails, the workspace stays incomplete until
// an operator or a future ensure path cleans it up: log loudly.
func deleteWorkspace(server config.RAGServer, domain, dirID string, logger logger.Logger) {
	res, err := callRAG(server, http.MethodDelete, nil, fmt.Sprintf("/partition/%s/workspaces/%s", domain, url.PathEscape(dirID)), echo.MIMEApplicationJSON)
	if err != nil {
		logger.Errorf("cannot rollback incomplete RAG workspace %s: %s", dirID, err)
		return
	}
	res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotFound {
		logger.Errorf("cannot rollback incomplete RAG workspace %s: status code %d", dirID, res.StatusCode)
	}
}

// workspaceExists tells whether the workspace exists on the openRAG server.
func workspaceExists(server config.RAGServer, domain, dirID string) (bool, error) {
	res, err := callRAG(server, http.MethodGet, nil, fmt.Sprintf("/partition/%s/workspaces/%s", domain, url.PathEscape(dirID)), echo.MIMEApplicationJSON)
	if err != nil {
		return false, err
	}
	res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("workspace check status code: %d", res.StatusCode)
	}
}

// postWorkspaceFiles posts a batch of file ids to a workspace's /files
// endpoint and returns the response status code.
func postWorkspaceFiles(server config.RAGServer, domain, workspaceID string, ids []string) (int, error) {
	body, err := json.Marshal(map[string]interface{}{"file_ids": ids})
	if err != nil {
		return 0, err
	}
	path := fmt.Sprintf("/partition/%s/workspaces/%s/files", domain, url.PathEscape(workspaceID))
	res, err := callRAG(server, http.MethodPost, body, path, echo.MIMEApplicationJSON)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return res.StatusCode, nil
}

// backfillChunk posts one backfill chunk of file ids to the workspace.
// openRAG's endpoint is all-or-nothing: ANY unknown file id in the chunk
// yields a 404 and adds NONE of the chunk's ids, even the ones that are
// indexed. When that happens, the chunk's ids are retried one by one so the
// indexable files are still attached; a per-id 404 is expected for
// non-indexed files (e.g. an image skipped by the indexer's class flags) and
// only logged. Any other non-2xx status (chunked or per-id) is systemic and
// is an ensure failure, so the query does not proceed with a silently
// incomplete workspace.
func backfillChunk(server config.RAGServer, domain, dirID string, ids []string, logger logger.Logger) error {
	status, err := postWorkspaceFiles(server, domain, dirID, ids)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		// Only a 404 can be a benign per-file condition. Anything else
		// non-2xx (401/403 auth or permission problems, 5xx failures…) is
		// systemic: fail the ensure rather than leave a silently incomplete
		// workspace behind.
		if status >= 300 {
			return fmt.Errorf("workspace backfill status code: %d", status)
		}
		return nil
	}

	// A 404 can mean "some file id in the chunk is not indexed" (benign) or
	// "the workspace itself is gone" (the ensure MUST fail so the query does
	// not proceed scoped to a nonexistent workspace): disambiguate before
	// falling back to per-id retries.
	exists, err := workspaceExists(server, domain, dirID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("workspace %s disappeared during backfill", dirID)
	}

	// At least one id in the chunk is unknown to openRAG: retry one by one.
	for _, id := range ids {
		idStatus, err := postWorkspaceFiles(server, domain, dirID, []string{id})
		if err != nil {
			return err
		}
		switch {
		case idStatus == http.StatusNotFound:
			logger.Debugf("workspace backfill skipped file %s (not indexed)", id)
		case idStatus >= 300:
			return fmt.Errorf("workspace backfill status code: %d", idStatus)
		}
	}
	return nil
}

// listFolderFileIDs walks the folder subtree and returns the ids of the
// (non-trashed) files it contains.
func listFolderFileIDs(inst *instance.Instance, dirID string) ([]string, error) {
	var ids []string
	fs := inst.VFS()
	err := vfs.WalkByID(fs, dirID, func(_ string, _ *vfs.DirDoc, file *vfs.FileDoc, err error) error {
		if err != nil {
			return err
		}
		if file != nil && !file.Trashed {
			ids = append(ids, file.DocID)
		}
		return nil
	})
	return ids, err
}

// diffMembership compares the desired and actual workspace memberships of a
// file and returns what must be added and removed.
func diffMembership(desired, actual []string) (toAdd, toRemove []string) {
	actualSet := make(map[string]bool, len(actual))
	for _, id := range actual {
		actualSet[id] = true
	}
	desiredSet := make(map[string]bool, len(desired))
	for _, id := range desired {
		desiredSet[id] = true
		if !actualSet[id] {
			toAdd = append(toAdd, id)
		}
	}
	for _, id := range actual {
		if !desiredSet[id] {
			toRemove = append(toRemove, id)
		}
	}
	return toAdd, toRemove
}
