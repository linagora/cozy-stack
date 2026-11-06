package lifecycle

import (
	"context"
	"time"

	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"golang.org/x/sync/errgroup"
)

// Reset will clean all the data from the instances, and most apps. It should
// be used only just before an import.
func Reset(inst *instance.Instance) error {
	instanceSettings, err := inst.SettingsDocument()
	if err != nil {
		return err
	}
	bitwardenSettings, err := settings.Get(inst)
	if err != nil && !couchdb.IsNotFoundError(err) {
		return err
	}
	if err = deleteAccounts(inst); err != nil {
		return err
	}
	removeTriggers(inst)
	if err = inst.VFS().Delete(); err != nil {
		return err
	}
	if err = couchdb.DeleteAllDBs(inst); err != nil {
		return err
	}

	// XXX CouchDB is eventually consistent, which means that we have a small
	// risk that recreating a database just after the deletion can fail
	// silently. So, we wait 2 seconds to limit the risk.
	time.Sleep(2 * time.Second)

	g, _ := errgroup.WithContext(context.Background())
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Files) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Apps) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Konnectors) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.OAuthClients) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Jobs) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Triggers) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Permissions) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Sharings) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.BitwardenCiphers) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.SessionsLogins) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Notifications) })
	g.Go(func() error { return resetRecentsDB(inst) })
	g.Go(func() error { return couchdb.CreateDB(inst, consts.Contacts) })
	g.Go(func() error {
		if err := couchdb.CreateDB(inst, consts.Settings); err != nil {
			return err
		}
		if bitwardenSettings != nil {
			bitwardenSettings.SetRev("")
			if err := bitwardenSettings.Save(inst); err != nil {
				return err
			}
		}
		instanceSettings.SetRev("")
		return couchdb.CreateNamedDocWithDB(inst, instanceSettings)
		// The myself contact is created by the import, not here, so that this
		// document has the same ID than on the source instance.
	})
	if err = g.Wait(); err != nil {
		return err
	}

	if err = DefineViewsAndIndex(inst); err != nil {
		return err
	}
	if err = inst.VFS().InitFs(); err != nil {
		return err
	}

	for _, app := range []string{"home", "store", "settings"} {
		if err = installApp(inst, app); err != nil {
			inst.Logger().Errorf("Failed to install %s: %s", app, err)
		}
	}
	return nil
}

// resetRecentsDB creates an empty personal recents database. While the
// databases are deleted, another instance of the stack can still record an
// activity of this user, as a sharing member, and so recreate this one: it is
// then reset, to start empty like the other databases. If it is recreated
// again in the meantime, it is kept as it is.
func resetRecentsDB(inst *instance.Instance) error {
	err := couchdb.CreateDB(inst, consts.FilesRecents)
	if !couchdb.IsFileExists(err) {
		return err
	}
	err = couchdb.ResetDB(inst, consts.FilesRecents)
	if !couchdb.IsFileExists(err) {
		return err
	}
	return couchdb.EnsureDBExist(inst, consts.FilesRecents)
}
