package moves

import (
	"runtime"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/move"
)

func init() {
	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "export",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 1,
		Timeout:      1 * time.Hour,
		Reserved:     true,
		WorkerFunc:   ExportWorker,
	})

	job.AddWorker(&job.WorkerConfig{
		WorkerType:   "import",
		Concurrency:  runtime.NumCPU(),
		MaxExecCount: 1,
		Timeout:      3 * time.Hour,
		Reserved:     true,
		WorkerFunc:   ImportWorker,
	})
}

// ExportWorker is the worker responsible for creating an export of the
// instance.
func ExportWorker(c *job.TaskContext) error {
	var opts move.ExportOptions
	if err := c.UnmarshalMessage(&opts); err != nil {
		return err
	}

	if opts.ContextualDomain != "" {
		c.Instance = c.Instance.WithContextualDomain(opts.ContextualDomain)
	}

	archiver := move.SystemArchiver()

	exportDoc, err := move.CreateExport(c.Instance, opts, archiver)
	if err != nil {
		c.Instance.Logger().WithNamespace("move").
			Warnf("Export failed: %s", err)
		if opts.MoveTo != nil {
			move.Abort(c.Instance, opts.MoveTo.URL, opts.MoveTo.Token)
		}
		if !opts.AdminReq {
			_ = move.SendExportFailureMail(c.Instance)
		}
		return err
	}

	if opts.AdminReq {
		exportDoc.NotifyRealtime()
		return nil
	}
	if opts.MoveTo != nil {
		return exportDoc.NotifyTarget(c.Instance, opts.MoveTo, opts.TokenSource, opts.IgnoreVault)
	}
	return exportDoc.SendExportMail(c.Instance)
}

// ImportWorker is the worker responsible for inserting the data from an export
// inside an instance.
func ImportWorker(c *job.TaskContext) error {
	var opts move.ImportOptions
	if err := c.UnmarshalMessage(&opts); err != nil {
		return err
	}

	if err := lifecycle.Block(c.Instance, instance.BlockedImporting.Code); err != nil {
		return err
	}

	inError, err := move.Import(c.Instance, opts)

	if erru := lifecycle.Unblock(c.Instance); erru != nil {
		// Try again
		time.Sleep(10 * time.Second)
		inst, errg := instance.Get(c.Instance.Domain)
		if errg == nil {
			erru = lifecycle.Unblock(inst)
		}
		if err == nil {
			err = erru
		}
	}

	status := move.StatusImportSuccess
	if err != nil {
		status = move.StatusImportFailure
		if opts.MoveFrom != nil {
			status = move.StatusMoveFailure
		}
		c.Instance.Logger().WithNamespace("move").
			Warnf("Import failed: %s", err)
	}

	if opts.MoveFrom != nil {
		if err == nil {
			status = move.StatusMoveSuccess
			move.CallFinalize(c.Instance, opts.MoveFrom.URL, opts.MoveFrom.Token, opts.Vault)
		} else {
			move.Abort(c.Instance, opts.MoveFrom.URL, opts.MoveFrom.Token)
		}
	}

	_ = move.SendImportDoneMail(c.Instance, status, inError)
	if err != nil {
		return err
	}

	if opts.MoveFrom == nil {
		return nil
	}
	return move.NotifySharings(c.Instance)
}
