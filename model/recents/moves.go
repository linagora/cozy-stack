package recents

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// MoveRow moves a row to the new ID of its item, held by the instance of the
// given domain (empty when it is this instance): it writes the row under the
// new ID, then deletes the old row. The dates are merged to the max with those
// of a row already there, and the name, type, mime and class are those of the
// row with the latest activity. It is idempotent: a replay finds the dates
// already merged and the old row gone.
func MoveRow(inst *instance.Instance, old *FileDirActivity, newID, ownerDomain string) error {
	if err := upsertMovedRow(inst, old, newID, ownerDomain); err != nil {
		return err
	}
	return DeleteRow(inst, old)
}

func upsertMovedRow(inst *instance.Instance, old *FileDirActivity, newID, ownerDomain string) error {
	for attempt := 1; ; attempt++ {
		var dst FileDirActivity
		err := couchdb.GetDoc(inst, consts.FilesRecents, newID, &dst)
		exists := err == nil
		stored := dst // The row as read, for the realtime event of its update.
		if !exists {
			if !couchdb.IsNotFoundError(err) && !couchdb.IsNoDatabaseError(err) {
				return err
			}
			dst = FileDirActivity{DocID: newID}
		}
		if dst.LastActivityAt <= old.LastActivityAt {
			dst.Name, dst.Type, dst.Mime, dst.Class = old.Name, old.Type, old.Mime, old.Class
		}
		dst.OwnerDomain = ownerDomain
		dst.LastEditedAt = max(dst.LastEditedAt, old.LastEditedAt)
		dst.LastViewedAt = max(dst.LastViewedAt, old.LastViewedAt)
		dst.LastActivityAt = max(dst.LastEditedAt, dst.LastViewedAt)
		dst.SchemaVersion = SchemaVersion
		if exists {
			err = couchdb.UpdateDocWithOld(inst, &dst, &stored)
		} else {
			err = couchdb.CreateNamedDocWithDB(inst, &dst)
		}
		if (couchdb.IsConflictError(err) || couchdb.IsFileExists(err)) && attempt < maxWriteAttempts {
			continue
		}
		return err
	}
}
