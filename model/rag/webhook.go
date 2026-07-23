package rag

import (
	"errors"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// ragStatusTriggerID is the fixed ID of the per-instance @webhook trigger that
// feeds the "rag-index-status" worker, so it can be looked up in a single query.
const ragStatusTriggerID = "rag-index-status"

// EnsureRAGWebhook returns the URL of the per-instance @webhook trigger feeding
// the "rag-index-status" worker, creating it on the first call.
func EnsureRAGWebhook(inst *instance.Instance) (string, error) {
	sched := job.System()

	t, err := sched.GetTrigger(inst, ragStatusTriggerID)
	if err != nil {
		if !errors.Is(err, job.ErrNotFoundTrigger) {
			return "", err
		}
		t, err = job.NewTrigger(inst, job.TriggerInfos{
			TID:        ragStatusTriggerID,
			Type:       "@webhook",
			WorkerType: "rag-index-status",
		}, nil)
		if err != nil {
			return "", err
		}
		if err = sched.AddTrigger(t); err != nil {
			if !couchdb.IsConflictError(err) {
				return "", err
			}
			// A concurrent first call already created the trigger; reuse it.
			if t, err = sched.GetTrigger(inst, ragStatusTriggerID); err != nil {
				return "", err
			}
		} else {
			inst.Logger().WithNamespace("rag").Infof("RAG webhook trigger created: %s", t.ID())
		}
	}
	return inst.PageURL("/jobs/webhooks/"+t.ID(), nil), nil
}
