package recents

import (
	"errors"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   job.RecentsMembershipsWorker,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      2 * time.Minute,
		WorkerFunc:   WorkerMemberships,
	})
}

// WorkerMemberships moves the rows of the members whose drives have changed at
// an owner: for each member of the job, every row of the member on the items of
// the owner takes the drive chosen by DriveForMember, with its path and metadata
// refreshed. It runs on the instance of the owner.
//
// A member is processed as a whole, whatever changed in their drives: the drive
// of each of their rows is recomputed from the current position of its item and
// the current drives of the member. A row whose item is no longer accessible to
// the member, or trashed, is left as it is, and a row whose item is destroyed is
// deleted, exactly like the recents-sync worker, whose row primitives this
// worker reuses.
func WorkerMemberships(ctx *job.TaskContext) error {
	var msg job.RecentsMembershipsMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	owner := ctx.Instance
	s := newSyncer(owner, ctx.Logger())
	var errs []error
	for _, instanceURL := range msg.Members {
		ref := &sharing.Member{Instance: instanceURL}
		host := ref.InstanceHost()
		if host == "" {
			continue
		}
		inst := sharing.LocalInstanceFromHost(host)
		if inst == nil || !recents.Enabled(inst) {
			continue
		}
		rows, err := recents.RowsByOwnerDomain(inst, owner.Domain)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, row := range rows {
			if err := s.syncRow(recipient{inst: inst, member: ref}, row); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
