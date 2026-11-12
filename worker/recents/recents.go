// Package recents contains the worker that propagates the changes of the files
// and folders of an instance to the personal recents rows.
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
	"github.com/cozy/cozy-stack/model/vfs"
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

// recipient is a user who can hold rows on the items of the owner.
type recipient struct {
	inst *instance.Instance
}

// WorkerSync updates or deletes the existing rows of the items of the job, from
// the current state of these items. It runs on the instance of their owner.
//
// The ID of an item identifies it across the instances of the stack, so a row
// with this ID is a row on this item. The rows are read first, and only them
// cost anything else: most recipients have no row on most items.
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

	s := newSyncer(ctx.Instance)
	var errs []error
	for _, r := range findRecipients(ctx.Instance) {
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

// findRecipients returns the owner, when they have the recents enabled.
func findRecipients(owner *instance.Instance) []recipient {
	if !recents.Enabled(owner) {
		return nil
	}
	return []recipient{{inst: owner}}
}

// syncer brings rows up to date with the items of an owner.
type syncer struct {
	owner *instance.Instance
	items map[string]*item
}

func newSyncer(owner *instance.Instance) *syncer {
	return &syncer{owner: owner, items: make(map[string]*item)}
}

func (s *syncer) syncRow(r recipient, row *recents.FileDirActivity) error {
	it, err := s.item(row.DocID)
	if err != nil {
		return err
	}
	if it.destroyed() {
		return recents.DeleteRow(r.inst, row)
	}
	// A trashed item is not an exception here: the row of the owner goes on
	// telling where the item is, and no field of the row tells it otherwise.
	return recents.RefreshRow(r.inst, row, it.metadata())
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

func (it *item) metadata() recents.Metadata {
	if it.dir != nil {
		return recents.DirMetadata(it.dir)
	}
	return recents.FileMetadata(it.file, it.fullpath)
}
