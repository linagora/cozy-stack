// Package recents contains the worker that deletes the personal recents rows
// of the files and folders of an instance that have been destroyed.
package recents

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   job.RecentsSyncWorker,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      2 * time.Minute,
		WorkerFunc:   WorkerSync,
	})
}

// WorkerSync deletes the rows of the items of the job that are destroyed for
// good. It runs on the instance of their owner.
//
// The position of a destroyed item cannot be read any more, so the recipients
// do not depend on it: the owner and all the ready members of the drives of the
// owner. The ID of an item identifies it across the instances of the stack, so
// a row with this ID is a row on this item. The rows are read first, and only
// them cost anything else: most recipients have no row on most items.
//
// An item that still exists, even in the trash, keeps its rows as they are: the
// list reads the item at each request.
func WorkerSync(ctx *job.TaskContext) error {
	var msg job.RecentsSyncMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	// The fixed IDs are shared by every instance: a row with such an ID, which
	// should not exist, says nothing about this item.
	ids := slices.DeleteFunc(msg.IDs, recents.IsFixedID)
	if len(ids) == 0 {
		return nil
	}
	recipients, err := findRecipients(ctx.Instance)
	if err != nil {
		return err
	}

	gone := goneItems{owner: ctx.Instance, known: make(map[string]bool)}
	var errs []error
	for _, inst := range recipients {
		rows, err := recents.RowsByIDs(inst, ids)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, row := range rows {
			isGone, err := gone.isGone(row.DocID)
			if err == nil && isGone {
				err = recents.DeleteRow(inst, row)
			}
			if err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// findRecipients returns the instances of the owner and of the ready members of
// their drives that are on this stack, for those who have the recents enabled.
func findRecipients(owner *instance.Instance) ([]*instance.Instance, error) {
	var recipients []*instance.Instance
	if recents.Enabled(owner) {
		recipients = append(recipients, owner)
	}
	drives, err := sharing.ListDrives(owner)
	if err != nil {
		if couchdb.IsNoDatabaseError(err) {
			return recipients, nil
		}
		return nil, err
	}
	seen := map[string]struct{}{owner.Domain: {}}
	for _, drive := range drives {
		// The instance also holds the drives of other owners that it is a member
		// of, with their members, who have nothing to do with its items.
		if drive.Owner {
			recipients = append(recipients, localMembers(drive, seen)...)
		}
	}
	return recipients, nil
}

// localMembers returns the instances of the ready members of a drive that are
// on this stack and have the recents enabled, except those whose host has been
// seen already.
func localMembers(drive *sharing.Sharing, seen map[string]struct{}) []*instance.Instance {
	var insts []*instance.Instance
	for i := range drive.Members {
		member := &drive.Members[i]
		if member.Status != sharing.MemberStatusReady {
			continue
		}
		host := member.InstanceHost()
		if _, ok := seen[host]; ok || host == "" {
			continue
		}
		seen[host] = struct{}{}
		inst := sharing.LocalInstanceFromHost(host)
		if inst != nil && recents.Enabled(inst) {
			insts = append(insts, inst)
		}
	}
	return insts
}

// goneItems tells which items of an owner are destroyed for good. An item is
// read at most once by job, and only when there is a row on it.
type goneItems struct {
	owner *instance.Instance
	known map[string]bool
}

// isGone reports whether the owner has no file or folder with this ID any
// more. A trashed item still exists.
func (g *goneItems) isGone(id string) (bool, error) {
	if gone, ok := g.known[id]; ok {
		return gone, nil
	}
	_, _, err := g.owner.VFS().DirOrFileByID(id)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	g.known[id] = err != nil
	return g.known[id], nil
}
