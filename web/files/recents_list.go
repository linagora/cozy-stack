package files

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/recents"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

const (
	recentsLimitParam  = "page[limit]"
	recentsCursorParam = "page[cursor]"
)

// recentsResource is the JSON:API resource of a recents row. Its attributes
// are all the stored fields of the row, without the version of its format.
type recentsResource struct {
	*recents.FileDirActivity

	// SchemaVersion shadows the field of the row, as a shallower field wins
	// over a deeper one of the same JSON name, and is never set.
	SchemaVersion *struct{} `json:"schema_version,omitempty"`

	// DriveID shadows the stored `drive_id` to suppress it, and DriveId
	// re-emits the value as `driveId`, the name the files routes use for the
	// drive of a shared item.
	DriveID *struct{} `json:"drive_id,omitempty"`
	DriveId string    `json:"driveId,omitempty"`
}

func (r *recentsResource) Relationships() jsonapi.RelationshipMap { return nil }
func (r *recentsResource) Included() []jsonapi.Object             { return nil }
func (r *recentsResource) Links() *jsonapi.LinksList              { return nil }

// ListRecentsHandler handles GET /files/recents: a page of the personal recents
// of the instance of the request, built from the stored rows only.
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

	objs := make([]jsonapi.Object, len(rows))
	for i := range rows {
		objs[i] = &recentsResource{FileDirActivity: &rows[i], DriveId: rows[i].DriveID}
	}
	return jsonapi.DataList(c, http.StatusOK, objs, links)
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
