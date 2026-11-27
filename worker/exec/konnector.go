// Package exec is for the exec worker, which covers both konnector and service
// execution.
package exec

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/pkg/registry"
	"github.com/spf13/afero"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	konnErrorLoginFailed         = "LOGIN_FAILED"
	konnErrorUserActionNeeded    = "USER_ACTION_NEEDED"
	konnErrorUserActionNeededCgu = "USER_ACTION_NEEDED.CGU_FORM"
)

type konnectorWorker struct {
	slug    string
	msg     *KonnectorMessage
	man     *app.KonnManifest
	workDir string

	err     error
	lastErr error
}

const (
	konnectorMsgTypeDebug    = "debug"
	konnectorMsgTypeInfo     = "info"
	konnectorMsgTypeWarning  = "warning"
	konnectorMsgTypeError    = "error"
	konnectorMsgTypeCritical = "critical"
)

// KonnectorMessage is the message structure sent to the konnector worker.
type KonnectorMessage struct {
	Account        string `json:"account"`        // Account is the identifier of the account
	Konnector      string `json:"konnector"`      // Konnector is the slug of the konnector
	FolderToSave   string `json:"folder_to_save"` // FolderToSave is the identifier of the folder
	BIWebhook      bool   `json:"bi_webhook,omitempty"`
	AccountDeleted bool   `json:"account_deleted,omitempty"`

	// Data contains the original value of the message, even fields that are not
	// part of our message definition.
	data json.RawMessage
}

// ToJSON returns a JSON reprensation of the KonnectorMessage
func (m *KonnectorMessage) ToJSON() string {
	return string(m.data)
}

// updateFolderToSave updates the message with the new dirID, and also the trigger
func (m *KonnectorMessage) updateFolderToSave(inst *instance.Instance, dir string) {
	m.FolderToSave = dir
	var d map[string]interface{}
	_ = json.Unmarshal(m.data, &d)
	d["folder_to_save"] = dir
	m.data, _ = json.Marshal(d)

	_ = couchdb.ForeachDocs(inst, consts.Triggers, func(_ string, data json.RawMessage) error {
		var infos *job.TriggerInfos
		if err := json.Unmarshal(data, &infos); err != nil {
			return err
		}
		var msg map[string]interface{}
		if err := json.Unmarshal(infos.Message, &msg); err != nil {
			return err
		}
		if msg["account"] != m.Account || msg["konnector"] != m.Konnector {
			return nil
		}
		msg["folder_to_save"] = dir
		var err error
		if infos.Message, err = json.Marshal(msg); err != nil {
			return err
		}
		return couchdb.UpdateDoc(inst, infos)
	})
}

func jobHookErrorCheckerKonnector(err error) bool {
	// If there was no previous error, we are fine to go on
	if err == nil {
		return true
	}

	lastError := err.Error()
	if strings.HasPrefix(lastError, konnErrorLoginFailed) ||
		strings.HasPrefix(lastError, konnErrorUserActionNeeded) {
		return false
	}
	return true
}

// beforeHookKonnector skips jobs from trigger that are failing on certain
// errors.
func beforeHookKonnector(j *job.Job) (bool, error) {
	var msg KonnectorMessage
	var slug string

	if err := json.Unmarshal(j.Message, &msg); err == nil {
		slug = msg.Konnector

		inst, err := lifecycle.GetInstance(j.DomainName())
		if err != nil {
			return false, err
		}

		flags, err := feature.GetFlags(inst)
		if err != nil {
			return false, err
		}
		skipMaintenance, err := flags.HasListItem("harvest.skip-maintenance-for", slug)
		if err != nil {
			return false, err
		}

		doc, err := app.GetMaintenanceOptions(slug)
		if err != nil {
			j.Logger().Warnf("konnector %q could not get local maintenance status", slug)
		} else if doc != nil {
			if j.Manual {
				opts, ok := doc["maintenance_options"].(map[string]interface{})
				if ok && opts["flag_disallow_manual_exec"] != true {
					return true, nil
				}
			}

			if skipMaintenance {
				j.Logger().Infof("skipping konnector %q's maintenance", slug)
				return true, nil
			} else {
				j.Logger().Infof("konnector %q has not been triggered because of its maintenance status", slug)
				return false, nil
			}
		}

		app, err := registry.GetApplication(slug, inst.Registries())
		if err != nil {
			j.Logger().Warnf("konnector %q could not get application to fetch maintenance status", slug)
		} else if app.MaintenanceActivated {
			if j.Manual && !app.MaintenanceOptions.FlagDisallowManualExec {
				return true, nil
			}

			if skipMaintenance {
				j.Logger().Infof("skipping konnector %q's maintenance", slug)
				return true, nil
			} else {
				j.Logger().Infof("konnector %q has not been triggered because of its maintenance status", slug)
				return false, nil
			}
		}

		if msg.BIWebhook {
			return true, nil
		}
	}

	if j.Manual || j.TriggerID == "" {
		return true, nil
	}

	state, err := job.GetTriggerState(j, j.TriggerID)
	if err != nil {
		return false, err
	}
	if state.Status == job.Errored {
		ignore :=
			strings.HasPrefix(state.LastError, konnErrorUserActionNeeded) &&
				state.LastError != konnErrorUserActionNeededCgu
		if strings.HasPrefix(state.LastError, konnErrorLoginFailed) {
			ignore = true
		}
		if ignore {
			j.Logger().
				WithField("account_id", msg.Account).
				WithField("slug", slug).
				Infof("Konnector ignore: %s", state.LastError)
			return false, nil
		}
	}
	return true, nil
}

func (w *konnectorWorker) PrepareWorkDir(ctx *job.TaskContext, i *instance.Instance) (string, func(), error) {
	cleanDir := func() {}

	// Reset the errors from previous runs on retries
	w.err = nil
	w.lastErr = nil

	var err error
	var data json.RawMessage
	var msg KonnectorMessage
	if err = ctx.UnmarshalMessage(&data); err != nil {
		return "", cleanDir, err
	}
	if err = json.Unmarshal(data, &msg); err != nil {
		return "", cleanDir, err
	}
	msg.data = data

	slug := msg.Konnector
	w.slug = slug
	w.msg = &msg

	w.man, err = app.GetKonnectorBySlugAndUpdate(i, slug,
		app.Copier(consts.KonnectorType, i), i.Registries())
	if errors.Is(err, app.ErrNotFound) {
		return "", cleanDir, job.BadTriggerError{Err: err}
	} else if err != nil {
		return "", cleanDir, err
	}

	// Check that the associated account is present.
	var acc *account.Account
	if msg.Account != "" && !msg.AccountDeleted {
		acc = &account.Account{}
		err = couchdb.GetDoc(i, consts.Accounts, msg.Account, acc)
		if couchdb.IsNotFoundError(err) {
			return "", cleanDir, job.BadTriggerError{Err: err}
		}
	}

	man := w.man
	// Upgrade "installed" to "ready"
	if err := app.UpgradeInstalledState(i, man); err != nil {
		return "", cleanDir, err
	}

	if man.State() != app.Ready {
		return "", cleanDir, errors.New("Konnector is not ready")
	}

	var workDir string
	osFS := afero.NewOsFs()
	workDir, err = afero.TempDir(osFS, "", "konnector-"+slug)
	if err != nil {
		return "", cleanDir, err
	}
	cleanDir = func() {
		_ = os.RemoveAll(workDir)
	}
	w.workDir = workDir
	workFS := afero.NewBasePathFs(osFS, workDir)

	fileServer := app.KonnectorsFileServer(i)
	err = copyFiles(workFS, fileServer, slug, man.Version(), man.Checksum())
	if err != nil {
		return "", cleanDir, err
	}

	// Create the folder in which the konnector has the right to write.
	if err = w.ensureFolderToSave(ctx, i, acc); err != nil {
		return "", cleanDir, err
	}

	// Make sure the konnector can write to this folder
	if err = w.ensurePermissions(i); err != nil {
		return "", cleanDir, err
	}

	// If we get the AccountDeleted flag on, we check if the konnector manifest
	// has defined an "on_delete_account" field, containing the path of the file
	// to execute on account deletation. If no such field is present, the job is
	// aborted.
	if w.msg.AccountDeleted {
		// make sure we are not executing a path outside of the konnector's
		// directory
		fileExecPath := path.Join("/", path.Clean(w.man.OnDeleteAccount()))
		fileExecPath = fileExecPath[1:]
		if fileExecPath == "" {
			return "", cleanDir, job.ErrAbort
		}
		return path.Join(workDir, fileExecPath), cleanDir, nil
	}

	return workDir, cleanDir, nil
}

// ensureFolderToSave tries hard to give a folder to the konnector where it can
// write its files if it needs to do so.
func (w *konnectorWorker) ensureFolderToSave(ctx *job.TaskContext, inst *instance.Instance, acc *account.Account) error {
	fs := inst.VFS()
	msg := w.msg

	if msg.FolderToSave == "" {
		return nil
	}

	// 1. Check if the folder identified by its ID exists
	dir, err := fs.DirByID(msg.FolderToSave)
	if err == nil {
		if !strings.HasPrefix(dir.Fullpath, vfs.TrashDirName) {
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	var sourceAccountIdentifier string
	if acc != nil && acc.Metadata != nil {
		sourceAccountIdentifier = acc.Metadata.SourceIdentifier
	}

	// 2. Check if the konnector has a reference to a folder
	start := []string{consts.Konnectors, consts.Konnectors + "/" + w.slug}
	end := []string{start[0], start[1], couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    start,
		EndKey:      end,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	if err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res); err == nil {
		count := 0
		dirID := ""
		for _, row := range res.Rows {
			dir := &vfs.DirDoc{}
			if err := couchdb.GetDoc(inst, consts.Files, row.ID, dir); err == nil {
				if strings.HasPrefix(dir.Fullpath, vfs.TrashDirName) {
					continue
				}
				if !hasCompatibleSourceAccountIdentifier(dir, sourceAccountIdentifier) {
					continue
				}
				count++
				dirID = row.ID
			}
		}
		if count == 1 {
			msg.updateFolderToSave(inst, dirID)
			return nil
		}
	}

	// 3 Check if a folder should be created
	if acc == nil {
		return nil
	}

	// 4. Find a path for the folder
	folderPath := acc.DefaultFolderPath
	if folderPath == "" {
		folderPath = acc.FolderPath // For legacy purposes
	}
	if folderPath == "" {
		folderPath = computeFolderPath(inst, w.man.Name(), acc)
	}

	// 5. Try to recreate the folder
	dir, err = vfs.MkdirAll(fs, folderPath)
	if err != nil {
		dir, err = fs.DirByPath(folderPath)
		if err != nil {
			log := inst.Logger().WithNamespace("konnector")
			log.Warnf("Can't create the default folder %s: %s", folderPath, err)
			return err
		}
	}
	msg.updateFolderToSave(inst, dir.ID())
	if len(dir.ReferencedBy) == 0 {
		dir.AddReferencedBy(couchdb.DocReference{
			Type: consts.Konnectors,
			ID:   consts.Konnectors + "/" + w.slug,
		})
		if sourceAccountIdentifier != "" {
			dir.AddReferencedBy(couchdb.DocReference{
				Type: consts.SourceAccountIdentifier,
				ID:   sourceAccountIdentifier,
			})
		}
		instanceURL := inst.PageURL("/", nil)
		if dir.CozyMetadata == nil {
			dir.CozyMetadata = vfs.NewCozyMetadata(instanceURL)
		} else {
			dir.CozyMetadata.CreatedOn = instanceURL
		}
		dir.CozyMetadata.CreatedByApp = w.slug
		dir.CozyMetadata.UpdatedByApp(&metadata.UpdatedByAppEntry{
			Slug:     w.slug,
			Date:     dir.CozyMetadata.UpdatedAt,
			Instance: instanceURL,
		})
		dir.CozyMetadata.SourceAccount = acc.ID()
		_ = couchdb.UpdateDoc(inst, dir)
	}

	// 6. Ensure that the account knows the folder path
	if acc.DefaultFolderPath == "" {
		acc.DefaultFolderPath = folderPath
		_ = couchdb.UpdateDoc(inst, acc)
	}

	return nil
}

func computeFolderPath(inst *instance.Instance, slug string, acc *account.Account) string {
	admin := inst.Translate("Tree Administrative")
	r := strings.NewReplacer("&", "_", "/", "_", "\\", "_", "#", "_",
		",", "_", "+", "_", "(", "_", ")", "_", "$", "_", "@", "_", "~",
		"_", "%", "_", ".", "_", "'", "_", "\"", "_", ":", "_", "*", "_",
		"?", "_", "<", "_", ">", "_", "{", "_", "}", "_")

	accountName := r.Replace(acc.Name)
	if accountName == "" {
		accountName = acc.ID()
	}

	title := cases.Title(language.Make(inst.Locale)).String(slug)
	return fmt.Sprintf("/%s/%s/%s", admin, title, accountName)
}

func hasCompatibleSourceAccountIdentifier(dir *vfs.DirDoc, sourceAccountIdentifier string) bool {
	if sourceAccountIdentifier == "" {
		return true
	}
	nb := 0
	for _, ref := range dir.ReferencedBy {
		if ref.Type == consts.SourceAccountIdentifier {
			if ref.ID == sourceAccountIdentifier {
				return true
			}
			nb++
		}
	}
	return nb == 0
}

// ensurePermissions checks that the konnector has the permissions to write
// files in the folder referenced by the konnector, and adds the permission if
// needed.
func (w *konnectorWorker) ensurePermissions(inst *instance.Instance) error {
	for {
		perms, err := permission.GetForKonnector(inst, w.slug)
		if err != nil {
			return err
		}
		value := consts.Konnectors + "/" + w.slug
		for _, rule := range perms.Permissions {
			if rule.Type == consts.Files && rule.Selector == couchdb.SelectorReferencedBy {
				for _, val := range rule.Values {
					if val == value {
						return nil
					}
				}
			}
		}
		rule := permission.Rule{
			Type:        consts.Files,
			Title:       "referenced folders",
			Description: "folders referenced by the konnector",
			Selector:    couchdb.SelectorReferencedBy,
			Values:      []string{value},
		}
		perms.Permissions = append(perms.Permissions, rule)
		err = couchdb.UpdateDoc(inst, perms)
		if !couchdb.IsConflictError(err) {
			return err
		}
	}
}

func copyFiles(workFS afero.Fs, fileServer appfs.FileServer, slug, version, shasum string) error {
	files, err := fileServer.FilesList(slug, version, shasum)
	if err != nil {
		return err
	}
	for _, file := range files {
		switch file {
		// The following files are completely useless for running a konnector, so we skip them
		// in order to lower pressure on underlying file storage backend during high konnector execution rate
		case
			"README.md",
			"package.json",
			".travis.yml",
			"LICENSE":
			continue
		// Backward compatibility with older konnector storage pattern
		// in unique tar file which has ben removed in #1332
		case app.KonnectorArchiveName:
			tarFile, err := fileServer.Open(slug, version, shasum, file)
			if err != nil {
				return err
			}
			err = extractTar(workFS, tarFile)
			if errc := tarFile.Close(); err == nil {
				err = errc
			}
			if err != nil {
				return err
			}
			continue
		}
		var src io.ReadCloser
		var dst io.WriteCloser
		src, err = fileServer.Open(slug, version, shasum, file)
		if err != nil {
			return err
		}
		dirname := path.Dir(file)
		if dirname != "." {
			if err = workFS.MkdirAll(dirname, 0755); err != nil {
				return err
			}
		}
		dst, err = workFS.OpenFile(file, os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, src)
		errc1 := dst.Close()
		errc2 := src.Close()
		if err != nil {
			return err
		}
		if errc1 != nil {
			return errc1
		}
		if errc2 != nil {
			return errc2
		}
	}
	return nil
}

func extractTar(workFS afero.Fs, tarFile io.ReadCloser) error {
	tr := tar.NewReader(tarFile)
	for {
		var hdr *tar.Header
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		dirname := path.Dir(hdr.Name)
		if dirname != "." {
			if err = workFS.MkdirAll(dirname, 0755); err != nil {
				return err
			}
		}
		var f afero.File
		f, err = workFS.OpenFile(hdr.Name, os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		errc := f.Close()
		if err != nil {
			return err
		}
		if errc != nil {
			return errc
		}
	}
}

func (w *konnectorWorker) Slug() string {
	return w.slug
}

func (w *konnectorWorker) PrepareCmdEnv(ctx *job.TaskContext, i *instance.Instance) (cmd string, env []string, err error) {
	parameters := w.man.Parameters()

	// The operator's secret is for the konnector published under this slug in
	// the registry, not for a code the user installed under the same slug.
	src, errSrc := url.Parse(w.man.Source())
	fromRegistry := errSrc == nil && src.Scheme == "registry" && src.Host == w.slug

	accountTypes, err := account.FindAccountTypesBySlug(w.slug, i.ContextName)
	if err == nil && fromRegistry && len(accountTypes) == 1 && accountTypes[0].HasSecretGrant() {
		secret := accountTypes[0].Secret
		if parameters == nil {
			parameters = map[string]interface{}{"secret": secret}
		} else {
			params := map[string]interface{}{}
			for k, v := range parameters {
				params[k] = v
			}
			params["secret"] = secret
			parameters = params
		}
	}

	paramsJSON, err := json.Marshal(parameters)
	if err != nil {
		return
	}

	language := w.man.Language()
	if language == "" {
		language = "node"
	}

	// Directly pass the job message as fields parameters
	fieldsJSON := w.msg.ToJSON()
	token := i.BuildKonnectorToken(w.man.Slug())

	payload, err := preparePayload(ctx, w.workDir)
	if err != nil {
		return "", nil, err
	}

	cmd = config.GetConfig().Konnectors.Cmd
	env = []string{
		"COZY_URL=" + i.PageURL("/", nil),
		"COZY_CREDENTIALS=" + token,
		"COZY_FIELDS=" + fieldsJSON,
		"COZY_PARAMETERS=" + string(paramsJSON),
		"COZY_PAYLOAD=" + payload,
		"COZY_LANGUAGE=" + language,
		"COZY_LOCALE=" + i.Locale,
		"COZY_TIME_LIMIT=" + ctxToTimeLimit(ctx),
		"COZY_JOB_ID=" + ctx.ID(),
		"COZY_JOB_MANUAL_EXECUTION=" + strconv.FormatBool(ctx.Manual()),
	}
	if triggerID, ok := ctx.TriggerID(); ok {
		env = append(env, "COZY_TRIGGER_ID="+triggerID)
	}
	return
}

func (w *konnectorWorker) Logger(ctx *job.TaskContext) logger.Logger {
	return ctx.Logger().WithField("slug", w.slug)
}

func (w *konnectorWorker) ScanOutput(ctx *job.TaskContext, i *instance.Instance, line []byte) error {
	var msg struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		NoRetry bool   `json:"no_retry"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Errorf("Could not parse stdout as JSON: %q", string(line))
	}

	// Truncate very long messages
	if len(msg.Message) > 4000 {
		msg.Message = msg.Message[:4000]
	}

	log := w.Logger(ctx)
	switch msg.Type {
	case konnectorMsgTypeDebug, konnectorMsgTypeInfo:
		log.Debug(msg.Message)
	case konnectorMsgTypeWarning, "warn":
		log.Warn(msg.Message)
	case konnectorMsgTypeError:
		// For retro-compatibility, we still use "error" logs as returned error,
		// only in the case that no "critical" message are actually returned. In
		// such case, We use the last "error" log as the returned error.
		w.lastErr = errors.New(msg.Message)
		log.Error(msg.Message)
	case konnectorMsgTypeCritical:
		w.err = errors.New(msg.Message)
		if msg.NoRetry {
			ctx.SetNoRetry()
		}
		log.Error(msg.Message)
	}

	realtime.GetHub().Publish(i,
		realtime.EventCreate,
		&couchdb.JSONDoc{Type: consts.JobEvents, M: map[string]interface{}{
			"type":    msg.Type,
			"message": msg.Message,
		}},
		nil)
	return nil
}

func (w *konnectorWorker) Error(i *instance.Instance, err error) error {
	if w.err != nil {
		return w.err
	}
	if w.lastErr != nil {
		return w.lastErr
	}
	return err
}

func (w *konnectorWorker) Commit(ctx *job.TaskContext, errjob error) error {
	log := w.Logger(ctx)
	if w.msg != nil {
		log = log.WithField("account_id", w.msg.Account)
		if w.msg.BIWebhook {
			log = log.WithField("bi_webhook", w.msg.BIWebhook)
		}
	}
	if w.man != nil {
		log = log.WithField("version", w.man.Version())
	}
	if errjob == nil {
		log.Info("Konnector success")
		// Clean the soft-deleted account
		msg := &KonnectorMessage{}
		if err := ctx.UnmarshalMessage(&msg); err == nil && msg.AccountDeleted {
			var doc couchdb.JSONDoc
			err := couchdb.GetDoc(ctx.Instance, consts.SoftDeletedAccounts, msg.Account, &doc)
			if err == nil {
				doc.Type = consts.SoftDeletedAccounts
				err = couchdb.DeleteDoc(ctx.Instance, &doc)
			}
			if err != nil {
				log.Warnf("Cannot clean soft-deleted account: %s", err)
			}
		}
	} else {
		log.Infof("Konnector failure: %s", errjob)
	}
	return nil
}
