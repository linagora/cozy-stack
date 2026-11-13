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
// recreated under a new ID on the shared drive of another owner of the same
// stack. It runs on the instance of the source owner.
//
// For each item, a source-side user (the source owner, or a member of the
// source drive) who can still open the item at its destination keeps their row,
// under the new ID, with the destination drive's drive_id for a member and none
// for the destination owner, and the dates merged to the max. Anyone who cannot
// open the destination loses their row.
//
// The originals are destroyed by the move, and nothing holds the propagation of
// their deletion: when it runs before this job, the rows are deleted and there
// is nothing left to move.
func WorkerMove(ctx *job.TaskContext) error {
	var msg job.RecentsMoveMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	dest := sharing.LocalInstanceFromHost(msg.DestOwnerDomain)
	if dest == nil {
		// The destination is not on this stack: nothing can be preserved, and
		// the rows are left to the propagation of the deletion.
		return nil
	}
	recipients, err := findRecipients(dest)
	if err != nil {
		return err
	}
	m := &mover{
		dest:       newSyncer(dest, ctx.Logger()),
		recipients: make(map[string]recipient, len(recipients)),
		newIDs:     make(map[string]string, len(msg.Pairs)),
	}
	for _, r := range recipients {
		m.recipients[r.inst.Domain] = r
	}
	oldIDs := make([]string, 0, len(msg.Pairs))
	for _, pair := range msg.Pairs {
		m.newIDs[pair.OldID] = pair.NewID
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
		for _, row := range rows {
			if err := m.moveRow(inst, row); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// mover carries the state of a recents move: the items of the destination
// owner, the users who can hold a row on them by the domain of their instance,
// and the new ID of each moved item by its old ID.
type mover struct {
	dest       *syncer
	recipients map[string]recipient
	newIDs     map[string]string
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
	for _, r := range localMembers(drive, map[string]struct{}{src.Domain: {}}) {
		insts = append(insts, r.inst)
	}
	return insts, nil
}

// moveRow moves a row to the new ID of its item when its user can still open
// the item at the destination, and deletes it otherwise: the user is neither
// the destination owner nor a member of a drive that gives the item, or the
// item is already gone.
func (m *mover) moveRow(inst *instance.Instance, row *recents.FileDirActivity) error {
	newID := m.newIDs[row.DocID]
	it, err := m.dest.item(newID)
	if err != nil {
		return err
	}
	r, known := m.recipients[inst.Domain]
	if !known || it.destroyed() {
		return recents.DeleteRow(inst, row)
	}
	meta, ok, err := m.dest.metadataFor(r, newID, it)
	if err != nil {
		return err
	}
	if !ok {
		return recents.DeleteRow(inst, row)
	}
	return recents.MoveRow(inst, row, newID, meta)
}
