package move

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	multierror "github.com/hashicorp/go-multierror"
)

// ImportOptions contains the options for launching the import worker.
type ImportOptions struct {
	SettingsURL      string       `json:"url,omitempty"`
	ManifestURL      string       `json:"manifest_url,omitempty"`
	Vault            bool         `json:"vault,omitempty"`
	MoveFrom         *FromOptions `json:"move_from,omitempty"`
	ReportGeneration string       `json:"report_generation,omitempty"`
}

// FromOptions is used when the import finishes to notify the source Cozy.
type FromOptions struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// CheckImport returns an error if an exports cannot be found at the given URL,
// or if the instance has not enough disk space to import the files.
func CheckImport(inst *instance.Instance, settingsURL string) error {
	manifestURL, err := transformSettingsURLToManifestURL(settingsURL)
	if err != nil {
		inst.Logger().WithNamespace("move").
			Debugf("Invalid settings URL %s: %s", settingsURL, err)
		return ErrExportNotFound
	}
	manifest, err := fetchManifest(manifestURL)
	if err != nil {
		inst.Logger().WithNamespace("move").
			Warnf("Cannot fetch manifest: %s", err)
		return ErrExportNotFound
	}
	if inst.BytesDiskQuota > 0 && manifest.TotalSize > inst.BytesDiskQuota {
		return ErrNotEnoughSpace
	}
	return nil
}

// ScheduleImport blocks the instance and adds a job to import the data from
// the given URL.
func ScheduleImport(ctx context.Context, inst *instance.Instance, options ImportOptions) error {
	manifestURL, err := transformSettingsURLToManifestURL(options.SettingsURL)
	if err != nil {
		return ErrExportNotFound
	}
	options.ManifestURL = manifestURL
	return ScheduleImportFromManifest(ctx, inst, options)
}

// ScheduleImportFromManifest admits an admin import with a direct manifest URL.
func ScheduleImportFromManifest(ctx context.Context, inst *instance.Instance, options ImportOptions) error {
	if options.ManifestURL == "" {
		return ErrExportNotFound
	}
	admission, err := lockImportAdmission(ctx, inst)
	if err != nil {
		return err
	}
	defer admission.Unlock()
	options.SettingsURL = ""
	options.ReportGeneration, err = inst.PauseReportsForImport()
	if err != nil {
		return err
	}
	settings, err := inst.SettingsDocument()
	if err != nil {
		return err
	}
	settings.M["importing"] = true
	if err = couchdb.UpdateDoc(inst, settings); err != nil {
		return err
	}
	msg, err := job.NewMessage(options)
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "import",
		Message:    msg,
	})
	return err
}

func transformSettingsURLToManifestURL(settingsURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(settingsURL))
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(u.Host, consts.SettingsSlug+".") {
		// Nested subdomains
		u.Host = strings.TrimPrefix(u.Host, consts.SettingsSlug+".")
	} else {
		// Flat subdomains
		parts := strings.Split(u.Host, ".")
		parts[0] = strings.TrimSuffix(parts[0], "-"+consts.SettingsSlug)
		u.Host = strings.Join(parts, ".")
	}
	if !strings.HasPrefix(u.Fragment, "/exports/") {
		return "", fmt.Errorf("Fragment is not in the expected format")
	}
	mac := strings.TrimPrefix(u.Fragment, "/exports/")
	u.Fragment = ""
	u.Path = "/move/exports/" + mac
	u.RawPath = ""
	u.RawQuery = ""
	return u.String(), nil
}

func fetchManifest(manifestURL string) (*ExportDoc, error) {
	res, err := safehttp.ClientWithKeepAlive.Get(manifestURL)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, ErrExportNotFound
	}
	doc := &ExportDoc{}
	if _, err = jsonapi.Bind(res.Body, doc); err != nil {
		return nil, err
	}
	if doc.State != ExportStateDone {
		return nil, ErrExportNotFound
	}
	return doc, nil
}

// Import downloads the documents and files from an export and add them to the
// local instance. It returns the list of slugs for apps/konnectors that have
// not been installed.
func Import(ctx context.Context, inst *instance.Instance, options ImportOptions) (notInstalled []string, err error) {
	admission, err := lockImportAdmission(ctx, inst)
	if err != nil {
		return nil, err
	}
	defer admission.Unlock()
	if err = inst.AdmitReportImport(options.ReportGeneration); err != nil {
		return nil, err
	}
	generation := inst.ReportImportID
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = lifecycle.Block(inst, instance.BlockedImporting.Code); err != nil {
		return nil, err
	}
	structuralSuccess := false
	allowDelete := false
	defer func() {
		if allowDelete {
			if cleanupErr := GetStore().ClearAllowDeleteAccounts(inst); cleanupErr != nil {
				err = multierror.Append(err, cleanupErr)
			}
		}
		settings, cleanupErr := inst.SettingsDocument()
		if cleanupErr == nil {
			delete(settings.M, "importing")
			cleanupErr = couchdb.UpdateDoc(inst, settings)
		}
		if cleanupErr != nil {
			err = multierror.Append(err, cleanupErr)
		}
		if unblockErr := unblockAfterImport(inst); unblockErr != nil {
			err = multierror.Append(err, unblockErr)
		}
		// A nil named error during panic unwinding is not import success.
		if structuralSuccess && err == nil {
			if err = ctx.Err(); err == nil {
				err = inst.CompleteReportImport(ctx, generation)
			}
		}
	}()

	doc, err := fetchManifest(options.ManifestURL)
	if err != nil {
		return nil, err
	}

	if err = GetStore().SetAllowDeleteAccounts(inst); err != nil {
		return nil, err
	}
	allowDelete = true
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = lifecycle.Reset(inst); err != nil {
		return nil, err
	}
	if err = GetStore().ClearAllowDeleteAccounts(inst); err != nil {
		return nil, err
	}
	allowDelete = false

	im := &importer{
		inst:            inst,
		fs:              inst.VFS(),
		options:         options,
		doc:             doc,
		servicesInError: make(map[string]bool),
	}
	if err = im.importPart(""); err != nil {
		return nil, err
	}
	for _, cursor := range doc.PartsCursors {
		if erri := ctx.Err(); erri != nil {
			return nil, multierror.Append(err, erri)
		}
		if erri := im.importPart(cursor); erri != nil {
			err = multierror.Append(err, erri)
		}
	}
	if err != nil {
		return nil, err
	}

	var inError []string
	for slug := range im.servicesInError {
		inError = append(inError, slug)
	}
	sort.Strings(inError)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	structuralSuccess = true
	return inError, nil
}

// Import waits behind an admitted report, including its SMTP and checkpoint,
// while a healthy Redis lease can take more than one acquisition window.
func lockImportAdmission(ctx context.Context, inst *instance.Instance) (lock.ErrorLocker, error) {
	admission := inst.ReportAdmissionLock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		err := admission.Lock()
		if errors.Is(err, lock.ErrTooManyRetries) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			admission.Unlock()
			return nil, err
		}
		return admission, nil
	}
}

func unblockAfterImport(inst *instance.Instance) error {
	if err := lifecycle.Unblock(inst); err == nil {
		return nil
	}
	time.Sleep(10 * time.Second)
	latest, err := instance.Get(inst.Domain)
	if err != nil {
		return err
	}
	return lifecycle.Unblock(latest)
}

// ImportIsFinished returns true unless an import is running
func ImportIsFinished(inst *instance.Instance) bool {
	settings, err := inst.SettingsDocument()
	if err != nil {
		return false
	}
	importing, _ := settings.M["importing"].(bool)
	return !importing
}

// Status is a type for the status of an import.
type Status int

const (
	// StatusMoveSuccess is the status when a move has been successful.
	StatusMoveSuccess Status = iota + 1
	// StatusImportSuccess is the status when a import of a tarball has been
	// successful.
	StatusImportSuccess
	// StatusMoveFailure is the status when the move has failed.
	StatusMoveFailure
	// StatusImportFailure is the status when the import has failed.
	StatusImportFailure
)

// SendImportDoneMail sends an email to the user when the import is done. The
// content will depend if the import has been successful or not, and if it was
// a move or just a import of a tarball.
func SendImportDoneMail(inst *instance.Instance, status Status, notInstalled []string) error {
	var email mail.Options
	switch status {
	case StatusMoveSuccess, StatusImportSuccess:
		tmpl := "import_success"
		if status == StatusMoveSuccess {
			tmpl = "move_success"
		}
		publicName, _ := csettings.PublicName(inst)
		link := inst.SubDomain(consts.HomeSlug)
		email = mail.Options{
			Mode:         mail.ModeFromStack,
			TemplateName: tmpl,
			TemplateValues: map[string]interface{}{
				"AppsNotInstalled": strings.Join(notInstalled, ", "),
				"CozyLink":         link.String(),
				"PublicName":       publicName,
			},
		}
	case StatusMoveFailure:
		email = mail.Options{
			Mode:         mail.ModeFromStack,
			TemplateName: "move_error",
		}
	case StatusImportFailure:
		email = mail.Options{
			Mode:         mail.ModeFromStack,
			TemplateName: "import_error",
		}
	default:
		return fmt.Errorf("Unknown import status: %v", status)
	}

	msg, err := job.NewMessage(&email)
	if err != nil {
		return err
	}
	_, err = job.System().PushJob(inst, &job.JobRequest{
		WorkerType: "sendmail",
		Message:    msg,
	})
	return err
}
