package rag

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
)

// checkpoint is the per-trigger progress in the changes feed, stored in a
// CouchDB local document of the indexed doctype's database.
type checkpoint struct {
	// LastSeq is the last sequence of the changes feed processed.
	LastSeq string
	// Retries counts the failed attempts at the batch starting after
	// LastSeq. Reset to 0 when LastSeq advances.
	Retries int
}

func loadCheckpoint(inst *instance.Instance, doctype, id string) (checkpoint, error) {
	result, err := couchdb.GetLocal(inst, doctype, id)
	if couchdb.IsNotFoundError(err) {
		return checkpoint{}, nil
	}
	if err != nil {
		return checkpoint{}, err
	}
	var cp checkpoint
	cp.LastSeq, _ = result["last_seq"].(string)
	if r, ok := result["retries"].(float64); ok {
		cp.Retries = int(r)
	}
	return cp, nil
}

// saveCheckpoint writes the checkpoint, unless the stored sequence is more
// recent than the one to save.
func saveCheckpoint(inst *instance.Instance, doctype, id string, cp checkpoint) error {
	result, err := couchdb.GetLocal(inst, doctype, id)
	if err != nil {
		if !couchdb.IsNotFoundError(err) {
			return err
		}
		result = make(map[string]interface{})
	} else if prev, ok := result["last_seq"].(string); ok {
		if revision.Generation(cp.LastSeq) < revision.Generation(prev) {
			return nil
		}
	}
	result["last_seq"] = cp.LastSeq
	result["retries"] = cp.Retries
	return couchdb.PutLocal(inst, doctype, id, result)
}

func deleteCheckpoint(inst *instance.Instance, doctype, id string) error {
	err := couchdb.DeleteLocal(inst, doctype, id)
	if couchdb.IsNotFoundError(err) {
		return nil
	}
	return err
}
