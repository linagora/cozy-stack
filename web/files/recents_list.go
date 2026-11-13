package files

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

const (
	recentsLimitParam  = "page[limit]"
	recentsCursorParam = "page[cursor]"
)

// recentsResource is the JSON:API resource of a recents row. Its attributes
// are all the stored fields of the row, without the version of its format,
// and it points to the item of the row with its `file` relationship.
type recentsResource struct {
	*recents.FileDirActivity

	// SchemaVersion shadows the field of the row, as a shallower field wins
	// over a deeper one of the same JSON name, and is never set.
	SchemaVersion *struct{} `json:"schema_version,omitempty"`

	// file is the item as the files routes give it. It is nil for an item
	// that the user cannot read: the relationship is then empty.
	file jsonapi.Object
	// gone is set for an item that is not found on its instance: its
	// resource is left out of the list.
	gone bool
}

func (r *recentsResource) Links() *jsonapi.LinksList { return nil }

func (r *recentsResource) Relationships() jsonapi.RelationshipMap {
	var data interface{}
	if r.file != nil {
		data = couchdb.DocReference{ID: r.file.ID(), Type: consts.Files}
	}
	return jsonapi.RelationshipMap{"file": jsonapi.Relationship{Data: data}}
}

func (r *recentsResource) Included() []jsonapi.Object {
	if r.file == nil {
		return nil
	}
	return []jsonapi.Object{r.file}
}

// ListRecentsHandler handles GET /files/recents: a page of the personal recents
// of the instance of the request, each with the current document of its item.
func ListRecentsHandler(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	if !recents.Enabled(inst) {
		return echo.NewHTTPError(http.StatusNotFound)
	}
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return err
	}
	switch pdoc.Type {
	case permission.TypeShareByLink, permission.TypeSharePreview, permission.TypeShareInteract:
		// A sharing member has no personal recents on the instance they act
		// on: they must never get the list of its owner.
		return middlewares.ErrForbidden
	}

	size, err := parseRecentsLimit(c)
	if err != nil {
		return err
	}
	after, err := parseRecentsCursor(c)
	if err != nil {
		return err
	}

	// The first page seeds the recents once, so that the first switch to the
	// server-side recents never shows an empty page. A deeper page is reached
	// through a cursor and never seeds. The seed is best effort: a failure
	// leaves the page to the stored rows without failing the read.
	if !c.QueryParams().Has(recentsCursorParam) {
		recents.Seed(inst)
	}

	// One more row than the page tells whether rows follow it.
	rows, err := recents.Candidates(inst, size+1, after)
	if err != nil {
		return err
	}
	var links *jsonapi.LinksList
	if len(rows) > size {
		// The cursor is the position of the last row read, whether it gives a
		// resource or not: the rows left out are not read again, and no more
		// rows are read to fill the page.
		rows = rows[:size]
		cursor, err := recents.PositionOf(&rows[size-1]).Cursor()
		if err != nil {
			return err
		}
		next := url.Values{
			recentsLimitParam:  {strconv.Itoa(size)},
			recentsCursorParam: {cursor},
		}
		links = &jsonapi.LinksList{Next: "/files/recents?" + next.Encode()}
	}

	resources, err := resolveRecents(inst, rows)
	if err != nil {
		return err
	}
	return jsonapi.DataListWithIncluded(c, http.StatusOK, resources, links)
}

// resolveRecents returns the resources of the rows of a page, in their order,
// without those of the items that are gone. The rows are grouped by the
// instance that holds their items, to read the items of each instance at once.
func resolveRecents(inst *instance.Instance, rows []recents.FileDirActivity) ([]jsonapi.Object, error) {
	all := make([]*recentsResource, len(rows))
	byOwner := make(map[string][]*recentsResource)
	for i := range rows {
		all[i] = &recentsResource{FileDirActivity: &rows[i]}
		owner := rows[i].OwnerDomain
		byOwner[owner] = append(byOwner[owner], all[i])
	}
	for owner, group := range byOwner {
		// The items of another instance are not read yet: their resources
		// keep an empty relationship.
		if owner != "" {
			continue
		}
		if err := resolveOwnRecents(inst, group); err != nil {
			return nil, err
		}
	}
	resources := make([]jsonapi.Object, 0, len(all))
	for _, r := range all {
		if !r.gone {
			resources = append(resources, r)
		}
	}
	return resources, nil
}

// resolveOwnRecents gives the resources of the items of the instance their
// documents, read in one request, as GetAllDocs does.
func resolveOwnRecents(inst *instance.Instance, group []*recentsResource) error {
	keys := make([]string, len(group))
	for i, r := range group {
		keys[i] = r.ID()
	}
	var results []vfs.DirOrFileDoc
	req := &couchdb.AllDocsRequest{Keys: keys}
	if err := couchdb.GetAllDocs(inst, consts.Files, req, &results); err != nil {
		return err
	}
	docs := make(map[string]*vfs.DirOrFileDoc, len(results))
	var thumbIDs []string
	for i := range results {
		if results[i].DirDoc == nil {
			continue
		}
		docs[results[i].ID()] = &results[i]
		if _, f := results[i].Refine(); f != nil && (f.Class == "image" || f.Class == "pdf") {
			thumbIDs = append(thumbIDs, f.ID())
		}
	}
	// The secrets of the thumbnail links are created in batch.
	var secrets map[string]string
	if len(thumbIDs) > 0 {
		secrets, _ = vfs.GetStore().AddThumbs(inst, thumbIDs)
	}

	fp := vfs.NewFilePatherWithCache(inst.VFS())
	for _, r := range group {
		doc, ok := docs[r.ID()]
		if !ok {
			r.gone = true
			continue
		}
		if d, f := doc.Refine(); d != nil {
			r.file = NewDir(d, nil)
		} else {
			file := NewFile(f, inst, nil)
			file.IncludePath(fp)
			if secret, ok := secrets[f.ID()]; ok {
				file.SetThumbSecret(secret)
			}
			r.file = file
		}
	}
	return nil
}

// parseRecentsLimit returns the size of the page: an invalid value is rejected,
// and a value above the maximum is brought down to it, as for the other
// paginated routes of the stack.
func parseRecentsLimit(c echo.Context) (int, error) {
	if !c.QueryParams().Has(recentsLimitParam) {
		return recents.DefaultPageSize, nil
	}
	size, err := strconv.Atoi(c.QueryParam(recentsLimitParam))
	if err != nil || size < 1 {
		return 0, jsonapi.InvalidParameter(recentsLimitParam, errors.New("invalid page limit"))
	}
	return min(size, recents.MaxPageSize), nil
}

// parseRecentsCursor returns the position after which the page starts.
func parseRecentsCursor(c echo.Context) (recents.Position, error) {
	if !c.QueryParams().Has(recentsCursorParam) {
		return recents.Position{}, nil
	}
	after, err := recents.ParseCursor(c.QueryParam(recentsCursorParam))
	if err != nil {
		return recents.Position{}, jsonapi.InvalidParameter(recentsCursorParam, errors.New("invalid cursor"))
	}
	return after, nil
}
