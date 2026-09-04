package rag

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/labstack/echo/v4"
)

// ErrNoIndexTrigger is returned by Reset when no rag-index trigger matches.
var ErrNoIndexTrigger = errors.New("no rag-index trigger found")

// PurgeResult summarizes a Purge run.
type PurgeResult struct {
	Scanned int `json:"scanned"`
	Deleted int `json:"deleted"`
}

// claims is what the instance's rag-index triggers cover.
type claims struct {
	global bool
	paths  []string // full paths of the scoped folders
}

func (c claims) covers(parentPath string) bool {
	if strings.HasPrefix(parentPath, vfs.TrashDirName) {
		return false
	}
	if c.global {
		return true
	}
	for _, p := range c.paths {
		if parentPath == p || strings.HasPrefix(parentPath, p+"/") {
			return true
		}
	}
	return false
}

// indexTriggers returns the rag-index triggers of the instance with their
// decoded message, skipping remove-action ones.
func indexTriggers(inst *instance.Instance) ([]job.Trigger, []IndexMessage, error) {
	all, err := job.System().GetAllTriggers(inst)
	if err != nil {
		return nil, nil, err
	}
	var triggers []job.Trigger
	var msgs []IndexMessage
	for _, t := range all {
		infos := t.Infos()
		if infos.WorkerType != workerType {
			continue
		}
		var msg IndexMessage
		if len(infos.Message) > 0 {
			if err := json.Unmarshal(infos.Message, &msg); err != nil {
				continue
			}
		}
		if msg.Action != "" {
			continue
		}
		if msg.Doctype == "" {
			msg.Doctype = consts.Files
		}
		triggers = append(triggers, t)
		msgs = append(msgs, msg)
	}
	return triggers, msgs, nil
}

func loadClaims(inst *instance.Instance) (claims, error) {
	var c claims
	_, msgs, err := indexTriggers(inst)
	if err != nil {
		return c, err
	}
	for _, msg := range msgs {
		if msg.DirID == "" {
			c.global = true
			continue
		}
		dir, err := inst.VFS().DirByID(msg.DirID)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return c, err
		}
		c.paths = append(c.paths, dir.Fullpath)
	}
	return c, nil
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

// Purge deletes from openRAG (and drops the index status of) every file
// that no rag-index trigger claims: files gone or trashed in the VFS, and
// files outside every scoped folder when there is no global trigger.
func Purge(inst *instance.Instance, logger logger.Logger) (PurgeResult, error) {
	var result PurgeResult
	server := inst.RAGServer()
	if server.URL == "" {
		return result, errors.New("no RAG server configured")
	}
	cl, err := loadClaims(inst)
	if err != nil {
		return result, err
	}
	ids, err := listPartitionFiles(server, inst.Domain)
	if err != nil {
		return result, err
	}
	fs := inst.VFS()
	dirPaths := map[string]string{}
	for _, id := range ids {
		result.Scanned++
		keep := false
		file, err := fs.FileByID(id)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		if err == nil && !file.Trashed {
			parentPath, ok := dirPaths[file.DirID]
			if !ok {
				dir, dirErr := fs.DirByID(file.DirID)
				switch {
				case dirErr == nil:
					parentPath, ok = dir.Fullpath, true
					dirPaths[file.DirID] = parentPath
				case errors.Is(dirErr, os.ErrNotExist):
					// The parent directory is gone: the file is orphaned,
					// nothing claims it.
				default:
					return result, dirErr
				}
			}
			keep = ok && cl.covers(parentPath)
		}
		if keep {
			continue
		}
		logger.Infof("purge: deleting unclaimed file %s from openRAG", id)
		if err := deleteFromRAG(inst, id); err != nil {
			return result, err
		}
		result.Deleted++
	}
	return result, nil
}

// resetCheckpoint deletes the checkpoint of one trigger under the lock that
// trigger's jobs take, so a batch already running cannot save its own
// LastSeq over the reset.
func resetCheckpoint(inst *instance.Instance, msg IndexMessage) error {
	mu := config.Lock().LongOperation(inst, msg.lockName())
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()
	return deleteCheckpoint(inst, msg.Doctype, msg.checkpointID())
}

// Reset drops the checkpoint of the matching rag-index triggers (all of them
// when dirID is empty) and launches them, forcing a full re-index from the
// beginning of the changes feed. It returns the number of triggers reset.
func Reset(inst *instance.Instance, dirID string) (int, error) {
	triggers, msgs, err := indexTriggers(inst)
	if err != nil {
		return 0, err
	}
	n := 0
	for i, t := range triggers {
		msg := msgs[i]
		if dirID != "" && msg.DirID != dirID {
			continue
		}
		if err := resetCheckpoint(inst, msg); err != nil {
			return n, err
		}
		req := t.Infos().JobRequest()
		req.Manual = true
		if _, err := job.System().PushJob(inst, req); err != nil {
			return n, err
		}
		n++
	}
	if n == 0 {
		return 0, ErrNoIndexTrigger
	}
	return n, nil
}
