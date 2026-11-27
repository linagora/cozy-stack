package trash

import (
	"fmt"
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/hashicorp/go-multierror"
	"github.com/justincampbell/bigduration"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "trash-files",
		Concurrency:  runtime.NumCPU() * 4,
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      2 * time.Hour,
		WorkerFunc:   WorkerTrashFiles,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "clean-old-trashed",
		Concurrency:  runtime.NumCPU() * 4,
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      2 * time.Hour,
		WorkerFunc:   WorkerCleanOldTrashed,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "clean-uploads",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 2,
		Reserved:     true,
		Timeout:      1 * time.Hour,
		WorkerFunc:   WorkerCleanUploads,
	})
}

// WorkerCleanUploads removes the resumable uploads that have expired, with
// their content.
func WorkerCleanUploads(ctx *job.TaskContext) error {
	removed, err := vfs.CleanUploads(ctx.Instance.VFS())
	if removed > 0 {
		ctx.Logger().Infof("Removed %d expired uploads", removed)
	}
	return err
}

// WorkerTrashFiles is a worker to remove files in Swift after they have been
// removed from CouchDB. It is used when cleaning the trash, as removing a lot
// of files from Swift can take some time.
func WorkerTrashFiles(ctx *job.TaskContext) error {
	opts := vfs.TrashJournal{}
	if err := ctx.UnmarshalMessage(&opts); err != nil {
		return err
	}
	fs := ctx.Instance.VFS()
	if err := fs.EnsureErased(opts); err != nil {
		ctx.Logger().WithField("critical", "true").
			Errorf("Error: %s", err)
		return err
	}
	return nil
}

// WorkerCleanOldTrashed is a worker used to automatically delete files and
// directories that are in the trash for too long. The threshold for deletion
// is configurable per context in the config file, via the
// fs.auto_clean_trashed_after parameter.
func WorkerCleanOldTrashed(ctx *job.TaskContext) error {
	cfg := config.GetConfig().Fs.AutoCleanTrashedAfter
	after, ok := cfg[ctx.Instance.ContextName]
	if !ok || after == "" {
		return nil
	}
	delay, err := bigduration.ParseDuration(after)
	if err != nil {
		ctx.Logger().WithField("critical", "true").
			Errorf("Invalid config for auto_clean_trashed_after: %s", err)
		return err
	}
	before := time.Now().Add(-delay)

	var list []*vfs.DirOrFileDoc
	sel := mango.And(
		mango.Equal("dir_id", consts.TrashDirID),
		mango.Lt("updated_at", before),
	)
	req := &couchdb.FindRequest{
		UseIndex: "by-dir-id-updated-at",
		Selector: sel,
		Limit:    1000,
	}
	if _, err := couchdb.FindDocsRaw(ctx.Instance, consts.Files, req, &list); err != nil {
		return err
	}

	var errm error
	fs := ctx.Instance.VFS()
	push := pushTrashJob(fs)
	for _, item := range list {
		d, f := item.Refine()
		if f != nil {
			err = fs.DestroyFile(f)
		} else if d != nil {
			err = fs.DestroyDirAndContent(d, push)
		} else {
			err = fmt.Errorf("Invalid type for %v", item)
		}
		if err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

func pushTrashJob(fs vfs.VFS) func(vfs.TrashJournal) error {
	return func(journal vfs.TrashJournal) error {
		return fs.EnsureErased(journal)
	}
}
