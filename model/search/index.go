package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

const (
	// WorkerType is the type of the jobs that index the files.
	WorkerType = "search-index"
	// indexLockName serializes the search-index jobs of an instance.
	indexLockName = "search-index/" + consts.Files
	// checkpointDocID is the CouchDB local document holding the checkpoint.
	checkpointDocID = "search-index"
)

// refusedDocErrors are the types of the bulk errors about a document itself,
// that a retry will not fix. The others, like a closed or missing index, fail
// the job and keep the checkpoint.
var refusedDocErrors = []string{"mapper_parsing_exception", "strict_dynamic_mapping_exception"}

// batchSize is the maximal number of changes indexed by a job (a variable for
// the tests).
var batchSize = 500

// readyClient is the client for which this process has created the indices.
// The stack tries at startup, but OpenSearch may be down then, and a bulk
// request on a missing index would create it without our mappings.
var readyClient atomic.Pointer[opensearch.Client]

// pushJob pushes another search-index job (a variable for the tests).
var pushJob = func(inst *instance.Instance) error {
	_, err := job.System().PushJob(inst, &job.JobRequest{WorkerType: WorkerType})
	return err
}

// Index indexes the changes of the files of an instance since the last
// checkpoint, at most batchSize of them, and pushes another job when changes
// remain. Without a checkpoint, it indexes all the files: that is the
// backfill.
func Index(ctx context.Context, inst *instance.Instance, log logger.Logger) error {
	client := config.GetConfig().Search
	if client == nil {
		log.Debugf("search is not configured: nothing to index")
		return nil
	}

	mu := config.Lock().LongOperation(inst, indexLockName)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	if readyClient.Load() != client {
		if err := EnsureIndices(ctx, client); err != nil {
			return err
		}
		readyClient.Store(client)
	}

	cp, err := couchdb.LoadCheckpoint(inst, consts.Files, checkpointDocID)
	if err != nil {
		return err
	}
	feed, err := couchdb.GetChanges(inst, &couchdb.ChangesRequest{
		DocType:     consts.Files,
		IncludeDocs: true,
		Since:       cp.LastSeq,
		Limit:       batchSize,
	})
	if err != nil {
		return err
	}
	scp, err := couchdb.LoadCheckpoint(inst, consts.Sharings, sharingsCheckpointDocID)
	if err != nil {
		return err
	}
	sfeed, err := couchdb.GetChanges(inst, &couchdb.ChangesRequest{
		DocType:     consts.Sharings,
		IncludeDocs: true,
		Since:       scp.LastSeq,
		Limit:       batchSize,
	})
	if couchdb.IsNoDatabaseError(err) {
		sfeed, err = &couchdb.ChangesResponse{LastSeq: scp.LastSeq}, nil
	}
	if err != nil {
		return err
	}
	if feed.LastSeq == cp.LastSeq && sfeed.LastSeq == scp.LastSeq {
		return nil
	}

	parents, err := loadParents(inst, feed.Results)
	if err != nil {
		return err
	}
	if ndjson := bulkActions(inst.Domain, feed.Results, parents); len(ndjson) > 0 {
		err := client.Bulk(ctx, FilesIndex, ndjson)
		var osErr *opensearch.Error
		if errors.As(err, &osErr) && osErr.DocID != "" && slices.Contains(refusedDocErrors, osErr.Type) {
			// Retrying will not fix a document that OpenSearch refuses: the
			// others are indexed, and the checkpoint moves on.
			log.Warnf("a file of the batch is not indexed: %s", err)
		} else if err != nil {
			if osErr != nil && osErr.Type == "index_not_found_exception" {
				// The retry creates the index again, with our mappings.
				readyClient.Store(nil)
			}
			return err
		}
	}

	// The checkpoints move only when the scopes are up to date too, so that
	// a folder change is never lost for them.
	if err := syncScopes(ctx, client, inst, feed.Results, sfeed.Results); err != nil {
		return err
	}

	cp = couchdb.Checkpoint{LastSeq: feed.LastSeq}
	if err := couchdb.SaveCheckpoint(inst, consts.Files, checkpointDocID, cp); err != nil {
		return err
	}
	scp = couchdb.Checkpoint{LastSeq: sfeed.LastSeq}
	if err := couchdb.SaveCheckpoint(inst, consts.Sharings, sharingsCheckpointDocID, scp); err != nil {
		return err
	}
	log.Debugf("%d file and %d sharing change(s) indexed, %d and %d pending",
		len(feed.Results), len(sfeed.Results), feed.Pending, sfeed.Pending)
	if feed.Pending > 0 || sfeed.Pending > 0 {
		return pushJob(inst)
	}
	return nil
}

// bulkActions turns the changes into the NDJSON body of a _bulk request: the
// changed docs are indexed, the deleted ones and those in a skipped tree are
// removed. parents are the parent folders of the changed files.
//
// shortcut: a folder moved into or out of a skipped tree does not reindex the
// files already in it, reindex the moved subtree if that matters.
func bulkActions(domain string, changes []couchdb.Change, parents map[string]*vfs.DirDoc) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, change := range changes {
		if change.Deleted || isInSkippedTree(change.Doc, parents) {
			if !strings.HasPrefix(change.DocID, "_design/") {
				_ = enc.Encode(map[string]any{"delete": map[string]string{"_id": Key(domain, change.DocID)}})
			}
			continue
		}
		doc, ok := NewFileDocument(domain, change.Doc)
		if !ok {
			continue
		}
		_ = enc.Encode(map[string]any{"index": map[string]string{"_id": doc.ResourceKey}})
		_ = enc.Encode(doc)
	}
	return buf.Bytes()
}

// isInSkippedTree returns true if the file or folder is under a skipped
// folder. A skipped folder itself is indexed.
func isInSkippedTree(doc couchdb.JSONDoc, parents map[string]*vfs.DirDoc) bool {
	if doc.M["type"] == consts.DirType {
		dirPath, _ := doc.M["path"].(string)
		return dirPath != "" && inSkippedTree(path.Dir(dirPath))
	}
	dirID, _ := doc.M["dir_id"].(string)
	parent := parents[dirID]
	return parent != nil && inSkippedTree(parent.Fullpath)
}

// loadParents returns the parent folders of the changed files, as the file
// docs have no path.
func loadParents(inst *instance.Instance, changes []couchdb.Change) (map[string]*vfs.DirDoc, error) {
	var ids []string
	seen := make(map[string]bool)
	for _, change := range changes {
		dirID, _ := change.Doc.M["dir_id"].(string)
		if change.Deleted || change.Doc.M["type"] == consts.DirType || dirID == "" || seen[dirID] {
			continue
		}
		seen[dirID] = true
		ids = append(ids, dirID)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return loadDirs(inst, ids)
}
