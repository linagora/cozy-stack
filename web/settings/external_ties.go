package settings

import (
	"net/http"

	"github.com/cozy/cozy-stack/model/instance"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

type apiExternalTies struct {
	*csettings.ExternalTies

	DocID string `json:"_id,omitempty"`
}

func (c *apiExternalTies) ID() string                             { return c.DocID }
func (c *apiExternalTies) Rev() string                            { return "" }
func (c *apiExternalTies) DocType() string                        { return consts.Settings }
func (c *apiExternalTies) Clone() couchdb.Doc                     { cloned := *c; return &cloned }
func (c *apiExternalTies) SetID(id string)                        { c.DocID = id }
func (c *apiExternalTies) SetRev(rev string)                      {}
func (c *apiExternalTies) Relationships() jsonapi.RelationshipMap { return nil }
func (c *apiExternalTies) Included() []jsonapi.Object             { return nil }
func (c *apiExternalTies) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/settings/external-ties"}
}
func (c *apiExternalTies) Fetch(field string) []string { return nil }

func NewExternalTies(ties *csettings.ExternalTies) jsonapi.Object {
	return &apiExternalTies{
		ExternalTies: ties,
		DocID:        consts.ExternalTiesID,
	}
}

func (h *HTTPHandler) getExternalTies(c echo.Context) error {
	if !middlewares.IsLoggedIn(c) && !middlewares.HasWebAppToken(c) {
		return middlewares.ErrForbidden
	}
	inst := middlewares.GetInstance(c)

	ties, err := h.svc.GetExternalTies(inst)
	if err != nil {
		return jsonapi.InternalServerError(err)
	}

	doc := NewExternalTies(ties)

	return jsonapi.Data(c, http.StatusOK, doc, nil)
}

func (h *HTTPHandler) redirectToPremium(c echo.Context) error {
	if !middlewares.IsLoggedIn(c) {
		return middlewares.ErrForbidden
	}
	inst := middlewares.GetInstance(c)

	// TODO: move to a SettingsService method to avoid the dependency on the
	// instance module.
	url, err := inst.ManagerURL(instance.ManagerPremiumURL)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if url == "" {
		return echo.NewHTTPError(http.StatusNotFound, "This instance does not have a premium subscription manager.")
	}

	return c.Redirect(http.StatusMovedPermanently, url)
}
