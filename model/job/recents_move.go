package job

// RecentsMoveWorker is the type of the worker that moves the personal recents
// rows of items recreated under a new ID by a move to the shared drive of
// another owner on the same stack.
const RecentsMoveWorker = "recents-move"

// RecentsMoveBatchSize is the maximum number of items of a recents-move job: a
// move of more items is pushed as several jobs.
const RecentsMoveBatchSize = 200

// RecentsMovePair is an item recreated under a new ID by a move: its personal
// recents rows are to be moved from the old ID to the new one.
type RecentsMovePair struct {
	OldID string `json:"old_id"`
	NewID string `json:"new_id"`
}

// RecentsMoveMessage is the message of a recents-move job, on the instance of
// the source owner: the items that a move has recreated on the instance of
// another owner.
type RecentsMoveMessage struct {
	Pairs []RecentsMovePair `json:"pairs"`
	// DestOwnerDomain is the domain of the instance of the owner of the
	// destination.
	DestOwnerDomain string `json:"dest_owner_domain"`
	// SourceDriveID is the sharing ID of the source drive, or empty when the
	// source is the personal space of the owner.
	SourceDriveID string `json:"source_drive_id,omitempty"`
}
