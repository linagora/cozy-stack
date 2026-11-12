package recents

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
)

// maxWriteAttempts bounds the writes of a row that conflict with an activity
// recorded at the same time.
const maxWriteAttempts = 3

// RowsByIDs returns the rows that exist on the instance for the given items.
// An instance without the database has no row.
func RowsByIDs(inst *instance.Instance, ids []string) ([]*FileDirActivity, error) {
	var found []*FileDirActivity
	req := &couchdb.AllDocsRequest{Keys: ids}
	if err := couchdb.GetAllDocs(inst, consts.FilesRecents, req, &found); err != nil {
		if couchdb.IsNoDatabaseError(err) {
			return nil, nil
		}
		return nil, err
	}
	// The keys without a document give a null.
	rows := make([]*FileDirActivity, 0, len(found))
	for _, row := range found {
		if row != nil {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

// DeleteRow deletes a row, whatever has been written on it since it was read:
// when the row has changed, it deletes what is stored. A row that is gone is
// left as it is.
func DeleteRow(inst *instance.Instance, row *FileDirActivity) error {
	for attempt := 1; ; attempt++ {
		err := couchdb.DeleteDoc(inst, row)
		if err == nil || !couchdb.IsConflictError(err) || attempt == maxWriteAttempts {
			return ignoreNotFound(err)
		}
		fresh := &FileDirActivity{}
		if err := couchdb.GetDoc(inst, consts.FilesRecents, row.DocID, fresh); err != nil {
			return ignoreNotFound(err)
		}
		row = fresh
	}
}

func ignoreNotFound(err error) error {
	if couchdb.IsNotFoundError(err) {
		return nil
	}
	return err
}
