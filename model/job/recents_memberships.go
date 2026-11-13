package job

// RecentsMembershipsWorker is the type of the worker that moves the personal
// recents rows of the members whose drives have changed at an owner.
const RecentsMembershipsWorker = "recents-memberships"

// RecentsMembershipsMessage is the message of a recents-memberships job: the
// Cozy URLs of the members whose membership in a drive of the instance of the
// job has changed. It does not tell what has changed: the worker reads the
// current drives of each member and recomputes the drive of each of their rows.
type RecentsMembershipsMessage struct {
	Members []string `json:"members"`
}
