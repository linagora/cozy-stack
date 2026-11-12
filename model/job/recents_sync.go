package job

// RecentsSyncWorker is the type of the worker that propagates the changes of
// the files and folders to the personal recents rows.
const RecentsSyncWorker = "recents-sync"

// RecentsSyncMessage is the message of a recents-sync job: the IDs of the files
// and folders of the instance of the job that have changed. It does not tell
// what has changed: the worker reads the current state of the items.
type RecentsSyncMessage struct {
	IDs []string `json:"ids"`
}
