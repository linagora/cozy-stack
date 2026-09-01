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
	lifecycle.RefreshBanners = func(domain string) { refreshQuota(domain, -1) }
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
