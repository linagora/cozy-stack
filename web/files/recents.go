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
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
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

// ActorInstance returns the instance of the person behind the request: the
// instance owner for a session-bound app token or a desktop/flagship OAuth
// client, the sharing member for a share-interact token or for the preview
// code of a member (a read-only opening). It returns nil when the request is
// not a person (konnector, service token, CLI, anonymous share by link,
// background worker): those record no activity. Unsupported write paths (ADR),
// never any activity: openings of mobile OAuth apps, other app tokens and
// konnectors.
func ActorInstance(c echo.Context) *instance.Instance {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil || pdoc == nil {
		return nil
	}
	inst := middlewares.GetInstance(c)

	switch pdoc.Type {
	case permission.TypeShareInteract:
		actor, ok := middlewares.GetActor(c)
		if !ok || actor == nil || actor.Domain == "" {
			return nil
		}
		return memberInstance(inst, actor.Domain)

	case permission.TypeSharePreview:
		// The request actor of a preview is anonymous, but a preview code is
		// minted per member: it tells which member opens the file read-only.
		member := previewMember(c, inst, pdoc)
		if member == nil || member.InstanceHost() == "" {
			return nil
		}
		return memberInstance(inst, member.InstanceHost())

	case permission.TypeWebapp:
		// A session-bound app token is the user in their browser; an app token
		// with no session is a service token, not a person.
		if claims, ok := c.Get("claims").(permission.Claims); ok && claims.SessionID != "" {
			return inst
		}
		return nil

	case permission.TypeOauth:
		// The flagship app and cozy-desktop act for their user.
		client, ok := pdoc.Client.(*oauth.Client)
		if ok && (client.Flagship || client.ClientKind == "desktop") {
			return inst
		}
		return nil

	default:
		return nil
	}
}

// memberInstance returns the instance of a sharing member by its domain.
func memberInstance(inst *instance.Instance, domain string) *instance.Instance {
	user, err := lifecycle.GetInstance(domain)
	if err != nil {
		// The member's instance is not on this stack: the activity is lost
		// (ADR scope). Log it, but never fail the request.
		inst.Logger().WithNamespace("recents").
			Warnf("Cannot record activity of %s: instance not found", domain)
		return nil
	}
	return user
}

// previewMember returns the sharing member to whom the preview code of the
// request was given, or nil for a code that is not a member's.
func previewMember(c echo.Context, inst *instance.Instance, pdoc *permission.Permission) *sharing.Member {
	token, err := middlewares.TransformShortcodeToJWT(inst, middlewares.GetRequestToken(c))
	if err != nil || token == "" {
		return nil
	}
	_, sharingID, ok := strings.Cut(pdoc.SourceID, "/")
	if !ok {
		return nil
	}
	s, err := sharing.FindSharing(inst, sharingID)
	if err != nil {
		return nil
	}
	member, err := s.FindMemberBySharecode(inst, token)
	if err != nil {
		return nil
	}
	return member
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

	// Lazy seeding on the first read: the marker makes it a no-op afterwards.
	if err := recents.Seed(user); err != nil {
		user.Logger().WithNamespace("recents").Warnf("Cannot seed the recents: %s", err)
	}

	after, exact, err := recents.ResolveCursor(user, cursor)
	if err != nil {
		return err
	}
	items, next, hasMore, incomplete, err := readRecentsPage(user, size, after)
	if err != nil {
		return err
	}
	// The row of the cursor could not be found: rows of its date may be
	// missed.
	incomplete = incomplete || !exact

	var links *jsonapi.LinksList
	if hasMore {
		encoded, err := encodeRecentsCursor(recents.CursorFor(next))
		if err != nil {
			return err
		}
		params := url.Values{
			"cursor":      {encoded},
			"page[limit]": {strconv.Itoa(size)},
		}
		links = &jsonapi.LinksList{Next: "/files/recents?" + params.Encode()}
	}
	meta := jsonapi.Meta{}
	if incomplete {
		meta.Incomplete = &incomplete
	}
	return jsonapi.DataListWithMeta(c, http.StatusOK, meta, items, links)
}

// readRecentsPage walks the recents index and keeps the first size readable
// items. The position of the last kept item is returned when more rows follow:
// the skipped ones are re-examined on the next page. One extra kept item acts
// as the proof that more rows follow.
func readRecentsPage(user *instance.Instance, size int, after recents.Position) ([]jsonapi.Object, recents.Position, bool, bool, error) {
	items := make([]jsonapi.Object, 0, size)
	pageEnd := after // position of the last item kept in the page
	examined := 0
	incomplete := false

	for len(items) <= size {
		want := size + 1 - len(items)
		rows, err := recents.Candidates(user, want, after)
		if err != nil {
			return nil, recents.Position{}, false, false, err
		}
		tail := len(rows) <= want // the batch holds the tail of the index

		stop := false
		for i := range rows {
			if len(items) > size || examined >= maxRecentsCandidates {
				stop = true // rows[i:] is left for the next page
				break
			}
			examined++
			row := &rows[i]
			after = recents.Position{LastActivityAt: row.LastActivityAt, DocID: row.DocID}
			item, keep, broken := resolveRecentsItem(user, row)
			incomplete = incomplete || broken
			if keep {
				items = append(items, item)
				if len(items) <= size {
					pageEnd = after
				}
			}
		}

		if stop {
			if len(items) > size {
				// The page is full: its last item ends the traversal point.
				return items[:size], pageEnd, true, incomplete, nil
			}
			// The work limit is reached while rows remain: the page may miss
			// items, and the traversal resumes after the last examined row.
			return items, after, true, true, nil
		}
		if tail {
			break // the index is exhausted
		}
	}

	if len(items) > size {
		return items[:size], pageEnd, true, incomplete, nil
	}
	return items, recents.Position{}, false, incomplete, nil
}

// resolveRecentsItem fetches the current metadata of a candidate and checks
// that the user can still read it. keep is false for trashed, destroyed or
// unauthorized items; broken is true when the item could not be checked and
// the page may therefore miss items. A row that can never be shown again is
// deleted; the other hidden rows are kept as they may come back.
func resolveRecentsItem(user *instance.Instance, row *recents.FileDirActivity) (jsonapi.Object, bool, bool) {
	_, fileID, ok := recents.ParseSourceLocator(row.SourceLocator)
	if !ok {
		repairRecentsRow(user, row)
		return nil, false, false
	}
	src := recents.GetInstance(row.SourceLocator)
	if src == nil {
		// The owner is gone or on another stack: its access rules cannot be
		// applied, so the item is hidden and the page may be incomplete.
		return nil, false, true
	}

	var doc vfs.DirOrFileDoc
	err := couchdb.GetDoc(src, consts.Files, fileID, &doc)
	if couchdb.IsNotFoundError(err) {
		repairRecentsRow(user, row) // the source item is destroyed
		return nil, false, false
	}
	if err != nil {
		return nil, false, true
	}
	dir, file := doc.Refine()

	if file != nil {
		if file.Trashed {
			return nil, false, false // a restored item comes back with its row
		}
	} else if strings.HasPrefix(dir.Fullpath, vfs.TrashDirName) {
		return nil, false, false
	}

	if src.ID() != user.ID() {
		// The item is owned by another instance of the stack: the owner
		// applies its own effective-access rules for the user.
		member := &sharing.Member{Instance: "https://" + user.Domain}
		if email, err := user.SettingsEMail(); err == nil {
			member.Email = email
		}
		ea, err := sharing.NewAccessResolver(src).ResolveForMember(fileID, member)
		if err != nil {
			return nil, false, true
		}
		if !ea.CanRead {
			return nil, false, false // losing access hides the row, it is kept
		}
	}

	item := &recentsItem{itemID: recents.PublicID(row.DocID)}
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
	if src.ID() != user.ID() {
		item.SourceInstance = src.Domain
	}
	return item, true, false
}

// repairRecentsRow deletes a row that can never be shown again. Best effort:
// a failed delete leaves the row hidden and it is retried on the next
// traversal.
func repairRecentsRow(user *instance.Instance, row *recents.FileDirActivity) {
	if err := couchdb.DeleteDoc(user, row); err != nil {
		user.Logger().WithNamespace("recents").
			Warnf("Cannot repair the recents row %s: %s", row.DocID, err)
	}
}
