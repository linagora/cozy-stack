package instances

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/move"
	"github.com/cozy/cozy-stack/model/orgdirectory"
	"github.com/cozy/cozy-stack/model/stack"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

func passwordDefinedFixer(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	if inst.PasswordDefined != nil {
		return c.NoContent(http.StatusNoContent)
	}

	defined := false
	if inst.OnboardingFinished {
		defined = true
		if inst.HasForcedOIDC() || inst.MagicLink {
			bitwarden, err := settings.Get(inst)
			if err == nil && !bitwarden.ExtensionInstalled {
				defined = false
			}
		}
	}
	inst.PasswordDefined = &defined
	if err := instance.Update(inst); err != nil {
		return c.JSON(http.StatusInternalServerError, echo.Map{
			"error": err,
		})
	}

	return c.NoContent(http.StatusNoContent)
}

func orphanAccountFixer(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	var accounts []*account.Account
	err = couchdb.GetAllDocs(inst, consts.Accounts, nil, &accounts)
	if err != nil || len(accounts) == 0 {
		return err
	}

	var konnectors []*couchdb.JSONDoc
	err = couchdb.GetAllDocs(inst, consts.Konnectors, nil, &konnectors)
	if err != nil {
		return err
	}

	var slugsToDelete []string
	for _, acc := range accounts {
		if acc.AccountType == "" {
			continue // Skip the design docs
		}
		found := false
		for _, konn := range konnectors {
			if konn.M["slug"] == acc.AccountType {
				found = true
				break
			}
		}
		if !found {
			for _, slug := range slugsToDelete {
				if slug == acc.AccountType {
					found = true
					break
				}
			}
			if !found {
				slugsToDelete = append(slugsToDelete, acc.AccountType)
			}
		}
	}
	if len(slugsToDelete) == 0 {
		return nil
	}

	if _, _, err = stack.Start(); err != nil {
		return err
	}
	jobsSystem := job.System()
	log := inst.Logger().WithNamespace("fixer")
	copier := app.Copier(consts.KonnectorType, inst)

	for _, slug := range slugsToDelete {
		opts := &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.KonnectorType,
			SourceURL:  "registry://" + slug + "/stable",
			Slug:       slug,
			Registries: inst.Registries(),
		}
		ins, err := app.NewInstaller(inst, copier, opts)
		if err != nil {
			return err
		}
		if _, err = ins.RunSync(); err != nil {
			return err
		}

		for _, acc := range accounts {
			if acc.AccountType != slug {
				continue
			}
			acc.ManualCleaning = true
			oldRev := acc.Rev() // The deletion job needs the rev just before the deletion
			if err := couchdb.DeleteDoc(inst, acc); err != nil {
				log.Errorf("Cannot delete account: %v", err)
			}
			j, err := account.PushAccountDeletedJob(jobsSystem, inst, acc.ID(), oldRev, slug)
			if err != nil {
				log.Errorf("Cannot push a job for account deletion: %v", err)
			}
			if err = j.WaitUntilDone(inst); err != nil {
				log.Error(err.Error())
			}
		}
		opts.Operation = app.Delete
		ins, err = app.NewInstaller(inst, copier, opts)
		if err != nil {
			return err
		}
		if _, err = ins.RunSync(); err != nil {
			return err
		}
	}

	return c.NoContent(http.StatusNoContent)
}

func sharingsMovedFixer(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	var errm error

	// First, update local sharing documents to use the new instance URL for "self"
	if err := move.UpdateSelfMemberInstance(inst); err != nil {
		inst.Logger().WithNamespace("fixer").
			Warnf("Error updating self member instance in sharings: %s", err)
		errm = multierror.Append(errm, err)
	}

	// Update all trigger domains to use the new domain
	if err := move.UpdateTriggersAfterMove(inst); err != nil {
		inst.Logger().WithNamespace("fixer").
			Warnf("Error updating trigger domains: %s", err)
		errm = multierror.Append(errm, err)
	}

	// Notify other instances about the move
	if err := move.NotifySharings(inst); err != nil {
		inst.Logger().WithNamespace("fixer").
			Warnf("Error notifying sharings: %s", err)
		errm = multierror.Append(errm, err)
	}

	if errm != nil {
		return errm
	}

	return c.NoContent(http.StatusNoContent)
}

type serviceMessage struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	// and some other fields not needed here
}

func serviceTriggersFixer(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	jobsSystem := job.System()
	triggers, err := jobsSystem.GetAllTriggers(inst)
	if err != nil {
		return err
	}
	byApps := make(map[string][]job.Trigger)
	for _, trigger := range triggers {
		trigger := trigger
		infos := trigger.Infos()
		if infos.WorkerType != "service" {
			continue
		}
		if infos.Type == "@at" {
			continue
		}
		var msg serviceMessage
		if err := json.Unmarshal(infos.Message, &msg); err != nil {
			continue
		}
		list := byApps[msg.Slug]
		list = append(list, trigger)
		byApps[msg.Slug] = list
	}

	var toDelete []job.Trigger
	recreated := 0
	updated := 0

	for slug, triggers := range byApps {
		manifest, err := app.GetWebappBySlug(inst, slug)
		if errors.Is(err, app.ErrNotFound) {
			// The app has been uninstalled, but some duplicate triggers has
			// been left
			toDelete = append(toDelete, triggers...)
			continue
		} else if err != nil {
			return err
		}

		// Fill the trigger ids for the services when they are missing.
		updateApp := false
		for name, service := range manifest.Services() {
			if service.TriggerOptions == "" {
				continue
			}
			var recreate bool
			if service.TriggerID == "" {
				for _, trigger := range triggers {
					infos := trigger.Infos()
					if infos.Debounce != service.Debounce {
						continue
					}
					opts := infos.Type + " " + infos.Arguments
					if opts != service.TriggerOptions {
						continue
					}
					var msg serviceMessage
					if err := json.Unmarshal(infos.Message, &msg); err != nil {
						continue
					}
					if msg.Name != name {
						continue
					}
					service.TriggerID = infos.TID
					updateApp = true
					break
				}
				recreate = service.TriggerID == ""
			} else {
				trigger, err := jobsSystem.GetTrigger(inst, service.TriggerID)
				recreate = errors.Is(err, job.ErrNotFoundTrigger)
				if err == nil {
					var msg serviceMessage
					if err := json.Unmarshal(trigger.Infos().Message, &msg); err != nil {
						return err
					}
					if msg.Name == "" {
						fixTriggerName(inst, trigger, msg, name)
						updated++
					}
				}
			}

			if recreate {
				triggerID, err := app.CreateServiceTrigger(inst, slug, name, service)
				if err != nil {
					return err
				}
				service.TriggerID = triggerID
				updateApp = true
				recreated++
			}
		}

		if updateApp {
			if err := couchdb.UpdateDoc(inst, manifest); err != nil {
				return err
			}
		}

		// Add to the list of triggers that should be deleted all the triggers
		// for this application that are not tied to a service.
		for _, trigger := range triggers {
			trigger := trigger
			tid := trigger.Infos().TID
			found := false
			for _, service := range manifest.Services() {
				if service.TriggerID == tid {
					found = true
				}
			}
			if !found {
				toDelete = append(toDelete, trigger)
			}
		}
	}

	for _, trigger := range toDelete {
		if err := jobsSystem.DeleteTrigger(inst, trigger.ID()); err != nil {
			return err
		}
	}

	return c.JSON(http.StatusOK, echo.Map{
		"Domain":                 domain,
		"RecreatedTriggersCount": recreated,
		"UpdatedTriggersCount":   updated,
		"DeletedTriggersCount":   len(toDelete),
	})
}

func fixTriggerName(inst *instance.Instance, trigger job.Trigger, msg serviceMessage, name string) error {
	msg.Name = name
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	infos := trigger.Infos().Clone().(*job.TriggerInfos)
	infos.Message = job.Message(raw)
	return couchdb.UpdateDoc(inst, infos)
}

func indexesFixer(c echo.Context) error {
	domain := c.Param("domain")
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}

	if err := lifecycle.DefineViewsAndIndex(inst); err != nil {
		return err
	}

	return c.NoContent(http.StatusNoContent)
}

func emailsFixer(c echo.Context) error {
	insts, err := instance.List()
	if err != nil {
		return wrapError(err)
	}
	dryRun := c.QueryParam("dry_run") == "true"
	return c.JSON(http.StatusOK, lifecycle.BackfillEmails(insts, dryRun))
}

func memberCopiesFixer(c echo.Context) error {
	dryRun := c.QueryParam("dry_run") == "true"
	report, err := orgdirectory.RemoveMemberCopies(c.Request().Context(), c.Param("org_id"), dryRun)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, report)
}
