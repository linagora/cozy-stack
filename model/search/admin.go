package search

import (
	"context"
	"errors"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/search/purge"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/opensearch"
)

// ErrNotConfigured is returned when search is not configured on the stack.
var ErrNotConfigured = errors.New("search is not configured")

// Status is the indexing status of an instance.
type Status struct {
	Trigger bool `json:"trigger"`
	// LastSeq is the last change indexed, empty until the backfill has
	// indexed its first batch.
	LastSeq string `json:"last_seq"`
}

// Enable turns search on for an instance: it creates the indices if needed,
// the trigger that follows the changes, and pushes the first job (the
// backfill when the instance has never been indexed).
func Enable(ctx context.Context, inst *instance.Instance) error {
	client := opensearch.New(config.GetConfig().Search)
	if client == nil {
		return ErrNotConfigured
	}
	if err := EnsureIndices(ctx, client); err != nil {
		return err
	}
	if err := job.EnsureSearchTrigger(inst); err != nil {
		return err
	}
	return pushJob(inst)
}

// Reset drops the checkpoints and the documents of an instance (files and
// scopes of its drives), and pushes a job to index it again.
func Reset(ctx context.Context, inst *instance.Instance) error {
	if config.GetConfig().Search.URL == nil {
		return ErrNotConfigured
	}
	mu := config.Lock().LongOperation(inst, indexLockName)
	if err := mu.Lock(); err != nil {
		return err
	}
	err := couchdb.DeleteCheckpoint(inst, consts.Files, checkpointDocID)
	if err == nil {
		// The scope docs are rebuilt from the sharings feed
		err = couchdb.DeleteCheckpoint(inst, consts.Sharings, sharingsCheckpointDocID)
	}
	if err == nil {
		err = purge.CleanInstance(inst.Domain)
	}
	mu.Unlock()
	if err != nil {
		return err
	}
	return pushJob(inst)
}

// GetStatus returns the indexing status of an instance.
func GetStatus(inst *instance.Instance) (*Status, error) {
	if config.GetConfig().Search.URL == nil {
		return nil, ErrNotConfigured
	}
	cp, err := couchdb.LoadCheckpoint(inst, consts.Files, checkpointDocID)
	if err != nil {
		return nil, err
	}
	return &Status{Trigger: job.HasSearchTrigger(inst), LastSeq: cp.LastSeq}, nil
}
