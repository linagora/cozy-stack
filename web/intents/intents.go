package intents

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/intent"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

type apiIntent struct {
	doc *intent.Intent
	ins *instance.Instance
}

func (i *apiIntent) ID() string                             { return i.doc.ID() }
func (i *apiIntent) Rev() string                            { return i.doc.Rev() }
func (i *apiIntent) DocType() string                        { return consts.Intents }
func (i *apiIntent) Clone() couchdb.Doc                     { return i }
func (i *apiIntent) SetID(id string)                        { i.doc.SetID(id) }
func (i *apiIntent) SetRev(rev string)                      { i.doc.SetRev(rev) }
func (i *apiIntent) Relationships() jsonapi.RelationshipMap { return nil }
func (i *apiIntent) Included() []jsonapi.Object             { return nil }
func (i *apiIntent) Links() *jsonapi.LinksList {
	parts := strings.SplitN(i.doc.Client, "/", 2)
	if len(parts) < 2 {
		return nil
	}
	perms, err := permission.GetForWebapp(i.ins, parts[1])
	if err != nil {
		return nil
	}
	return &jsonapi.LinksList{
		Self:  "/intents/" + i.ID(),
		Perms: "/permissions/" + perms.ID(),
	}
}

// In the JSON-API, the client is the domain of the client-side app that
// asked the intent (it is used for postMessage)
func (i *apiIntent) MarshalJSON() ([]byte, error) {
	was := i.doc.Client
	parts := strings.SplitN(i.doc.Client, "/", 2)
	if len(parts) < 2 {
		i.doc.Client = ""
	} else {
		i.doc.Client = i.resolveClientURL(parts[1])
	}
	res, err := json.Marshal(i.doc)
	i.doc.Client = was
	return res, err
}

func (i *apiIntent) resolveClientURL(slug string) string {
	return app.ResolveClientURL(i.ins, slug)
}

func RequestAppSourceID(pdoc *permission.Permission) string {
	if pdoc.Type != permission.TypeOauth {
		return pdoc.SourceID
	}

	oc, ok := pdoc.Client.(*oauth.Client)
	if !ok {
		return pdoc.SourceID
	}

	if slug := oauth.GetLinkedAppSlug(oc.SoftwareID); slug != "" {
		return consts.Apps + "/" + slug
	}

	return pdoc.SourceID
}

func createIntent(c echo.Context) error {
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	instance := middlewares.GetInstance(c)
	intent := &intent.Intent{}
	if _, err = jsonapi.Bind(c.Request().Body, intent); err != nil {
		return jsonapi.BadRequest(err)
	}
	if intent.Action == "" {
		return jsonapi.InvalidParameter("action", errors.New("Action is missing"))
	}
	if intent.Type == "" {
		return jsonapi.InvalidParameter("type", errors.New("Type is missing"))
	}
	intent.Client = RequestAppSourceID(pdoc)
	intent.SetID("")
	intent.SetRev("")
	intent.Services = nil
	if err = intent.Save(instance); err != nil {
		return wrapIntentsError(err)
	}
	if err = intent.FillServices(instance); err != nil {
		return wrapIntentsError(err)
	}
	// Fill available webapps only if there are no services found
	if len(intent.Services) == 0 {
		if err = intent.FillAvailableWebapps(instance); err != nil {
			return wrapIntentsError(err)
		}
	}
	if err = intent.Save(instance); err != nil {
		return wrapIntentsError(err)
	}
	api := &apiIntent{intent, instance}
	return jsonapi.Data(c, http.StatusOK, api, nil)
}

func getIntent(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	intent := &intent.Intent{}
	id := c.Param("id")
	pdoc, err := middlewares.GetPermission(c)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	if err = couchdb.GetDoc(instance, consts.Intents, id, intent); err != nil {
		return wrapIntentsError(err)
	}
	allowed := false
	for _, service := range intent.Services {
		if pdoc.SourceID == consts.Apps+"/"+service.Slug {
			allowed = true
		}
	}
	if !allowed {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	api := &apiIntent{intent, instance}
	return jsonapi.Data(c, http.StatusOK, api, nil)
}

func wrapIntentsError(err error) error {
	if couchdb.IsNotFoundError(err) {
		return jsonapi.NotFound(err)
	}
	return jsonapi.InternalServerError(err)
}

func NewAPIIntent(doc *intent.Intent, ins *instance.Instance) *apiIntent {
	return &apiIntent{doc: doc, ins: ins}
}

// Routes sets the routing for the intents service
func Routes(router *echo.Group) {
	router.POST("", createIntent)
	router.GET("/:id", getIntent)
}
