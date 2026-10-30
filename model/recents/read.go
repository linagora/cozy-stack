package recents

import (
	"strings"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// Position is a place in the traversal of the recents index: the sort key and
// the _id of a row. It never leaves the stack, as the _id holds the internal
// ID of the source instance.
type Position struct {
	LastActivityAt string
	DocID          string
}

// IsZero returns true when the position is the start of a traversal.
func (p Position) IsZero() bool { return p.LastActivityAt == "" && p.DocID == "" }

// Cursor is the opaque pagination cursor of a recents traversal, given to the
// clients: the sort key and the public ID of the last returned row. A refresh
// starts a new traversal (an empty cursor), and the cursor is bound to the user
// by living in their recents database.
type Cursor struct {
	LastActivityAt string `json:"last_activity_at"`
	ID             string `json:"id"`
}

// IsZero returns true when the cursor is the start of a traversal.
func (c Cursor) IsZero() bool { return c.LastActivityAt == "" && c.ID == "" }

// CursorFor returns the cursor of a position, for the clients.
func CursorFor(p Position) Cursor {
	return Cursor{LastActivityAt: p.LastActivityAt, ID: PublicID(p.DocID)}
}

// maxCursorLookup bounds the rows read to find the row of a cursor among the
// rows sharing its date (many files synchronized in the same second).
const maxCursorLookup = 200

// ResolveCursor finds the position of a cursor: the row of its date whose
// public ID matches. When that row is not found (updated or deleted since, or
// beyond the lookup bound), the traversal resumes before the date of the
// cursor and exact is false: same-date rows may then be skipped.
func ResolveCursor(inst prefixer.Prefixer, cur Cursor) (pos Position, exact bool, err error) {
	if cur.IsZero() {
		return Position{}, true, nil
	}
	req := &couchdb.FindRequest{
		UseIndex: "by-last-activity-and-id",
		Selector: mango.And(
			mango.Equal("last_activity_at", cur.LastActivityAt),
			mango.Gt("_id", nil),
		),
		Sort: mango.SortBy{
			{Field: "last_activity_at", Direction: mango.Desc},
			{Field: "_id", Direction: mango.Desc},
		},
		Limit:  maxCursorLookup,
		Fields: []string{"_id"},
	}
	var docs []FileDirActivity
	err = couchdb.FindDocs(inst, consts.FilesRecents, req, &docs)
	if err != nil && !couchdb.IsNoDatabaseError(err) {
		return Position{}, false, err
	}
	for _, doc := range docs {
		if PublicID(doc.DocID) == cur.ID {
			return Position{LastActivityAt: cur.LastActivityAt, DocID: doc.DocID}, true, nil
		}
	}
	// No _id sorts before "": the traversal resumes strictly before the date.
	return Position{LastActivityAt: cur.LastActivityAt}, false, nil
}

// Candidates returns at most one more row than asked, from the index: the rows
// strictly after the position, sorted by last_activity_at desc with _id desc as
// tie-breaker. The extra row lets the caller know if more rows follow, so size
// is not capped here: the caller bounds it.
func Candidates(inst prefixer.Prefixer, size int, after Position) ([]FileDirActivity, error) {
	selector := mango.And(
		mango.Gt("last_activity_at", nil),
		mango.Gt("_id", nil),
	)
	if !after.IsZero() {
		selector = mango.And(
			selector,
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
		UseIndex: "by-last-activity-and-id",
		Selector: selector,
		Sort: mango.SortBy{
			{Field: "last_activity_at", Direction: mango.Desc},
			{Field: "_id", Direction: mango.Desc},
		},
		Limit:  size + 1,
		Fields: []string{"_id", "_rev", "source_locator", "last_activity_at"},
	}
	var docs []FileDirActivity
	err := couchdb.FindDocs(inst, consts.FilesRecents, req, &docs)
	if couchdb.IsNoDatabaseError(err) {
		// The database is created by the first recorded activity.
		return nil, nil
	}
	return docs, err
}

// MaxPageSize is the maximum page size of the endpoint.
const MaxPageSize = 100

// ParseSourceLocator splits a source_locator (domain/fileID) into its domain
// and file ID. It returns ok=false for a malformed locator.
func ParseSourceLocator(locator string) (domain, fileID string, ok bool) {
	domain, fileID, found := strings.Cut(locator, "/")
	if !found || domain == "" || fileID == "" {
		return "", "", false
	}
	return domain, fileID, true
}

// GetInstance loads the source instance of a row from its source_locator.
// It returns nil when the instance is not on this stack or is gone.
func GetInstance(locator string) *instance.Instance {
	domain, _, ok := ParseSourceLocator(locator)
	if !ok {
		return nil
	}
	inst, _ := instance.Get(domain)
	return inst
}
