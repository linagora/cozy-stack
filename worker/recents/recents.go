// Package recents contains the worker that propagates the changes of the files
// and folders of an instance to the personal recents rows.
package recents

import (
	"errors"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
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

// recipient is a user who can hold rows on the items of the owner: the owner,
// without a member, or a member of one of their drives.
type recipient struct {
	inst   *instance.Instance
	member *sharing.Member
}

// WorkerSync updates or deletes the existing rows of the items of the job, from
// the current state of these items. It runs on the instance of their owner.
//
// The recipients are the same for an update and a destruction, and do not depend
// on the position of the item, which cannot be read any more once it is
// destroyed: the owner and all the ready members of the drives of the owner. The
// ID of an item identifies it across the instances of the stack, so a row with
// this ID is a row on this item. The rows are read first, and only them cost
// anything else: most recipients have no row on most items. For an update, a
// member with a row and no drive covering the item keeps the row as it is.
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

	s := newSyncer(ctx.Instance, ctx.Logger())
	var errs []error
	for _, r := range recipients {
		rows, err := recents.RowsByIDs(r.inst, ids)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, row := range rows {
			if err := s.syncRow(r, row); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// findRecipients returns the owner and the ready members of their drives that
// are on this stack, for those who have the recents enabled. A member of several
// drives is returned once: the drive is chosen for each item by DriveForMember.
func findRecipients(owner *instance.Instance) ([]recipient, error) {
	var recipients []recipient
	if recents.Enabled(owner) {
		recipients = append(recipients, recipient{inst: owner})
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

// localMembers returns the ready members of a drive that are on this stack and
// have the recents enabled, except those whose host has been seen already.
func localMembers(drive *sharing.Sharing, seen map[string]struct{}) []recipient {
	var recipients []recipient
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
			recipients = append(recipients, recipient{inst: inst, member: member})
		}
	}
	return recipients
}

// syncer brings rows up to date with the items of an owner.
type syncer struct {
	owner    *instance.Instance
	resolver *sharing.AccessResolver
	items    map[string]*item
	log      logger.Logger
}

func newSyncer(owner *instance.Instance, log logger.Logger) *syncer {
	return &syncer{
		owner:    owner,
		resolver: sharing.NewAccessResolver(owner),
		items:    make(map[string]*item),
		log:      log,
	}
}

func (s *syncer) syncRow(r recipient, row *recents.FileDirActivity) error {
	it, err := s.item(row.DocID)
	if err != nil {
		return err
	}
	if it.destroyed() {
		return recents.DeleteRow(r.inst, row)
	}
	meta, ok, err := s.metadataFor(r, row.DocID, it)
	if err != nil || !ok {
		return err
	}
	return recents.RefreshRow(r.inst, row, meta)
}

// metadataFor returns the metadata of an item for a recipient. It returns false
// for a member when the item is trashed or when none of their drives gives it.
func (s *syncer) metadataFor(r recipient, id string, it *item) (recents.Metadata, bool, error) {
	if r.member == nil {
		// A trashed item is not an exception here: the row of the owner goes on
		// telling where the item is, and no field of the row tells it otherwise.
		return it.metadata(), true, nil
	}
	// The path of a trashed item is under the trash and tells nothing about the
	// drives of the member: the row stays as it is until the item is restored.
	if it.trashed() {
		return recents.Metadata{}, false, nil
	}
	scope, err := s.resolver.DriveForMember(id, r.member)
	if err != nil || scope == nil {
		return recents.Metadata{}, false, err
	}
	meta, err := memberMetadata(s.owner, scope, it)
	if err != nil {
		s.log.Warnf("No path for %s in a drive of %s: %s", id, r.inst.Domain, err)
		return recents.Metadata{}, false, nil
	}
	return meta, true, nil
}

// item returns the current state of an item of the owner, read at most once by
// job, and only when there is a row on it.
func (s *syncer) item(id string) (*item, error) {
	if it, ok := s.items[id]; ok {
		return it, nil
	}
	it, err := loadItem(s.owner.VFS(), id)
	if err != nil {
		return nil, err
	}
	s.items[id] = it
	return it, nil
}

// item is the state of a file or a folder of the owner. It holds neither dir nor
// file when the item has been destroyed.
type item struct {
	dir      *vfs.DirDoc
	file     *vfs.FileDoc
	fullpath string
}

func loadItem(fs vfs.VFS, id string) (*item, error) {
	dir, file, err := fs.DirOrFileByID(id)
	if errors.Is(err, os.ErrNotExist) {
		return &item{}, nil
	}
	if err != nil {
		return nil, err
	}
	if dir != nil {
		return &item{dir: dir, fullpath: dir.Fullpath}, nil
	}
	if file == nil {
		return nil, errors.New("recents: the item is neither a file nor a folder")
	}
	fullpath, err := file.Path(fs)
	if err != nil {
		return nil, err
	}
	return &item{file: file, fullpath: fullpath}, nil
}

func (it *item) destroyed() bool {
	return it.dir == nil && it.file == nil
}

func (it *item) trashed() bool {
	if it.file != nil && it.file.Trashed {
		return true
	}
	return strings.HasPrefix(it.fullpath, vfs.TrashDirName+"/")
}

func (it *item) metadata() recents.Metadata {
	if it.dir != nil {
		return recents.DirMetadata(it.dir)
	}
	return recents.FileMetadata(it.file, it.fullpath)
}

// memberMetadata returns the metadata of an item for a member, in the drive
// chosen for them. It returns an error rather than a path that would show the
// tree of the owner above the drive.
func memberMetadata(owner *instance.Instance, scope *sharing.SharingScope, it *item) (recents.Metadata, error) {
	path, err := scope.PathFromRoot(it.fullpath)
	if err != nil {
		return recents.Metadata{}, err
	}
	meta := it.metadata()
	meta.Path = path
	meta.DriveID = scope.Sharing.SID
	meta.OwnerDomain = owner.Domain
	return meta, nil
}
