package job

import (
	"fmt"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
)

// EnsureCleanUploadsTrigger schedules a daily removal of the expired
// resumable uploads of the instance.
func EnsureCleanUploadsTrigger(inst *instance.Instance) {
	sched := System()
	infos := TriggerInfos{
		Type:       "@cron",
		WorkerType: "clean-uploads",
	}
	if sched.HasTrigger(inst, infos) {
		return
	}

	now := time.Now()
	infos.Arguments = fmt.Sprintf("0 %d %d * * *", now.Minute(), now.Hour())
	trigger, err := NewTrigger(inst, infos, nil)
	if err != nil {
		inst.Logger().Errorf("Cannot create clean-uploads trigger: %s", err)
		return
	}
	if err = sched.AddTrigger(trigger); err != nil {
		inst.Logger().Errorf("Cannot create clean-uploads trigger: %s", err)
	}
}
