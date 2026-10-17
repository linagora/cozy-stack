// Package driveactivity counts the files the members create, update and
// delete in the shared drives that report their activity, and publishes the
// counts on the activity exchange once per window, member and top folder.
package driveactivity

import (
	"strconv"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
)

const defaultWindow = 30 * time.Second

// windowsBeforeReopen bounds how long a window stays open when its flush
// never ran, after which the next change opens a new one.
const windowsBeforeReopen = 10

// Count records a change made by member in the drive s. The change itself is
// already done, so the digest is best effort: Count only logs its errors.
func Count(inst *instance.Instance, s *sharing.Sharing, member *sharing.Member, kind string, file *vfs.FileDoc) {
	if file == nil || member == nil || member.Email == "" || !s.ReportsActivity() {
		return
	}
	window := windowOf(inst)
	start := strconv.FormatInt(time.Now().UnixMilli(), 10)
	c := change{member: member.Email, folder: file.DirID, kind: kind, name: file.DocName}
	opened, err := getStore().add(driveKey(inst, s.SID), c, start, windowsBeforeReopen*window)
	if err != nil {
		inst.Logger().WithNamespace("drive-activity").
			Warnf("Cannot count a change in drive %s: %s", s.SID, err)
		return
	}
	if !opened {
		return
	}
	if err := scheduleFlush(inst, s.SID, start, window); err != nil {
		inst.Logger().WithNamespace("drive-activity").
			Warnf("Cannot schedule the flush of drive %s: %s", s.SID, err)
	}
}

func windowOf(inst *instance.Instance) time.Duration {
	settings, ok := inst.SettingsContext()
	if !ok {
		return defaultWindow
	}
	cfg, _ := settings["drive_activity"].(map[string]interface{})
	w, _ := cfg["window"].(string)
	if d, err := time.ParseDuration(w); err == nil && d > 0 {
		return d
	}
	return defaultWindow
}

func scheduleFlush(inst *instance.Instance, sharingID, start string, window time.Duration) error {
	msg, err := job.NewMessage(&FlushMessage{SharingID: sharingID, Window: start})
	if err != nil {
		return err
	}
	t, err := job.NewTrigger(inst, job.TriggerInfos{
		Type:       "@in",
		WorkerType: WorkerFlush,
		Arguments:  window.String(),
	}, msg)
	if err != nil {
		return err
	}
	return job.System().AddTrigger(t)
}

func driveKey(inst *instance.Instance, sharingID string) string {
	return inst.DBPrefix() + "/" + sharingID
}
