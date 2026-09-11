package banner

import (
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/logger"
)

func init() {
	// A write that crosses the threshold, where reading the usage back is safe
	// because the file is already indexed. The rising edge only: on the downward
	// crossing this runs after the callback below and reads a usage the destroy
	// has not reached yet, resurrecting the banner it just removed.
	vfs.RegisterDiskQuotaAlertCallback(func(domain string, exceeded bool) {
		if exceeded {
			refreshQuota(domain, -1)
		}
	})

	// Freeing space, whether or not it crosses the threshold.
	//
	// ponytail: one indexed read per destroy operation. If a large trash
	// cleanup makes that show up, debounce per instance rather than per file.
	vfs.RegisterDiskUsageFreedCallback(refreshQuota)

	// The other half of the quota: the Cloudery moves the limit rather than
	// the usage, and a downgrade crosses the threshold with nothing written.
	// A language change lands here too, and reaches the commanded banners as
	// well as the ones the stack words itself.
	lifecycle.RefreshBanners = func(domain string) {
		refreshQuota(domain, -1)
		refreshCommands(domain)
	}
}

// refreshQuota re-evaluates the quota banner of an instance. A negative usage
// means read it back; a destroy passes the usage it will leave instead.
func refreshQuota(domain string, used int64) {
	if err := refreshQuotaAt(domain, used); err != nil {
		logger.WithDomain(domain).WithNamespace("banner").
			Warnf("cannot materialize the quota banner: %s", err)
	}
}

func refreshQuotaAt(domain string, used int64) error {
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	// Off by default. Turning the switch back off stops the writes but leaves
	// the documents already materialized: a rollback needs a cleanup too.
	if !inst.HasBannersEnabled() {
		return nil
	}

	// Two triggers for one instance can run at once, from separate uploads
	// or across stack processes. The fixed document id already stops both
	// from creating; the lock stops the loser's evaluation from being thrown
	// away on the conflict, so it re-reads and writes after the winner.
	mu := config.Lock().ReadWrite(inst, "banners")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	fs := inst.VFS()
	if used < 0 {
		if used, err = fs.DiskUsage(); err != nil {
			return err
		}
	}

	state := QuotaState{
		Used:        used,
		Quota:       fs.DiskQuota(),
		Locale:      inst.Locale,
		ContextName: inst.ContextName,
	}
	// The offers page is the only target the stack knows; without it the
	// banner is still worth displaying, just without its call to action.
	if premium, err := inst.ManagerURL(instance.ManagerPremiumURL); err == nil {
		state.SettingsURL = premium
	}

	now := time.Now()
	return Materialize(inst, CategoryQuota, EvaluateQuota(state, now), now)
}

// refreshCommands re-materializes the commanded banners of an instance from
// the commands the stack retained. It decides nothing: the wording, the
// revision and the decision time are the ones the backend already sent, and
// only the language is picked again. Without this a language change leaves a
// commanded banner in the previous language next to a stack written one in
// the new one.
func refreshCommands(domain string) {
	if err := refreshCommandsAt(domain); err != nil {
		logger.WithDomain(domain).WithNamespace("banner").
			Warnf("cannot refresh the commanded banners: %s", err)
	}
}

func refreshCommandsAt(domain string) error {
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	if !inst.HasBannersEnabled() {
		return nil
	}

	// The lock a command takes, so a refresh cannot race a newer command into
	// restoring the banner that command just replaced.
	mu := config.Lock().ReadWrite(inst, "banners")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	// ponytail: one _all_docs read per instance patch, including the quota
	// patches that share this hook and never need it. Split the hook by reason
	// if that read ever shows up.
	states, err := storedCommands(inst)
	if err != nil {
		return err
	}

	now := time.Now()
	for _, state := range states {
		// A cleared category holds no document, and a record from before the
		// stack retained the wording has nothing to pick from.
		if state.Clear || state.Accepted == nil {
			continue
		}
		// A category the context has stopped accepting is left alone rather
		// than rewritten; turning the setting off needs a cleanup either way.
		if !inst.AllowsBannerCategory(state.Category) {
			continue
		}
		// Re-localizing rewrites a banner, it does not restore one. The
		// retained command carries the wording, not the occurrence's own
		// start, which lives on the document an application is allowed to
		// delete; recreating from the command alone would move a scheduled
		// banner to the decision time of whichever command wrote last.
		current, err := Stored(inst, state.Category)
		if err != nil {
			return err
		}
		if current == nil {
			continue
		}
		if err := Materialize(inst, state.Category, state.Accepted.banner(inst.Locale), now); err != nil {
			return err
		}
	}
	return nil
}
