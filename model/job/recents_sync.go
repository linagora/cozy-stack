package job

// RecentsSyncWorker is the type of the worker that deletes the personal recents
// rows of the files and folders that have been destroyed.
const RecentsSyncWorker = "recents-sync"

// RecentsSyncMessage is the message of a recents-sync job: the IDs of files and
// folders of the instance of the job that may have been destroyed. The worker
// checks it, and leaves the rows of an item that still exists.
type RecentsSyncMessage struct {
	IDs []string `json:"ids"`
}
