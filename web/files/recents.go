package files

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// RecordActivity captures a personal activity on a file for the person behind
// the request. The row write is best-effort: a failed write is a lost activity
// (accepted for a recents list) and never fails the request.
func RecordActivity(c echo.Context, fileID string, kind recents.Kind) {
	user := ActorInstance(c)
	if user == nil || !recents.Enabled(user) {
		return
	}
	inst := middlewares.GetInstance(c)
	if err := recents.RecordActivity(user, inst, fileID, kind); err != nil {
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record %s activity on %s: %s", kind, fileID, err)
	}
}

// ActorInstance returns the instance of the person behind the request, as
// resolved by the actor middleware: the instance owner for a session-bound app
// token, the flagship app (certified, or with the "*" scope) or a desktop
// OAuth client, and the sharing member for a share-interact code or a preview
// code (a read-only opening), once the member has accepted the sharing. On the
// shared drive proxy, the person is checked on the original token of the
// recipient when the owner's instance is served directly on this stack; a
// request forwarded over HTTP only carries the drive token and counts as the
// recipient (ADR). It returns nil when the request is not a person (konnector,
// service token, other OAuth client, CLI, anonymous share by link, background
// worker), when the member has not accepted the sharing yet, or when the
// member's instance is not on this stack: those record no activity.
func ActorInstance(c echo.Context) *instance.Instance {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc == nil {
		return nil
	}
	if pdoc.Type == permission.TypeSharePreview {
		// The request actor of a preview is anonymous, but ParseJWT has found
		// the member to whom the code was given. Before the acceptance, the
		// instance of a member comes from the sharer's contacts.
		member := middlewares.GetShareMember(c)
		if member == nil || member.Status != sharing.MemberStatusReady ||
			member.InstanceHost() == "" {
			return nil
		}
		return middlewares.MemberInstance(middlewares.GetInstance(c), member.InstanceHost())
	}
	actor, ok := middlewares.GetActor(c)
	if !ok || !actor.Person {
		return nil
	}
	return actor.Instance
}

// backupDeviceIDsKey is the metadata the flagship app sets on the files and
// the device folder of a photo backup.
const backupDeviceIDsKey = "backupDeviceIds"

// isAutomaticCreation tells whether a creation is written by the flagship app
// on its own rather than by the person: the photo backup, the konnector
// destination folders, the konnector traces and the support logs. An upload
// the person makes from the app (OS share sheet) is a file creation like any
// other. created is nil for a folder: the flagship app creates folders only
// for its own automatic writes, a folder the person creates goes through the
// Drive webview session.
func isAutomaticCreation(c echo.Context, created *vfs.FileDoc) bool {
	if created != nil {
		if _, ok := created.Metadata[backupDeviceIDsKey]; ok {
			return true
		}
	}
	if !isFlagshipClient(c) {
		return false
	}
	if created == nil {
		return true
	}
	fpath, err := created.Path(middlewares.GetInstance(c).VFS())
	if err != nil {
		return false
	}
	return strings.HasPrefix(fpath, "/Settings/Logs/") ||
		strings.HasPrefix(fpath, "/Settings/AALogs/")
}

// isFlagshipClient tells whether the request is made with an OAuth token of
// the flagship app: a certified client, or the "*" scope given to the flagship
// app when its context skips the certification.
func isFlagshipClient(c echo.Context) bool {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc == nil || pdoc.Type != permission.TypeOauth {
		return false
	}
	if client, ok := pdoc.Client.(*oauth.Client); ok && client.Flagship {
		return true
	}
	claims, ok := c.Get("claims").(permission.Claims)
	return ok && claims.Scope == "*"
}

// defaultRecentsPageSize is the page size when the request asks for none.
const defaultRecentsPageSize = 50

// maxRecentsCandidates bounds the work of one traversal: at most that many
// candidate rows are examined to fill a page. Skipped candidates make the
// page shorter when the bound bites, and meta.incomplete then says so.
const maxRecentsCandidates = 200

// recentsItem is the JSON:API resource of a recents entry: the current light
// metadata of its source item. The owner's paths and parent folders are never
// exposed.
type recentsItem struct {
	itemID string

	FileID         string    `json:"file_id"`
	Name           string    `json:"name"`
	Type           string    `json:"type"`
	Size           int64     `json:"size,string"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	SourceInstance string    `json:"source_instance,omitempty"`
}

func (r *recentsItem) ID() string                             { return r.itemID }
func (r *recentsItem) Rev() string                            { return "" }
func (r *recentsItem) DocType() string                        { return consts.FilesRecents }
func (r *recentsItem) SetID(id string)                        { r.itemID = id }
func (r *recentsItem) SetRev(_ string)                        {}
func (r *recentsItem) Clone() couchdb.Doc                     { cloned := *r; return &cloned }
func (r *recentsItem) Relationships() jsonapi.RelationshipMap { return nil }
func (r *recentsItem) Included() []jsonapi.Object             { return nil }
func (r *recentsItem) Links() *jsonapi.LinksList              { return nil }

// encodeRecentsCursor makes the pagination cursor opaque for the clients. It
// is bound to the user by living in their recents database.
func encodeRecentsCursor(cur recents.Cursor) (string, error) {
	b, err := json.Marshal(cur)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// decodeRecentsCursor is the inverse of encodeRecentsCursor.
func decodeRecentsCursor(raw string) (recents.Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return recents.Cursor{}, err
	}
	var cur recents.Cursor
	if err := json.Unmarshal(b, &cur); err != nil {
		return recents.Cursor{}, err
	}
	if cur.LastActivityAt == "" || cur.ID == "" {
		return recents.Cursor{}, errors.New("incomplete cursor")
	}
	return cur, nil
}

// ReadRecentsHandler handles GET /files/recents: the personal recents of the
// user behind the request, as a paginated JSON:API list. Every candidate is
// re-checked against its source instance: trashed, destroyed and unauthorized
// items are skipped, and a page is refilled within a fixed work limit.
func ReadRecentsHandler(c echo.Context) error {
	if !recents.Enabled(middlewares.GetInstance(c)) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}

	size := defaultRecentsPageSize
	if raw := c.QueryParam("page[limit]"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return jsonapi.InvalidParameter("page[limit]", errors.New("invalid page limit"))
		}
		size = n
	}
	if size > recents.MaxPageSize {
		size = recents.MaxPageSize
	}

	var cursor recents.Cursor
	if raw := c.QueryParam("cursor"); raw != "" {
		decoded, err := decodeRecentsCursor(raw)
		if err != nil {
			return jsonapi.InvalidParameter("cursor", err)
		}
		cursor = decoded
	}

	user := ActorInstance(c)
	if user == nil {
		pdoc, err := middlewares.GetPermission(c)
		if err != nil {
			return err
		}
		if pdoc.Type == permission.TypeShareInteract {
			// A sharing member has no personal recents on the instance they
			// act on: never fall back to the owner's list.
			return middlewares.ErrForbidden
		}
		user = middlewares.GetInstance(c)
	}

	// Lazy seeding on the first page of a traversal: the marker makes it a
	// no-op afterwards.
	if cursor.IsZero() {
		if err := recents.Seed(user); err != nil {
			user.Logger().WithNamespace("recents").Warnf("Cannot seed the recents: %s", err)
		}
	}

	after, exact, err := recents.ResolveCursor(user, cursor)
	if err != nil {
		return err
	}
	page, err := readRecentsPage(user, size, after)
	if err != nil {
		return err
	}
	// The row of the cursor could not be found: rows of its date may be
	// missed.
	incomplete := page.incomplete || !exact

	var links *jsonapi.LinksList
	if page.hasMore {
		params := url.Values{"page[limit]": {strconv.Itoa(size)}}
		// Without a position, the next page starts the traversal again: all
		// the rows examined by this one, up to its end, are deleted.
		if !page.next.IsZero() {
			encoded, err := encodeRecentsCursor(recents.CursorFor(page.next))
			if err != nil {
				return err
			}
			params.Set("cursor", encoded)
		}
		links = &jsonapi.LinksList{Next: "/files/recents?" + params.Encode()}
	}
	meta := jsonapi.Meta{}
	if incomplete {
		meta.Incomplete = &incomplete
	}
	return jsonapi.DataListWithMeta(c, http.StatusOK, meta, page.items, links)
}

// recentsPage is the result of the traversal of the recents index for a page.
type recentsPage struct {
	items []jsonapi.Object
	// next is the position where the next page resumes, when hasMore is
	// true. A zero position starts the traversal again.
	next       recents.Position
	hasMore    bool
	incomplete bool
}

// readRecentsPage walks the recents index and keeps the first size readable
// items. The position of the last kept item is returned when more rows follow:
// the skipped ones are re-examined on the next page. One extra kept item acts
// as the proof that more rows follow. A cursor must point to a row that is
// still there: when the last kept item comes from a migrated row, the position
// is the last examined row before it that is not deleted, and the item may
// come back on the next page under the same public ID.
//
// The rows are read and checked by batches: the first one has the size of the
// page plus the proof, as most rows are kept, and each next one is twice the
// previous, as a batch that did not fill the page has skipped many rows. The
// repairs of the examined rows are written in bulk at the end.
//
// Known limit: when all the rows examined up to the work limit are deleted,
// the position does not move, and the deletions make the next page go further.
// If writing them fails, the next page examines the same rows again and comes
// back empty, until a write succeeds.
func readRecentsPage(user *instance.Instance, size int, after recents.Position) (*recentsPage, error) {
	r := newRecentsReader(user)
	defer r.repairs.Apply(user)

	page := &recentsPage{items: make([]jsonapi.Object, 0, size)}
	pageEnd := after // position where the page ends, up to its last item
	resume := after  // position of the last examined row that is not deleted
	examined := 0
	batch := size + 1

	for len(page.items) <= size {
		// One more row than the work limit allows tells whether rows are left
		// after the last examined one.
		limit := min(batch, maxRecentsCandidates-examined+1)
		rows, err := recents.Candidates(user, limit, after)
		if err != nil {
			return nil, err
		}
		tail := len(rows) < limit // the batch holds the tail of the index
		results := r.resolve(rows[:min(len(rows), maxRecentsCandidates-examined)])

		for i := range rows {
			if len(page.items) > size {
				break // rows[i:] is left for the next page
			}
			if examined >= maxRecentsCandidates {
				// The work limit is reached while rows remain: the page may
				// miss items. The traversal resumes after the last examined
				// row that is still there, so that its cursor is exact.
				page.next, page.hasMore, page.incomplete = resume, true, true
				return page, nil
			}
			examined++
			row := &rows[i]
			after = recents.Position{LastActivityAt: row.LastActivityAt, DocID: row.DocID}
			item, broken, deleted := r.examine(row, &results[i])
			if !deleted {
				resume = after
			}
			page.incomplete = page.incomplete || broken
			if item != nil {
				page.items = append(page.items, item)
				if len(page.items) <= size {
					pageEnd = resume
				}
			}
		}
		if tail {
			break // the index is exhausted
		}
		batch *= 2
	}

	if len(page.items) > size {
		// The page is full: its last item ends the traversal point.
		page.items = page.items[:size]
		page.next, page.hasMore = pageEnd, true
	}
	return page, nil
}

// recentsResolution is what the check of a row has found.
type recentsResolution struct {
	// key is the key of the row once repaired: the key under the successor
	// for a migrated row. A key is shown once per page.
	key string
	// item is the item to show, nil when the row is hidden.
	item *recentsItem
	// broken is true when the row could not be checked: the page may miss
	// an item.
	broken bool
	// remove is true for a row that can never be shown again.
	remove bool
	// locator is the source locator to write on a row whose locator is not
	// the current one.
	locator string
	// migrate is set for a row of a moved source.
	migrate *recentsMigration
	// duplicate is true for a migrated row whose new row comes first in the
	// traversal: the item is shown there.
	duplicate bool
}

// recentsMigration is the move of a row to the key under the successor of its
// source.
type recentsMigration struct {
	to       *instance.Instance
	fileID   string
	existing *recents.FileDirActivity
}

// recentsReader checks the rows of a traversal, with the sources and the
// repairs shared by its batches.
type recentsReader struct {
	user    *instance.Instance
	sources *recents.Sources
	repairs recents.Repairs
	seen    map[string]bool
}

func newRecentsReader(user *instance.Instance) *recentsReader {
	return &recentsReader{
		user:    user,
		sources: recents.NewSources(user),
		seen:    make(map[string]bool),
	}
}

// examine applies the resolution of an examined row: it records its repair,
// and returns the item to show, if any. deleted is true when the repair
// deletes the row.
func (r *recentsReader) examine(row *recents.FileDirActivity, res *recentsResolution) (jsonapi.Object, bool, bool) {
	deleted := false
	switch {
	case res.remove:
		r.repairs.Delete(row)
		return nil, false, true
	case res.migrate != nil:
		r.repairs.Migrate(row, res.migrate.existing, res.migrate.to, res.migrate.fileID)
		deleted = true
	case res.locator != "":
		r.repairs.Refresh(row, res.locator)
	}
	if res.duplicate || r.seen[res.key] {
		return nil, false, deleted
	}
	r.seen[res.key] = true
	if res.item == nil {
		return nil, res.broken, deleted
	}
	return res.item, false, deleted
}

// resolve checks a batch of rows: the current metadata of their items, read
// from the database of the instance that holds them, and the access of the
// user on the items of the other instances. It costs one request for the
// unknown source instances, then per source instance one request for the items
// and, for the other instances, at most three for the access. A moved or
// deleted source adds the lookups of its successor, up to maxMoves moves, and
// the rows migrated to a successor one request for the rows already stored
// under their new keys. A row that can never be shown again is removed; the
// other hidden rows are kept as they may come back.
func (r *recentsReader) resolve(rows []recents.FileDirActivity) []recentsResolution {
	results := make([]recentsResolution, len(rows))
	r.sources.Resolve(rows)

	groups := make(map[string][]int) // row indexes by holding instance
	holders := make(map[string]*instance.Instance)
	fileIDs := make([]string, len(rows))
	var migrated []string
	for i := range rows {
		row := &rows[i]
		res := &results[i]
		res.key = row.DocID
		instID, fileID, ok := recents.ParseResourceKey(row.DocID)
		if !ok {
			res.remove = true
			continue
		}
		src := r.sources.Get(instID)
		switch src.State {
		case recents.SourceGone:
			res.remove = true
			continue
		case recents.SourceUnavailable:
			res.broken = true
			continue
		case recents.SourceMoved:
			res.key = recents.ResourceKey(src.Instance, fileID)
			res.migrate = &recentsMigration{to: src.Instance, fileID: fileID}
			migrated = append(migrated, res.key)
		default:
			if locator := recents.SourceLocator(src.Instance, fileID); row.SourceLocator != locator {
				res.locator = locator
			}
		}
		fileIDs[i] = fileID
		holderID := src.Instance.DocID
		holders[holderID] = src.Instance
		groups[holderID] = append(groups[holderID], i)
	}

	r.findMigratedRows(rows, results, migrated)
	for holderID, indexes := range groups {
		r.resolveHolder(holders[holderID], rows, results, fileIDs, indexes)
	}
	return results
}

// findMigratedRows loads the rows already stored under the keys of the
// migrated rows, to merge their dates. A migrated row whose new row comes
// first in the traversal is a duplicate: the item is shown at the place of the
// new row.
func (r *recentsReader) findMigratedRows(rows []recents.FileDirActivity, results []recentsResolution, keys []string) {
	if len(keys) == 0 {
		return
	}
	existing, err := recents.GetRows(r.user, keys)
	if err != nil {
		r.user.Logger().WithNamespace("recents").
			Warnf("Cannot read the migrated recents rows: %s", err)
	}
	for i := range results {
		res := &results[i]
		if res.migrate == nil {
			continue
		}
		if err != nil {
			// Migrating without the new row could lose its dates: the row is
			// kept as it is, and hidden.
			res.migrate = nil
			res.broken = true
			res.key = rows[i].DocID
			continue
		}
		next := existing[res.key]
		if next == nil {
			continue
		}
		res.migrate.existing = next
		row := &rows[i]
		res.duplicate = next.LastActivityAt > row.LastActivityAt ||
			(next.LastActivityAt == row.LastActivityAt && next.DocID > row.DocID)
	}
}

// resolveHolder checks the rows whose items are held by an instance.
func (r *recentsReader) resolveHolder(holder *instance.Instance, rows []recents.FileDirActivity, results []recentsResolution, fileIDs []string, indexes []int) {
	ids := make([]string, len(indexes))
	for j, i := range indexes {
		ids[j] = fileIDs[i]
	}
	docs, err := vfs.DirOrFileDocsByIDs(holder, ids)
	if err != nil {
		for _, i := range indexes {
			results[i].broken = true
		}
		return
	}

	own := holder.DocID == r.user.DocID
	var targets []sharing.AccessTarget
	var targetIndexes []int
	for _, i := range indexes {
		res := &results[i]
		if res.broken {
			continue
		}
		doc, ok := docs[fileIDs[i]]
		if !ok {
			// The item is destroyed, or is missing from the successor of a
			// moved source: the row is not migrated.
			res.migrate = nil
			res.remove = true
			continue
		}
		dir, file := doc.Refine()
		switch {
		case dir == nil && file == nil:
			continue // neither a file nor a folder
		case file != nil && file.Trashed:
			continue // a restored item comes back with its row
		case file == nil && strings.HasPrefix(dir.Fullpath, vfs.TrashDirName):
			continue
		}
		res.item = newRecentsItem(res.key, dir, file)
		if !own {
			res.item.SourceInstance = holder.Domain
			targets = append(targets, sharing.AccessTarget{Dir: dir, File: file})
			targetIndexes = append(targetIndexes, i)
		}
	}
	if len(targets) == 0 {
		return
	}

	// The items are owned by another instance of the stack: the owner applies
	// its own effective-access rules for the user.
	access, err := sharing.NewAccessResolver(holder).ResolveDocsForInstance(targets, r.user)
	for j, i := range targetIndexes {
		res := &results[i]
		switch {
		case err != nil || access[j].Err != nil:
			res.item = nil
			res.broken = true
		case !access[j].Access.CanRead:
			res.item = nil // losing access hides the row, it is kept
		}
	}
}

// newRecentsItem returns the resource of an item, under the public ID of its
// row.
func newRecentsItem(key string, dir *vfs.DirDoc, file *vfs.FileDoc) *recentsItem {
	item := &recentsItem{itemID: recents.PublicID(key)}
	if file != nil {
		item.FileID = file.DocID
		item.Name = file.DocName
		item.Type = consts.FileType
		item.Size = file.ByteSize
		item.CreatedAt = file.CreatedAt
		item.UpdatedAt = file.UpdatedAt
	} else {
		item.FileID = dir.DocID
		item.Name = dir.DocName
		item.Type = consts.DirType
		item.CreatedAt = dir.CreatedAt
		item.UpdatedAt = dir.UpdatedAt
	}
	return item
}
