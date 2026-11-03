package lifecycle

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	job "github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/rag"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/mail"
	"github.com/labstack/echo/v4"
)

func AskDeletion(inst *instance.Instance) error {
	clouderies := config.GetConfig().Clouderies
	var cloudery config.ClouderyConfig
	cloudery, ok := clouderies[inst.ContextName]
	if !ok {
		cloudery = clouderies[config.DefaultInstanceContext]
	}

	api := cloudery.API
	clouderyURL := api.URL
	clouderyToken := api.Token

	u, err := url.Parse(clouderyURL)
	if err != nil {
		return err
	}
	u.Path = "/api/admin/instances/" + url.PathEscape(inst.UUID)

	req, err := http.NewRequest(http.MethodDelete, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Add(echo.HeaderAuthorization, "Bearer "+clouderyToken)
	res, err := managerHTTPClient.Do(req)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// Destroy is used to remove the instance. All the data linked to this
// instance will be permanently deleted.
func Destroy(domain string) error {
	domain, err := validateDomain(domain)
	if err != nil {
		return err
	}
	inst, err := instance.Get(domain)
	if err != nil {
		return err
	}

	// Check that we don't try to run twice the deletion of accounts
	if inst.Deleting {
		return instance.ErrDeletionAlreadyRequested
	}
	inst.Deleting = true
	if err := instance.Update(inst); err != nil {
		return err
	}

	// Deleting accounts manually to invoke the "account deletion hook" which may
	// launch a worker in order to clean the account.
	if err := deleteAccounts(inst); err != nil {
		sendAlert(inst, err)
		return err
	}

	if err := rag.CleanInstance(inst); err != nil {
		return err
	}

	// Reload the instance, it can have been updated in CouchDB if the instance
	// had at least one account and was not up-to-date for its indexes/views.
	inst, err = instance.Get(domain)
	if err != nil {
		return err
	}
	inst.Deleting = false
	_ = instance.Update(inst)

	removeTriggers(inst)

	// The recents rows of the members of the sharings on the items of the
	// instance are followed once its databases are deleted.
	followRecents := recents.PrepareDestroy(inst)

	if err = couchdb.DeleteAllDBs(inst); err != nil {
		inst.Logger().Errorf("Could not delete all CouchDB databases: %s", err.Error())
		return err
	}
	followRecents()

	if err = inst.VFS().Delete(); err != nil {
		inst.Logger().Errorf("Could not delete VFS: %s", err.Error())
		return err
	}

	err = instance.Delete(inst)
	if couchdb.IsConflictError(err) {
		// We may need to try again as CouchDB can return an old version of
		// this document when we have concurrent updates for indexes/views
		// version and deleting flag.
		time.Sleep(3 * time.Second)
		inst, errg := instance.Get(domain)
		if couchdb.IsNotFoundError(errg) {
			err = nil
		} else if inst != nil {
			err = instance.Delete(inst)
		}
	}
	return err
}

func deleteAccounts(inst *instance.Instance) error {
	var accounts []*account.Account
	if err := couchdb.GetAllDocs(inst, consts.Accounts, nil, &accounts); err != nil {
		if couchdb.IsNoDatabaseError(err) {
			return nil
		}
		return err
	}

	var toClean []account.CleanEntry
	for _, acc := range accounts {
		// Accounts that are not tied to a konnector must not be deleted, and
		// the aggregator accounts in particular.
		slug := acc.AccountType
		if slug == "" {
			continue
		}
		man, err := app.GetKonnectorBySlug(inst, slug)
		if errors.Is(err, app.ErrNotFound) {
			copier := app.Copier(consts.KonnectorType, inst)
			installer, erri := app.NewInstaller(inst, copier,
				&app.InstallerOptions{
					Operation:  app.Install,
					Type:       consts.KonnectorType,
					SourceURL:  "registry://" + slug + "/stable",
					Slug:       slug,
					Registries: inst.Registries(),
				},
			)
			if erri == nil {
				if appManifest, erri := installer.RunSync(); erri == nil {
					man = appManifest.(*app.KonnManifest)
					err = nil
				}
			}
		}
		if err != nil {
			return err
		}
		entry := account.CleanEntry{
			Account:          acc,
			Triggers:         nil, // We don't care, the triggers will all be deleted a bit later
			ManifestOnDelete: man.OnDeleteAccount() != "",
			Slug:             slug,
		}
		toClean = append(toClean, entry)
	}
	if len(toClean) == 0 {
		return nil
	}

	return account.CleanAndWait(inst, toClean)
}

func sendAlert(inst *instance.Instance, e error) {
	alert := config.GetConfig().AlertAddr
	if alert == "" {
		return
	}
	addr := &mail.Address{
		Name:  "Support",
		Email: alert,
	}
	values := map[string]interface{}{
		"Domain": inst.Domain,
		"Error":  e.Error(),
	}
	msg, err := job.NewMessage(mail.Options{
		Mode:           mail.ModeFromUser,
		To:             []*mail.Address{addr},
		TemplateName:   "alert_account",
		TemplateValues: values,
		Layout:         mail.CozyCloudLayout,
		Locale:         consts.DefaultLocale,
	})
	if err == nil {
		_, _ = job.System().PushJob(inst, &job.JobRequest{
			WorkerType: "sendmail",
			Message:    msg,
		})
	}
}

func removeTriggers(inst *instance.Instance) {
	sched := job.SystemUnsafe()
	if sched == nil {
		return
	}
	triggers, err := sched.GetAllTriggers(inst)
	if err == nil {
		for _, t := range triggers {
			if err = sched.DeleteTrigger(inst, t.Infos().TID); err != nil {
				logger.WithDomain(inst.Domain).Errorf(
					"Failed to remove trigger: %s", err)
			}
		}
	}
}
