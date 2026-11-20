package orgdrive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/messaging"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
)

// WorkerRemoval revokes and trashes drives after a removed command.
const WorkerRemoval = "org-drive-removal"

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   WorkerRemoval,
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 5,
		Reserved:     true,
		Timeout:      2 * time.Hour,
		WorkerFunc:   workRemoval,
	})
}

// RemovalMessage is the message of the org-drive-removal jobs.
type RemovalMessage struct {
	OrganizationID string    `json:"organization_id"`
	Reference      Reference `json:"reference"`
	Revision       int64     `json:"revision"`
}

// RemovedData is the data of com.twake.drive.org_drive.removed.v1.
type RemovedData struct {
	Reference Reference `json:"reference"`
	Revision  int64     `json:"revision"`
}

// MarkRemoved cuts the access to the drives of the reference at once, since
// the drive routes refuse an inactive sharing, and stores the revision. It
// returns false for a command older than the last one applied.
func MarkRemoved(inst *instance.Instance, cmd *Command) (bool, error) {
	mu := referenceLock(inst, cmd.Reference)
	if err := mu.Lock(); err != nil {
		return false, err
	}
	defer mu.Unlock()

	all, err := sharing.FindByReference(inst, cmd.Reference.Source, cmd.Reference.ID)
	if err != nil {
		return false, err
	}
	for _, s := range all {
		if s.Reference.Revision > cmd.Revision {
			return false, nil
		}
	}
	for _, s := range all {
		if !s.Owner || !s.Drive || !s.Active {
			continue
		}
		s.Active = false
		s.Reference.Revision = cmd.Revision
		if err := couchdb.UpdateDoc(inst, s); err != nil {
			return false, err
		}
	}
	return true, nil
}

func pushRemoval(inst *instance.Instance, cmd *Command) error {
	msg, err := job.NewMessage(&RemovalMessage{
		OrganizationID: cmd.OrganizationID,
		Reference:      cmd.Reference,
		Revision:       cmd.Revision,
	})
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{WorkerType: WorkerRemoval, Message: msg})
	return err
}

func workRemoval(ctx *job.TaskContext) error {
	var msg RemovalMessage
	if err := ctx.UnmarshalMessage(&msg); err != nil {
		return err
	}
	return remove(ctx, ctx.Publisher, ctx.Instance, &msg)
}

func remove(ctx context.Context, pub messaging.Publisher, inst *instance.Instance, msg *RemovalMessage) error {
	mu := referenceLock(inst, msg.Reference)
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	all, err := sharing.FindByReference(inst, msg.Reference.Source, msg.Reference.ID)
	if err != nil {
		return err
	}
	for _, s := range all {
		if s.Owner && s.Drive && !s.Active && s.Reference.Revision == msg.Revision {
			if err := s.Revoke(inst); err != nil {
				if !errors.Is(err, permission.ErrExpiredToken) {
					return fmt.Errorf("revoke drive %s: %w", s.SID, err)
				}
				// Replaying a completed revocation finds already-expired permissions.
				for _, m := range s.Members[1:] {
					if m.Status != sharing.MemberStatusRevoked {
						return fmt.Errorf("revoke drive %s: %w", s.SID, err)
					}
				}
			}
			if err := trash(inst, s); err != nil {
				return fmt.Errorf("remove drive %s: %w", s.SID, err)
			}
		}
	}
	return publishRemoved(ctx, pub, inst, msg)
}

// trash moves the root folder to the trash using the normal retention policy.
func trash(inst *instance.Instance, s *sharing.Sharing) error {
	rule := s.FirstFilesRule()
	if rule == nil || len(rule.Values) == 0 {
		return nil
	}
	fs := inst.VFS()
	dir, err := fs.DirByID(rule.Values[0])
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if vfs.IsInTrash(dir.Fullpath) {
		return nil
	}

	job.EnsureCleanOldTrashedTrigger(inst)
	_, err = vfs.TrashDir(fs, dir)
	return err
}

func publishRemoved(ctx context.Context, pub messaging.Publisher, inst *instance.Instance, msg *RemovalMessage) error {
	ref := msg.Reference
	req, err := rabbitmq.NewActivityRequest(inst.ContextName, rabbitmq.RoutingKeyOrgDriveRemoved, msg.OrganizationID, RemovedData{
		Reference: ref,
		Revision:  msg.Revision,
	})
	if err != nil {
		return err
	}
	if err := pub.Publish(ctx, req); err != nil {
		return fmt.Errorf("removed reply for %s/%s: %w", ref.Source, ref.ID, err)
	}
	log.Infof("%s/%s: drives removed from %s at revision %d", ref.Source, ref.ID, inst.Domain, msg.Revision)
	return nil
}
