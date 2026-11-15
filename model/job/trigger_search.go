package job

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
)

// searchTriggerInfos is the trigger of the search-index jobs (the worker is
// in worker/search; model/search cannot be imported here).
var searchTriggerInfos = TriggerInfos{
	Type:       "@event",
	WorkerType: "search-index",
	Arguments:  consts.Files + " " + consts.Sharings,
	Debounce:   "5s",
}

// HasSearchTrigger tells if the instance has the trigger of the search-index
// jobs.
func HasSearchTrigger(inst *instance.Instance) bool {
	return System().HasTrigger(inst, searchTriggerInfos)
}

// EnsureSearchTrigger creates the trigger of the search-index jobs, that
// follows the changes of the files and sharings, when search is configured
// and the instance does not have it yet.
func EnsureSearchTrigger(inst *instance.Instance) error {
	if config.GetConfig().Search.URL == nil || HasSearchTrigger(inst) {
		return nil
	}
	infos := searchTriggerInfos
	trigger, err := NewTrigger(inst, infos, nil)
	if err != nil {
		return err
	}
	return System().AddTrigger(trigger)
}
