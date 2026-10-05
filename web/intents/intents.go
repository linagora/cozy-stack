package intents

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/intent"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

type apiIntent struct {
	doc         *intent.Intent
	ins         *instance.Instance
	sessionCode string
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
	output := i.doc.Clone().(*intent.Intent)
	parts := strings.SplitN(output.Client, "/", 2)
	if len(parts) < 2 {
		output.Client = ""
	} else {
		output.Client = i.resolveClientURL(parts[1])
	}
	if i.sessionCode != "" {
		addSessionCodeToServices(i.ins, output.Services, i.sessionCode)
	}
	return json.Marshal(output)
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
	instance := middlewares.GetInstance(c)
	grant, err := createIntentSessionCodeGrant(c, instance)
	if err != nil {
		return err
	}
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
	intent.Client = RequestAppSourceID(grant.Permission)
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
	sessionCode := ""
	if grant.Source != "" && hasCozyService(instance, intent.Services) {
		sessionCode, err = auth.MintSessionCode(c, instance, grant.Source)
		if err != nil {
			return jsonapi.InternalServerError(err)
		}
	}
	api := &apiIntent{doc: intent, ins: instance, sessionCode: sessionCode}
	return jsonapi.Data(c, http.StatusOK, api, nil)
}

func createIntentSessionCodeGrant(c echo.Context, inst *instance.Instance) (auth.SessionCodeGrant, error) {
	if c.QueryParam("force_session_id") != "true" {
		pdoc, err := middlewares.GetPermission(c)
		if err != nil {
			return auth.SessionCodeGrant{}, echo.NewHTTPError(http.StatusForbidden)
		}
		return auth.SessionCodeGrant{Permission: pdoc}, nil
	}

	grant, ok := auth.AuthorizeSessionCodeToken(c, inst)
	if !ok {
		return auth.SessionCodeGrant{}, echo.NewHTTPError(http.StatusForbidden)
	}
	return grant, nil
}

func addSessionCodeToServices(inst *instance.Instance, services []intent.Service, sessionCode string) {
	for idx := range services {
		// Session codes grant a Cozy session; never include them in external URLs.
		u := parseCozyServiceURL(inst, services[idx].Slug, services[idx].Href)
		if u == nil {
			continue
		}
		query := u.Query()
		query.Set("session_code", sessionCode)
		u.RawQuery = query.Encode()
		services[idx].Href = u.String()
	}
}

func hasCozyService(inst *instance.Instance, services []intent.Service) bool {
	for _, service := range services {
		if parseCozyServiceURL(inst, service.Slug, service.Href) != nil {
			return true
		}
	}
	return false
}

func parseCozyServiceURL(inst *instance.Instance, slug, href string) *url.URL {
	u, err := url.Parse(href)
	if err != nil || u.Host != inst.SubDomain(slug).Host {
		return nil
	}
	return u
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
		if RequestAppSourceID(pdoc) == consts.Apps+"/"+service.Slug {
			allowed = true
		}
	}
	if !allowed {
		return echo.NewHTTPError(http.StatusForbidden)
	}
	api := &apiIntent{doc: intent, ins: instance}
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
