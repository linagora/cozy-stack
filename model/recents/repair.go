package recents

import (
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// GetRows loads the rows with the given keys in one request, indexed by key.
// Missing rows are absent from the map.
func GetRows(inst prefixer.Prefixer, keys []string) (map[string]*FileDirActivity, error) {
	rows := make(map[string]*FileDirActivity, len(keys))
	if len(keys) == 0 {
		return rows, nil
	}
	var docs []*FileDirActivity
	req := &couchdb.AllDocsRequest{Keys: keys}
	err := couchdb.GetAllDocs(inst, consts.FilesRecents, req, &docs)
	if couchdb.IsNoDatabaseError(err) {
		return rows, nil
	}
	if err != nil {
		return nil, err
	}
	for _, doc := range docs {
		if doc != nil {
			rows[doc.DocID] = doc
		}
	}
	return rows, nil
}

// Repairs collects the changes of rows decided by a traversal, to write them
// in bulk at its end.
type Repairs struct {
	deletes    []*FileDirActivity
	refreshes  []*FileDirActivity
	migrations []migration
}

// migration is a row of a moved source rewritten under the key of its
// successor. to is nil when the row of the successor already holds its dates.
type migration struct {
	from *FileDirActivity
	to   *FileDirActivity
}

// deletedRow is the deletion of a row in a bulk write.
type deletedRow struct {
	ID      string `json:"_id"`
	Rev     string `json:"_rev"`
	Deleted bool   `json:"_deleted"`
}

func deletionOf(row *FileDirActivity) deletedRow {
	return deletedRow{ID: row.DocID, Rev: row.DocRev, Deleted: true}
}

// Delete deletes a row that can never be shown again.
func (r *Repairs) Delete(row *FileDirActivity) {
	r.deletes = append(r.deletes, row)
}

// Refresh rewrites the source locator of a row. Its key does not change.
func (r *Repairs) Refresh(row *FileDirActivity, locator string) {
	refreshed := *row
	refreshed.SourceLocator = locator
	r.refreshes = append(r.refreshes, &refreshed)
}

// Migrate rewrites a row of a moved source under the key of its successor,
// to which the file IDs are kept, and deletes it. existing is the row already
// stored under that key, if any: the dates of both rows are merged with their
// maximum, so that no activity is lost.
func (r *Repairs) Migrate(row, existing *FileDirActivity, to *instance.Instance, fileID string) {
	key, locator := rowKey(to, fileID)
	if existing == nil {
		r.migrations = append(r.migrations, migration{from: row, to: &FileDirActivity{
			DocID:          key,
			SourceLocator:  locator,
			LastEditedAt:   row.LastEditedAt,
			LastViewedAt:   row.LastViewedAt,
			LastActivityAt: row.LastActivityAt,
			SchemaVersion:  SchemaVersion,
		}})
		return
	}
	merged := *existing
	changed := mergeRow(&merged, row)
	if merged.SourceLocator != locator {
		merged.SourceLocator = locator
		changed = true
	}
	m := migration{from: row}
	if changed {
		m.to = &merged
	}
	r.migrations = append(r.migrations, m)
}

// mergeRow applies the dates of src to dst with a max per component, then
// recomputes the sort key, as mergeActivity does. It reports whether dst
// changed.
func mergeRow(dst, src *FileDirActivity) bool {
	changed := false
	if src.LastEditedAt > dst.LastEditedAt {
		dst.LastEditedAt = src.LastEditedAt
		changed = true
	}
	if src.LastViewedAt > dst.LastViewedAt {
		dst.LastViewedAt = src.LastViewedAt
		changed = true
	}
	dst.LastActivityAt = max(dst.LastEditedAt, dst.LastViewedAt)
	return changed
}

// Apply writes the repairs in at most two requests: the deleted, refreshed and
// migrated rows first, then the deletion of the migrated rows whose new row is
// written, so that a failed write never loses the dates of a row. Best effort:
// a failed repair is logged, and retried by a later traversal. A conflict or
// a row already deleted is not logged, as a concurrent activity or traversal
// has changed the row.
func (r *Repairs) Apply(user *instance.Instance) {
	docs := make([]interface{}, 0, len(r.deletes)+len(r.refreshes)+len(r.migrations))
	for _, row := range r.deletes {
		docs = append(docs, deletionOf(row))
	}
	// A row that is also the target of a migration gets its locator from the
	// merge: writing the refresh too would make one of both writes conflict.
	targets := make(map[string]bool)
	for _, m := range r.migrations {
		if m.to != nil {
			targets[m.to.DocID] = true
		}
	}
	for _, row := range r.refreshes {
		if !targets[row.DocID] {
			docs = append(docs, row)
		}
	}
	firstMigration := len(docs)
	for _, m := range r.migrations {
		if m.to != nil {
			docs = append(docs, m.to)
		}
	}
	res, ok := writeRows(user, docs)
	if !ok {
		return
	}

	var deletes []interface{}
	i := firstMigration
	for _, m := range r.migrations {
		if m.to != nil {
			written := res[i].Ok
			i++
			if !written {
				continue
			}
		}
		deletes = append(deletes, deletionOf(m.from))
	}
	writeRows(user, deletes)
}

// writeRows writes rows in bulk, and logs the failures.
func writeRows(user *instance.Instance, docs []interface{}) ([]couchdb.UpdateResponse, bool) {
	if len(docs) == 0 {
		return nil, true
	}
	log := user.Logger().WithNamespace("recents")
	res, err := couchdb.BulkWriteDocs(user, consts.FilesRecents, docs)
	if err != nil {
		log.Warnf("Cannot repair the recents rows: %s", err)
		return nil, false
	}
	for _, r := range res {
		if r.Ok || r.Error == "conflict" || r.Error == "not_found" {
			continue
		}
		log.Warnf("Cannot repair the recents row %s: %s %s", r.ID, r.Error, r.Reason)
	}
	return res, true
}
