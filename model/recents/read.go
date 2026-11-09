package recents

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

const (
	// DefaultPageSize is the page size of the recents when a request asks for
	// none.
	DefaultPageSize = 50
	// MaxPageSize is the largest page size of the recents.
	MaxPageSize = 100
)

// sortIndex is the name of the index that sorts the rows.
const sortIndex = "by-last-activity-and-id"

// Position is a place in the traversal of the rows, that the pagination cursor
// carries. It needs no row of its own: a row whose date has changed since it
// was read is moved ahead of the traversal, which goes on after the position.
type Position struct {
	LastActivityAt string `json:"last_activity_at"`
	DocID          string `json:"id"`
}

// PositionOf returns the position of a row.
func PositionOf(row *FileDirActivity) Position {
	return Position{LastActivityAt: row.LastActivityAt, DocID: row.DocID}
}

// IsZero returns true for the position before the first row.
func (p Position) IsZero() bool { return p == Position{} }

// Cursor encodes the position as a pagination cursor. A cursor only has a
// meaning for the rows of the database where it is used, and it gives access
// to nothing else.
func (p Position) Cursor() (string, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ParseCursor is the inverse of Position.Cursor. It returns an error for a
// cursor that does not hold a complete position.
func ParseCursor(cursor string) (Position, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return Position{}, err
	}
	var pos Position
	if err := json.Unmarshal(b, &pos); err != nil {
		return Position{}, err
	}
	if pos.DocID == "" {
		return Position{}, errors.New("the cursor has no ID")
	}
	if _, err := time.Parse(time.RFC3339, pos.LastActivityAt); err != nil {
		return Position{}, errors.New("the cursor has no valid date")
	}
	return pos, nil
}

// Candidates returns at most limit rows strictly after the position, sorted by
// last_activity_at descending then _id descending. A missing database has no
// rows.
func Candidates(db prefixer.Prefixer, limit int, after Position) ([]FileDirActivity, error) {
	selector := mango.And(
		mango.Gt("last_activity_at", nil),
		mango.Gt("_id", nil),
	)
	if !after.IsZero() {
		selector = mango.And(
			selector,
			// The bound of the index range: Mango cannot start the scan from
			// the $or below, it only filters the rows with it.
			mango.Lte("last_activity_at", after.LastActivityAt),
			mango.Or(
				mango.Lt("last_activity_at", after.LastActivityAt),
				mango.And(
					mango.Equal("last_activity_at", after.LastActivityAt),
					mango.Lt("_id", after.DocID),
				),
			),
		)
	}
	req := &couchdb.FindRequest{
		UseIndex: sortIndex,
		Selector: selector,
		Sort: mango.SortBy{
			{Field: "last_activity_at", Direction: mango.Desc},
			{Field: "_id", Direction: mango.Desc},
		},
		Limit: limit,
	}
	var rows []FileDirActivity
	err := couchdb.FindDocs(db, consts.FilesRecents, req, &rows)
	if couchdb.IsNoDatabaseError(err) {
		return nil, nil
	}
	return rows, err
}
