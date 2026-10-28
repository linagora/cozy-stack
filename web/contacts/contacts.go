// Package contacts exposes the routes for the myself document and the
// suggestions of recipients.
package contacts

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

type apiMyself struct{ *contact.Contact }

func (m *apiMyself) MarshalJSON() ([]byte, error)           { return json.Marshal(m.Contact) }
func (m *apiMyself) Links() *jsonapi.LinksList              { return &jsonapi.LinksList{Self: "/contacts/myself"} }
func (m *apiMyself) Relationships() jsonapi.RelationshipMap { return jsonapi.RelationshipMap{} }
func (m *apiMyself) Included() []jsonapi.Object             { return []jsonapi.Object{} }

// MyselfHandler is the handler for POST /contacts/myself. It returns the
// information about the io.cozy.contacts document for the owner of this
// instance, the "myself" contact. If the document does not exist, it is
// recreated with some basic fields.
func MyselfHandler(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	myself, err := contact.GetMyself(inst)
	if errors.Is(err, contact.ErrNotFound) {
		var settings *couchdb.JSONDoc
		settings, err = inst.SettingsDocument()
		if err == nil {
			myself, err = contact.CreateMyself(inst, settings)
		}
	}
	if err != nil {
		return err
	}

	if err := middlewares.Allow(c, permission.GET, myself); err != nil {
		return err
	}

	return jsonapi.Data(c, http.StatusOK, &apiMyself{myself}, nil)
}

// apiSuggestion has no id and no kind when no document matches the email.
type apiSuggestion struct {
	Type       string                 `json:"type"`
	ID         string                 `json:"id,omitempty"`
	Attributes map[string]interface{} `json:"attributes"`
	Meta       map[string]string      `json:"meta,omitempty"`
}

// SuggestHandler is the handler for GET /contacts/suggest. It returns the
// groups and people the user can share with, for what they are typing.
func SuggestHandler(c echo.Context) error {
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Contacts); err != nil {
		return err
	}
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Groups); err != nil {
		return err
	}
	q := strings.TrimSpace(c.QueryParam("q"))
	if utf8.RuneCountInString(q) < 2 {
		return jsonapi.InvalidParameter("q", errors.New("at least 2 characters are required"))
	}
	limit := 20
	if l := c.QueryParam("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > 256 {
			return jsonapi.InvalidParameter("limit", errors.New("must be between 1 and 256"))
		}
		limit = n
	}

	suggestions, err := sharing.Suggest(middlewares.GetInstance(c), q, limit)
	if err != nil {
		return err
	}
	data := make([]apiSuggestion, len(suggestions))
	for i, s := range suggestions {
		if s.Group != nil {
			data[i] = apiSuggestion{
				Type: consts.Groups,
				ID:   s.Group.ID(),
				Attributes: map[string]interface{}{
					"name": s.Group.Name(), "color": s.Group.Color(), "member_count": s.MemberCount,
				},
			}
			continue
		}
		data[i] = apiSuggestion{Type: consts.Contacts}
		if s.Contact == nil {
			data[i].Attributes = map[string]interface{}{
				"fullname": s.Name,
				"email":    []interface{}{map[string]interface{}{"address": s.Email, "primary": true}},
			}
			continue
		}
		data[i].ID = s.Contact.ID()
		delete(s.Contact.M, "_id")
		delete(s.Contact.M, "_rev")
		data[i].Attributes = s.Contact.M
		data[i].Meta = map[string]string{"kind": s.Kind}
	}
	c.Response().Header().Set(echo.HeaderContentType, jsonapi.ContentType)
	return c.JSON(http.StatusOK, echo.Map{"data": data})
}

// Routes sets the routing for the contacts.
func Routes(router *echo.Group) {
	router.POST("/myself", MyselfHandler)
	router.GET("/suggest", SuggestHandler)
}
