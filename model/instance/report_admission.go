package instance

import (
	"context"
	"errors"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
)

// ErrStaleReportImport identifies a superseded or already completed import job.
var ErrStaleReportImport = errors.New("stale report import admission")

// ReportAdmissionLock orders import admission before report, directory and VFS
// locks. A report holds it through SMTP and its completion checkpoint. Like the
// other LongOperation users, this assumes a healthy distributed lock lease.
func (i *Instance) ReportAdmissionLock() lock.ErrorLocker {
	return config.Lock().LongOperation(i, "public-link-report-admission")
}

// ReportsPaused reads local server-owned state, not the imported settings or a
// job's possibly stale Instance. Missing/unreadable authority is not success.
func (i *Instance) ReportsPaused() (bool, error) {
	latest, err := i.reportImportInstance()
	if err != nil {
		return true, err
	}
	if latest.ReportImportPending || latest.Deleting || latest.Moved || latest.BlockingReason == BlockedMoving.Code {
		return true, nil
	}
	importing := latest.BlockingReason == BlockedImporting.Code
	if !importing {
		settings, err := latest.SettingsDocument()
		if err != nil {
			return true, err
		}
		if marker, exists := settings.M["importing"]; exists {
			var valid bool
			importing, valid = marker.(bool)
			if !valid {
				return true, errors.New("invalid report import marker")
			}
		}
	}
	if !importing {
		return false, nil
	}
	// Remember a legacy import even if its settings marker later disappears.
	// Keep an empty generation so its already queued import job can still enter.
	latest.ReportImportPending = true
	if err := couchdb.UpdateDoc(prefixer.GlobalPrefixer, latest); err != nil {
		return true, err
	}
	return true, nil
}

// PauseReportsForImport must run under ReportAdmissionLock before enqueueing.
// Its generation fences stale jobs even after a later import has completed.
func (i *Instance) PauseReportsForImport() (string, error) {
	latest, err := i.reportImportInstance()
	if err != nil {
		return "", err
	}
	latest.ReportImportID = utils.RandomString(32)
	latest.ReportImportPending = true
	if err = couchdb.UpdateDoc(prefixer.GlobalPrefixer, latest); err != nil {
		return "", err
	}
	i.acceptReportImportInstance(latest)
	return latest.ReportImportID, nil
}

// AdmitReportImport rejects superseded jobs before blocking or resetting the
// instance. Old jobs are accepted only on an instance with no newer admission.
func (i *Instance) AdmitReportImport(generation string) error {
	latest, err := i.reportImportInstance()
	if err != nil {
		return err
	}
	if generation == "" {
		if latest.ReportImportID != "" {
			return ErrStaleReportImport
		}
		latest.ReportImportID = utils.RandomString(32)
		latest.ReportImportPending = true
		if err = couchdb.UpdateDoc(prefixer.GlobalPrefixer, latest); err != nil {
			return err
		}
	} else if latest.ReportImportID != generation || !latest.ReportImportPending {
		return ErrStaleReportImport
	}
	i.acceptReportImportInstance(latest)
	return nil
}

// CompleteReportImport admits scheduling again only after a successful import
// and local cleanup, under admission. It does not attest any imported report.
func (i *Instance) CompleteReportImport(ctx context.Context, generation string) error {
	latest, err := i.reportImportInstance()
	if err != nil {
		return err
	}
	if generation == "" || latest.ReportImportID != generation || !latest.ReportImportPending {
		return ErrStaleReportImport
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	old := latest.Clone()
	latest.ReportImportPending = false
	if err := couchdb.UpdateDocWithOld(prefixer.GlobalPrefixer, latest, old); err != nil {
		return err
	}
	i.acceptReportImportInstance(latest)
	return nil
}

func (i *Instance) reportImportInstance() (*Instance, error) {
	if i.ID() == "" {
		return nil, errors.New("missing report import authority")
	}
	latest := &Instance{}
	if err := couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, i.ID(), latest); err != nil {
		return nil, err
	}
	return latest, nil
}

func (i *Instance) acceptReportImportInstance(latest *Instance) {
	latest.vfs = i.vfs
	latest.contextualDomain = i.contextualDomain
	*i = *latest
}
