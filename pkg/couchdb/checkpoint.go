package couchdb

import (
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Checkpoint is the progress of a job in a changes feed, stored in a CouchDB
// local document of the followed doctype's database.
type Checkpoint struct {
	// LastSeq is the last sequence of the changes feed processed.
	LastSeq string
	// Retries counts the failed attempts at the batch starting after
	// LastSeq. Reset to 0 when LastSeq advances.
	Retries int
}

// LoadCheckpoint reads a checkpoint. A missing checkpoint is an empty one.
func LoadCheckpoint(db prefixer.Prefixer, doctype, id string) (Checkpoint, error) {
	result, err := GetLocal(db, doctype, id)
	if IsNotFoundError(err) {
		return Checkpoint{}, nil
	}
	if err != nil {
		return Checkpoint{}, err
	}
	var cp Checkpoint
	cp.LastSeq, _ = result["last_seq"].(string)
	if r, ok := result["retries"].(float64); ok {
		cp.Retries = int(r)
	}
	return cp, nil
}

// SaveCheckpoint writes the checkpoint, unless the stored sequence is more
// recent than the one to save.
func SaveCheckpoint(db prefixer.Prefixer, doctype, id string, cp Checkpoint) error {
	result, err := GetLocal(db, doctype, id)
	if err != nil {
		if !IsNotFoundError(err) {
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
	return PutLocal(db, doctype, id, result)
}

// DeleteCheckpoint removes a checkpoint. A missing checkpoint is not an error.
func DeleteCheckpoint(db prefixer.Prefixer, doctype, id string) error {
	err := DeleteLocal(db, doctype, id)
	if IsNotFoundError(err) {
		return nil
	}
	return err
}
