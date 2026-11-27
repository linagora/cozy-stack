package job

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
)

// searchTriggerInfos is the trigger of the search-index jobs (the worker is
// in worker/search; model/search cannot be imported here). It has no
// arguments, so that a trigger is found whatever its arguments are.
var searchTriggerInfos = TriggerInfos{
	Type:       "@event",
	WorkerType: "search-index",
	Debounce:   "5s",
}

// searchTriggerArguments are the doctypes whose changes push the jobs.
const searchTriggerArguments = consts.Files + " " + consts.Sharings

// HasSearchTrigger tells if the instance has the trigger of the search-index
// jobs.
func HasSearchTrigger(inst *instance.Instance) bool {
	return System().HasTrigger(inst, searchTriggerInfos)
}

// EnsureSearchTrigger creates the trigger of the search-index jobs, that
// follows the changes of the files and sharings, when search is configured
// and the instance does not have it yet.
//
// shortcut: two calls at the same time, or a CouchDB error while looking for
// the trigger, can add a second one; its jobs find nothing left to index, as
// the index lock serializes them. Remove the duplicate if that happens.
func EnsureSearchTrigger(inst *instance.Instance) error {
	if config.GetConfig().Search == nil || HasSearchTrigger(inst) {
		return nil
	}
	infos := searchTriggerInfos
	infos.Arguments = searchTriggerArguments
	trigger, err := NewTrigger(inst, infos, nil)
	if err != nil {
		return err
	}
	return System().AddTrigger(trigger)
}
