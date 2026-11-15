package files

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/search"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// maxSearchQueryLength is the maximal length of a search query, in characters.
const maxSearchQueryLength = 256

// apiSearchHit is a search hit in JSON-API.
type apiSearchHit struct {
	DocID     string `json:"_id,omitempty"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Mime      string `json:"mime,omitempty"`
	Class     string `json:"class,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	DirID     string `json:"dir_id"`
	Path      string `json:"path"`
	DriveID   string `json:"drive_id,omitempty"`
}

func (h *apiSearchHit) ID() string                             { return h.DocID }
func (h *apiSearchHit) Rev() string                            { return "" }
func (h *apiSearchHit) DocType() string                        { return consts.Files }
func (h *apiSearchHit) Clone() couchdb.Doc                     { cloned := *h; return &cloned }
func (h *apiSearchHit) SetID(id string)                        { h.DocID = id }
func (h *apiSearchHit) SetRev(_ string)                        {}
func (h *apiSearchHit) Relationships() jsonapi.RelationshipMap { return nil }
func (h *apiSearchHit) Included() []jsonapi.Object             { return nil }
func (h *apiSearchHit) Links() *jsonapi.LinksList              { return nil }

var _ jsonapi.Object = (*apiSearchHit)(nil)

// SearchFiles searches the files and folders of the user, and those of the
// shared drives they are a member of, by their name.
func SearchFiles(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Files); err != nil {
		return err
	}
	var body struct {
		Q      string `json:"q"`
		Limit  int    `json:"limit"`
		Cursor string `json:"cursor"`
	}
	if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return jsonapi.BadRequest(err)
	}
	q := strings.TrimSpace(body.Q)
	if q == "" || utf8.RuneCountInString(q) > maxSearchQueryLength {
		return jsonapi.BadRequest(errors.New("q must have 1 to 256 characters"))
	}
	cursor := body.Cursor
	if pageCursor := c.QueryParam("page[cursor]"); pageCursor != "" {
		cursor = pageCursor
	}

	client := opensearch.New(config.GetConfig().Search)
	if client == nil {
		return jsonapi.NewError(http.StatusServiceUnavailable, "search is not configured")
	}
	inst := middlewares.GetInstance(c)
	ctx := c.Request().Context()
	results, err := search.Search(ctx, client, inst, q, body.Limit, cursor)
	if errors.Is(err, search.ErrInvalidCursor) {
		return jsonapi.BadRequest(err)
	}
	if err != nil {
		return err
	}

	hits := make([]search.Hit, len(results.Hits))
	for i, h := range results.Hits {
		hits[i] = search.Hit{OwnerInstanceID: h.OwnerInstanceID, DirID: h.DirID, DriveID: h.DriveID}
	}
	paths, err := search.VisiblePaths(inst, hits, results.Drives)
	if err != nil {
		return err
	}
	objs := make([]jsonapi.Object, 0, len(results.Hits))
	for i, h := range results.Hits {
		if !paths[i].OK {
			continue
		}
		objs = append(objs, &apiSearchHit{
			DocID: h.DocID, Name: h.Name, Type: h.Type, Mime: h.Mime, Class: h.Class,
			UpdatedAt: h.UpdatedAt, DirID: h.DirID, Path: paths[i].Path, DriveID: h.DriveID,
		})
	}

	var links jsonapi.LinksList
	if results.Cursor != "" {
		links.Next = "/files/search?page[cursor]=" + url.QueryEscape(results.Cursor)
	}
	return jsonapi.DataList(c, http.StatusOK, objs, &links)
}
