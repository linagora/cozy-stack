package recents

import (
	"errors"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   job.RecentsMoveWorker,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      2 * time.Minute,
		WorkerFunc:   WorkerMove,
	})
}

// WorkerMove moves the personal recents rows of the items that a move has
// recreated under a new ID on the instance of another owner of the same stack.
// It runs on the instance of the source owner.
//
// Every source-side user (the source owner, or a member of the source drive)
// keeps their row, under the new ID, with the domain of the destination owner
// and the dates merged to the max. Whether they can open the item at its
// destination is not checked here: the list tells it at each request, and
// shows what the row holds to a user who cannot.
//
// The originals are destroyed by the move, and nothing holds the propagation of
// their deletion: when it runs before this job, the rows are deleted and there
// is nothing left to move.
func WorkerMove(ctx *job.TaskContext) error {
	var msg job.RecentsMoveMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	newIDs := make(map[string]string, len(msg.Pairs))
	oldIDs := make([]string, 0, len(msg.Pairs))
	for _, pair := range msg.Pairs {
		newIDs[pair.OldID] = pair.NewID
		oldIDs = append(oldIDs, pair.OldID)
	}
	sources, err := sourceInstances(ctx.Instance, msg.SourceDriveID)
	if err != nil {
		return err
	}

	var errs []error
	for _, inst := range sources {
		rows, err := recents.RowsByIDs(inst, oldIDs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		ownerDomain := msg.DestOwnerDomain
		if inst.Domain == ownerDomain {
			// The owner of the destination holds the items.
			ownerDomain = ""
		}
		for _, row := range rows {
			if err := recents.MoveRow(inst, row, newIDs[row.DocID], ownerDomain); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// sourceInstances returns the instances of the users who could hold a row on an
// item of the source: the source owner and the ready members of the source
// drive that are on this stack, for those who have the recents enabled. Without
// a source drive, the source is the personal space of the owner.
func sourceInstances(src *instance.Instance, driveID string) ([]*instance.Instance, error) {
	var insts []*instance.Instance
	if recents.Enabled(src) {
		insts = append(insts, src)
	}
	if driveID == "" {
		return insts, nil
	}
	drive, err := sharing.FindSharing(src, driveID)
	if couchdb.IsNotFoundError(err) {
		return insts, nil
	}
	if err != nil {
		return nil, err
	}
	return append(insts, localMembers(drive, map[string]struct{}{src.Domain: {}})...), nil
}
