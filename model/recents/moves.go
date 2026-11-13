package recents

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// MoveRow moves a row to a new ID: it writes the row under the new ID with the
// given metadata and the dates of the old row merged to the max with any row
// already there, then deletes the old row. It is idempotent: a replay finds the
// dates already merged and the old row gone.
func MoveRow(inst *instance.Instance, old *FileDirActivity, newID string, meta Metadata) error {
	if err := upsertMovedRow(inst, old, newID, meta); err != nil {
		return err
	}
	return DeleteRow(inst, old)
}

func upsertMovedRow(inst *instance.Instance, old *FileDirActivity, newID string, meta Metadata) error {
	for attempt := 1; ; attempt++ {
		var dst FileDirActivity
		err := couchdb.GetDoc(inst, consts.FilesRecents, newID, &dst)
		if couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err) {
			dst = FileDirActivity{DocID: newID, SchemaVersion: SchemaVersion}
			dst.setMetadata(meta)
			dst.LastEditedAt = old.LastEditedAt
			dst.LastViewedAt = old.LastViewedAt
			dst.LastActivityAt = max(dst.LastEditedAt, dst.LastViewedAt)
			err = couchdb.CreateNamedDocWithDB(inst, &dst)
			if (couchdb.IsConflictError(err) || couchdb.IsFileExists(err)) && attempt < maxWriteAttempts {
				continue
			}
			return err
		}
		if err != nil {
			return err
		}
		dst.LastEditedAt = max(dst.LastEditedAt, old.LastEditedAt)
		dst.LastViewedAt = max(dst.LastViewedAt, old.LastViewedAt)
		dst.LastActivityAt = max(dst.LastEditedAt, dst.LastViewedAt)
		dst.setMetadata(meta)
		err = couchdb.UpdateDoc(inst, &dst)
		if couchdb.IsConflictError(err) && attempt < maxWriteAttempts {
			continue
		}
		return err
	}
}
