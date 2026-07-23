package rag

import (
	"encoding/json"
	"fmt"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	modelrag "github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "rag-index-status",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Timeout:      30 * time.Second,
		WorkerFunc:   WorkerIndexStatus,
	})
}

type statusMessage struct {
	Partition string `json:"partition"`
	FileID    string `json:"file_id"`
	Status    string `json:"status"`    // "success" | "error" | "notsupported"
	Timestamp string `json:"timestamp"` // RFC3339Nano
	// Metadata is the file metadata carried by the callback. Its "version" field
	// holds the content md5sum, used to drop callbacks about outdated content.
	Metadata struct {
		Version string `json:"version"`
	} `json:"metadata"`
}

func WorkerIndexStatus(ctx *job.TaskContext) error {
	inst := ctx.Instance
	log := inst.Logger().WithNamespace("rag")

	raw, err := ctx.UnmarshalPayload()
	if err != nil {
		return err
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var msg statusMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	if msg.FileID == "" {
		return fmt.Errorf("rag-index-status: missing file_id in payload")
	}

	switch msg.Status {
	case modelrag.RAGStatusSuccess, modelrag.RAGStatusError, modelrag.RAGStatusNotSupported:
	default:
		return fmt.Errorf("rag-index-status: unknown status %q for file %s", msg.Status, msg.FileID)
	}

	var ts time.Time
	if msg.Timestamp != "" {
		ts, err = time.Parse(time.RFC3339Nano, msg.Timestamp)
		if err != nil {
			log.Warnf("rag-index-status: invalid timestamp %q for file %s, using now", msg.Timestamp, msg.FileID)
			ts = time.Now()
		}
	} else {
		log.Warnf("rag-index-status: missing timestamp for file %s, using now", msg.FileID)
		ts = time.Now()
	}

	log.Debugf("rag-index-status: file %s status=%s ts=%s", msg.FileID, msg.Status, ts)

	if err := modelrag.SetRAGStatus(inst, msg.FileID, msg.Status, msg.Metadata.Version, ts); err != nil {
		if couchdb.IsNotFoundError(err) {
			log.Debugf("rag-index-status: file %s not found (possibly deleted), skipping", msg.FileID)
			return nil
		}
		return err
	}
	return nil
}
