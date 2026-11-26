package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
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
	if feed.LastSeq == cp.LastSeq {
		return nil
	}

	if ndjson := bulkActions(inst.Domain, feed.Results); len(ndjson) > 0 {
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

	cp = couchdb.Checkpoint{LastSeq: feed.LastSeq}
	if err := couchdb.SaveCheckpoint(inst, consts.Files, checkpointDocID, cp); err != nil {
		return err
	}
	log.Debugf("%d change(s) indexed up to %s, %d pending", len(feed.Results), feed.LastSeq, feed.Pending)
	if feed.Pending > 0 {
		return pushJob(inst)
	}
	return nil
}

// bulkActions turns the changes into the NDJSON body of a _bulk request: the
// changed docs are indexed, the deleted ones are removed.
func bulkActions(domain string, changes []couchdb.Change) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, change := range changes {
		if strings.HasPrefix(change.DocID, "_design/") {
			continue
		}
		if change.Deleted {
			_ = enc.Encode(map[string]any{"delete": map[string]string{"_id": Key(domain, change.DocID)}})
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
