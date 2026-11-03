// Package apps is the HTTP frontend of the application package. It
// exposes the HTTP api install, update or uninstall applications.
package apps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	"github.com/cozy/cozy-stack/pkg/appfs"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/limits"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/registry"
	"github.com/cozy/cozy-stack/web/jobs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
)

// JSMimeType is the content-type for javascript
const JSMimeType = "application/javascript"

const typeTextEventStream = "text/event-stream"

type AppLog struct {
	Time  time.Time `json:"timestamp"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
}

type apiApp struct {
	app.Manifest
}

func (man *apiApp) MarshalJSON() ([]byte, error) {
	return json.Marshal(man.Manifest)
}

// Links is part of the Manifest interface
func (man *apiApp) Links() *jsonapi.LinksList {
	var route string
	links := jsonapi.LinksList{}
	switch a := man.Manifest.(type) {
	case (*app.WebappManifest):
		route = "/apps/"
		if a.Icon() != "" {
			links.Icon = "/apps/" + a.Slug() + "/icon/" + a.Version()
		}
		if (a.State() == app.Ready || a.State() == app.Installed) &&
			a.Instance != nil {
			links.Related = a.Instance.SubDomain(a.Slug()).String()
		}
	case (*app.KonnManifest):
		route = "/konnectors/"
		if a.Icon() != "" {
			links.Icon = "/konnectors/" + a.Slug() + "/icon/" + a.Version()
		}
		links.Perms = "/permissions/konnectors/" + a.Slug()
	}
	if route != "" {
		links.Self = route + man.Manifest.Slug()
	}
	return &links
}

// Relationships is part of the Manifest interface
func (man *apiApp) Relationships() jsonapi.RelationshipMap {
	return jsonapi.RelationshipMap{}
}

// Included is part of the Manifest interface
func (man *apiApp) Included() []jsonapi.Object {
	return []jsonapi.Object{}
}

// apiApp is a jsonapi.Object
var _ jsonapi.Object = (*apiApp)(nil)

func getHandler(appType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		slug := c.Param("slug")
		man, err := app.GetBySlug(instance, slug, appType)
		if err != nil {
			return wrapAppsError(err)
		}
		if err := middlewares.Allow(c, permission.GET, man); err != nil {
			return err
		}

		registries := instance.Registries()
		copier := app.Copier(man.AppType(), instance)
		man = app.DoLazyUpdate(instance, man, copier, registries)

		if appType == consts.WebappType {
			// TODO: Check is this line is really necessary
			man.(*app.WebappManifest).Instance = instance
		}
		return jsonapi.Data(c, http.StatusOK, &apiApp{man}, nil)
	}
}

func downloadHandler(appType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		inst := middlewares.GetInstance(c)
		slug := c.Param("slug")
		man, err := app.GetBySlug(inst, slug, appType)
		if err != nil {
			return wrapAppsError(err)
		}
		if err := middlewares.Allow(c, permission.GET, man); err != nil {
			return err
		}

		version := c.Param("version")
		if version == "" {
			version = man.Version()
		}

		source := man.Source()
		if strings.HasPrefix(source, "registry://") {
			registries := inst.Registries()
			v, err := registry.GetVersion(slug, version, registries)
			if err != nil {
				return wrapAppsError(err)
			}
			return c.Redirect(http.StatusSeeOther, v.URL)
		}

		if version != man.Version() {
			err := errors.New("code for this version is not available")
			return jsonapi.PreconditionFailed("version", err)
		}

		var fs appfs.FileServer
		switch appType {
		case consts.WebappType:
			man := man.(*app.WebappManifest)
			if man.FromAppsDir {
				fs = app.FSForAppDir(slug)
			} else {
				fs = app.AppsFileServer(inst)
			}
		case consts.KonnectorType:
			fs = app.KonnectorsFileServer(inst)
		}

		return fs.ServeCodeTarball(c.Response(), c.Request(), slug, version, man.Checksum())
	}
}

// installHandler handles all POST /:slug request and tries to install
// or update the application with the given Source.
func installHandler(installerType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		slug := c.Param("slug")
		source := c.QueryParam("Source")
		if source == "" {
			source = "registry://" + slug + "/stable"
		}
		if err := middlewares.AllowInstallApp(c, installerType, source, permission.POST); err != nil {
			return err
		}

		var overridenParameters map[string]interface{}
		if p := c.QueryParam("Parameters"); p != "" {
			if err := json.Unmarshal([]byte(p), &overridenParameters); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest)
			}
		}

		var w http.ResponseWriter
		isEventStream := c.Request().Header.Get(echo.HeaderAccept) == typeTextEventStream
		if isEventStream {
			w = c.Response().Writer
			w.Header().Set(echo.HeaderContentType, typeTextEventStream)
			w.WriteHeader(200)
		}

		inst, err := app.NewInstaller(instance, app.Copier(installerType, instance),
			&app.InstallerOptions{
				Operation:   app.Install,
				Type:        installerType,
				SourceURL:   source,
				Slug:        slug,
				Deactivated: c.QueryParam("Deactivated") == "true",
				Registries:  instance.Registries(),

				OverridenParameters: overridenParameters,
			},
		)
		if err != nil {
			if isEventStream {
				var b []byte
				if b, err = json.Marshal(err.Error()); err == nil {
					writeStream(w, "error", string(b))
				}
			}
			return wrapAppsError(err)
		}

		go inst.Run()
		return pollInstaller(c, instance, isEventStream, w, slug, inst)
	}
}

// logsHandler handles all POST /:slug/logs requests and forwards the log lines
// sent as a JSON array to the server logger.
func logsHandler(appType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		inst := middlewares.GetInstance(c)

		slug := c.Param("slug")
		if err := middlewares.AllowMaximal(c); err != nil {
			// If logs are not sent by the flagship app, check that it's sent by
			// a konnector or an app with the logs permission and get its slug
			// from the permission.
			pdoc, err := middlewares.GetPermission(c)
			if err != nil {
				return err
			}

			if err := middlewares.AllowWholeType(c, permission.POST, consts.AppLogs); err != nil {
				return err
			}

			if appType == consts.KonnectorType && pdoc.Type == permission.TypeKonnector {
				slug = strings.TrimPrefix(pdoc.SourceID, consts.Konnectors+"/")
			} else if appType == consts.WebappType && pdoc.Type == permission.TypeWebapp {
				slug = strings.TrimPrefix(pdoc.SourceID, consts.Apps+"/")
			} else {
				return middlewares.ErrForbidden
			}
		}

		clientSide := false
		if appType == consts.KonnectorType {
			man, err := app.GetKonnectorBySlug(inst, slug)
			if err != nil {
				return wrapAppsError(err)
			}
			clientSide = man.ClientSide()
		}

		var logs []AppLog
		if err := json.NewDecoder(c.Request().Body).Decode(&logs); err != nil {
			return jsonapi.BadJSON()
		}

		l := logger.WithDomain(inst.Domain).WithNamespace("jobs").
			WithField("slug", slug).
			WithField("job_id", c.QueryParam("job_id"))

		if clientSide {
			l = l.WithField("worker_id", "client")
		}
		for _, log := range logs {
			level, err := logger.ParseLevel(log.Level)
			if err != nil {
				return jsonapi.InvalidAttribute("level", err)
			}

			l := l.WithTime(log.Time)

			if v := c.QueryParam("version"); v != "" {
				l = l.WithField("version", v)
			}

			l.Log(level, log.Msg)
		}

		return c.NoContent(http.StatusNoContent)
	}
}

// updateHandler handles all POST /:slug request and tries to install
// or update the application with the given Source.
func updateHandler(installerType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		slug := c.Param("slug")
		source := c.QueryParam("Source")
		if err := middlewares.AllowInstallApp(c, installerType, source, permission.POST); err != nil {
			return err
		}

		var overridenParameters map[string]interface{}
		if p := c.QueryParam("Parameters"); p != "" {
			if err := json.Unmarshal([]byte(p), &overridenParameters); err != nil {
				return echo.NewHTTPError(http.StatusBadRequest)
			}
		}

		var w http.ResponseWriter
		isEventStream := c.Request().Header.Get(echo.HeaderAccept) == typeTextEventStream
		if isEventStream {
			w = c.Response().Writer
			w.Header().Set(echo.HeaderContentType, typeTextEventStream)
			w.WriteHeader(200)
		}

		permissionsAcked, _ := strconv.ParseBool(c.QueryParam("PermissionsAcked"))
		inst, err := app.NewInstaller(instance, app.Copier(installerType, instance),
			&app.InstallerOptions{
				Operation:  app.Update,
				Type:       installerType,
				SourceURL:  source,
				Slug:       slug,
				Registries: instance.Registries(),

				PermissionsAcked:    permissionsAcked,
				OverridenParameters: overridenParameters,
			},
		)
		if err != nil {
			if isEventStream {
				var b []byte
				if b, err = json.Marshal(err.Error()); err == nil {
					writeStream(w, "error", string(b))
				}
				return nil
			}
			return wrapAppsError(err)
		}

		go inst.Run()
		return pollInstaller(c, instance, isEventStream, w, slug, inst)
	}
}

// deleteHandler handles all DELETE /:slug used to delete an application with
// the specified slug.
func deleteHandler(installerType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		slug := c.Param("slug")
		source := "registry://" + slug
		if err := middlewares.AllowInstallApp(c, installerType, source, permission.DELETE); err != nil {
			return err
		}

		// Check if there is a mobile client attached to this app
		if installerType == consts.WebappType {
			oauthClient, err := oauth.FindClientBySoftwareID(instance, "registry://"+slug)
			if err == nil && oauthClient != nil {
				return wrapAppsError(app.ErrLinkedAppExists)
			}
		}

		// Delete accounts locally and remotely for banking konnectors
		if installerType == consts.KonnectorType {
			toDelete, err := findAccountsToDelete(instance, slug)
			if err != nil {
				return wrapAppsError(err)
			}
			if len(toDelete) > 0 {
				man, err := app.GetKonnectorBySlug(instance, slug)
				if err != nil {
					return wrapAppsError(err)
				}
				deleteKonnectorWithAccounts(instance, man, toDelete)
				return jsonapi.Data(c, http.StatusAccepted, &apiApp{man}, nil)
			}
		}

		inst, err := app.NewInstaller(instance, app.Copier(installerType, instance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       installerType,
				Slug:       slug,
				Registries: instance.Registries(),
			},
		)
		if err != nil {
			return wrapAppsError(err)
		}
		man, err := inst.RunSync()
		if err != nil {
			return wrapAppsError(err)
		}
		return jsonapi.Data(c, http.StatusOK, &apiApp{man}, nil)
	}
}

func findAccountsToDelete(instance *instance.Instance, slug string) ([]account.CleanEntry, error) {
	jobsSystem := job.System()
	triggers, err := jobsSystem.GetAllTriggers(instance)
	if err != nil {
		return nil, err
	}

	var toDelete []account.CleanEntry
	for _, t := range triggers {
		if !t.Infos().IsKonnectorTrigger() {
			continue
		}

		var msg struct {
			Account string `json:"account"`
			Slug    string `json:"konnector"`
		}
		err := t.Infos().Message.Unmarshal(&msg)
		if err == nil && msg.Slug == slug && msg.Account != "" {
			// XXX we can have several triggers for the same account (e.g. cron + webhook)
			hasEntry := false
			for i, entry := range toDelete {
				if entry.Account.ID() == msg.Account {
					toDelete[i].Triggers = append(entry.Triggers, t)
					hasEntry = true
					break
				}
			}
			if !hasEntry {
				acc := &account.Account{}
				if err := couchdb.GetDoc(instance, consts.Accounts, msg.Account, acc); err == nil {
					entry := account.CleanEntry{
						Account:  acc,
						Triggers: []job.Trigger{t},
					}
					toDelete = append(toDelete, entry)
				}
			}
		}
	}
	return toDelete, nil
}

func deleteKonnectorWithAccounts(instance *instance.Instance, man *app.KonnManifest, toDelete []account.CleanEntry) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				var err error
				switch r := r.(type) {
				case error:
					err = r
				default:
					err = fmt.Errorf("%v", r)
				}
				stack := make([]byte, 4<<10) // 4 KB
				length := runtime.Stack(stack, false)
				log := instance.Logger().WithNamespace("konnectors").WithField("panic", true)
				log.Errorf("PANIC RECOVER %s: %s", err.Error(), stack[:length])
			}
		}()

		slug := man.Slug()
		for i := range toDelete {
			toDelete[i].ManifestOnDelete = man.OnDeleteAccount() != ""
			toDelete[i].Slug = slug
		}

		log := instance.Logger().WithNamespace("konnectors")
		if err := account.CleanAndWait(instance, toDelete); err != nil {
			log.Errorf("Cannot clean accounts: %v", err)
			return
		}
		inst, err := app.NewInstaller(instance, app.Copier(consts.KonnectorType, instance),
			&app.InstallerOptions{
				Operation:  app.Delete,
				Type:       consts.KonnectorType,
				Slug:       slug,
				Registries: instance.Registries(),
			},
		)
		if err != nil {
			log.Errorf("Cannot uninstall the konnector: %v", err)
			return
		}
		_, err = inst.RunSync()
		if err != nil {
			log.Errorf("Cannot uninstall the konnector: %v", err)
		}
	}()
}

func pollInstaller(c echo.Context, instance *instance.Instance, isEventStream bool, w http.ResponseWriter, slug string, inst *app.Installer) error {
	if !isEventStream {
		man, _, err := inst.Poll()
		if err != nil {
			return wrapAppsError(err)
		}
		go func() {
			for {
				_, done, err := inst.Poll()
				if done || err != nil {
					break
				}
			}
		}()
		return jsonapi.Data(c, http.StatusAccepted, &apiApp{man}, nil)
	}

	manc := inst.ManifestChannel()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case man := <-manc:
			if err := man.Error(); err != nil {
				var b []byte
				if b, err = json.Marshal(err.Error()); err == nil {
					writeStream(w, "error", string(b))
				}
				return nil
			}
			buf := new(bytes.Buffer)
			if err := jsonapi.WriteData(buf, &apiApp{man}, nil); err == nil {
				writeStream(w, "state", strings.TrimSuffix(buf.String(), "\n"))
			}
			if s := man.State(); s == app.Ready || s == app.Installed || s == app.Errored {
				return nil
			}

		case <-ticker.C:
			_, _ = w.Write([]byte(": still working\r\n"))
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
}

func writeStream(w http.ResponseWriter, event string, b string) {
	s := fmt.Sprintf("event: %s\r\ndata: %s\r\n\r\n", event, b)
	_, err := w.Write([]byte(s))
	if err != nil {
		return
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// listWebappsHandler handles all GET / requests which can be used to list
// installed applications.
func listWebappsHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Apps); err != nil {
		return err
	}

	// Adding the startKey if it is given in the request
	startKey := c.QueryParam("start_key")

	// Same for the limit
	var limit int
	if l := c.QueryParam("limit"); l != "" {
		if converted, err := strconv.Atoi(l); err == nil {
			limit = converted
		}
	}

	docs, next, err := app.ListWebappsWithPagination(instance, limit, startKey)
	if err != nil {
		return wrapAppsError(err)
	}
	objs := make([]jsonapi.Object, len(docs))
	for i, d := range docs {
		d.Instance = instance
		objs[i] = &apiApp{d}
	}

	// Generating links list for the next apps
	links := generateLinksList(c, next, limit, next)

	return jsonapi.DataList(c, http.StatusOK, objs, links)
}

// listKonnectorsHandler handles all GET / requests which can be used to list
// installed applications.
func listKonnectorsHandler(c echo.Context) error {
	instance := middlewares.GetInstance(c)
	if err := middlewares.AllowWholeType(c, permission.GET, consts.Konnectors); err != nil {
		return err
	}

	// Adding the startKey if it is given in the request
	var startKey string
	if sk := c.QueryParam("start_key"); sk != "" {
		startKey = sk
	}

	// Same for the limit
	var limit int
	if l := c.QueryParam("limit"); l != "" {
		if converted, err := strconv.Atoi(l); err == nil {
			limit = converted
		}
	}
	docs, next, err := app.ListKonnectorsWithPagination(instance, limit, startKey)
	if err != nil {
		return wrapAppsError(err)
	}
	objs := make([]jsonapi.Object, len(docs))
	for i, d := range docs {
		objs[i] = &apiApp{d}
	}

	// Generating links list for the next konnectors
	links := generateLinksList(c, next, limit, next)

	return jsonapi.DataList(c, http.StatusOK, objs, links)
}

func generateLinksList(c echo.Context, next string, limit int, nextID string) *jsonapi.LinksList {
	links := &jsonapi.LinksList{}
	if next != "" { // Do not generate the next URL if there are no next konnectors
		nextURL := &url.URL{
			Scheme: c.Scheme(),
			Host:   c.Request().Host,
			Path:   c.Path(),
		}
		values := nextURL.Query()
		values.Set("start_key", nextID)
		values.Set("limit", strconv.Itoa(limit))
		nextURL.RawQuery = values.Encode()

		links.Next = nextURL.String()
	}
	return links
}

// iconHandler gives the icon of an application
func iconHandler(appType consts.AppType) echo.HandlerFunc {
	return func(c echo.Context) error {
		instance := middlewares.GetInstance(c)
		slug := c.Param("slug")
		version := c.Param("version")
		a, err := app.GetBySlug(instance, slug, appType)
		if err != nil {
			if errors.Is(err, app.ErrNotFound) {
				return jsonapi.NotFound(err)
			}
			return err
		}

		if !middlewares.IsLoggedIn(c) {
			if err := middlewares.Allow(c, permission.GET, a); err != nil {
				return echo.NewHTTPError(http.StatusUnauthorized, err.Error())
			}
		}

		if version != "" {
			// The maximum cache-control recommanded is one year :
			// https://www.ietf.org/rfc/rfc2616.txt
			c.Response().Header().Set("Cache-Control", "max-age=31536000, immutable")
		}

		var fs appfs.FileServer
		var filepath string
		switch appType {
		case consts.WebappType:
			a := a.(*app.WebappManifest)
			filepath = path.Join("/", a.Icon())
			if a.FromAppsDir {
				fs = app.FSForAppDir(slug)
			} else {
				fs = app.AppsFileServer(instance)
			}
		case consts.KonnectorType:
			filepath = path.Join("/", a.Icon())
			fs = app.KonnectorsFileServer(instance)
		}

		err = fs.ServeFileContent(c.Response(), c.Request(),
			a.Slug(), a.Version(), a.Checksum(), filepath)
		if os.IsNotExist(err) {
			return echo.NewHTTPError(http.StatusNotFound, err)
		}
		return err
	}
}

func createTrigger(c echo.Context) error {
	inst := middlewares.GetInstance(c)
	slug := c.Param("slug")
	man, err := app.GetBySlug(inst, slug, consts.KonnectorType)
	if err != nil {
		return wrapAppsError(err)
	}
	var createdByApp string
	if claims := c.Get("claims"); claims != nil {
		cl := claims.(permission.Claims)
		if cl.Subject != "" {
			createdByApp = cl.Subject
		}
	}
	t, err := man.(*app.KonnManifest).BuildTrigger(inst, c.QueryParam("AccountID"), createdByApp)
	if err != nil {
		return wrapAppsError(err)
	}
	if err = middlewares.Allow(c, permission.POST, t); err != nil {
		return err
	}
	sched := job.System()
	if err = sched.AddTrigger(t); err != nil {
		return wrapAppsError(err)
	}

	if c.QueryParam("ExecNow") == "true" {
		req := t.Infos().JobRequest()
		req.Manual = true
		_, _ = sched.PushJob(inst, req)
	}

	return jsonapi.Data(c, http.StatusCreated, jobs.NewAPITrigger(t.Infos(), inst), nil)
}

type apiOpenParams struct {
	slug   string
	cookie string
	params serveParams
}

func (o *apiOpenParams) ID() string         { return o.slug }
func (o *apiOpenParams) Rev() string        { return "" }
func (o *apiOpenParams) DocType() string    { return consts.AppsOpenParameters }
func (o *apiOpenParams) SetID(id string)    {}
func (o *apiOpenParams) SetRev(rev string)  {}
func (o *apiOpenParams) Clone() couchdb.Doc { return o }
func (o *apiOpenParams) MarshalJSON() ([]byte, error) {
	data := map[string]interface{}{}
	data["Cookie"] = o.cookie
	data["Token"] = o.params.Token
	data["Domain"] = o.params.Domain()
	data["SubDomain"] = o.params.SubDomain
	data["Tracking"] = strconv.FormatBool(o.params.Tracking)
	data["Locale"] = o.params.Locale()
	data["AppEditor"] = o.params.AppEditor()
	data["AppName"] = o.params.AppName()
	data["AppNamePrefix"] = o.params.AppNamePrefix()
	data["AppSlug"] = o.params.AppSlug()
	data["IconPath"] = o.params.IconPath()
	data["Flags"], _ = o.params.Flags()
	data["Capabilities"], _ = o.params.Capabilities()
	data["CozyBar"], _ = o.params.CozyBar()
	data["CozyFonts"] = o.params.CozyFonts()
	data["CozyClientJS"], _ = o.params.CozyClientJS()
	data["ThemeCSS"] = o.params.ThemeCSS()
	data["Favicon"] = o.params.Favicon()
	data["DefaultWallpaper"] = o.params.DefaultWallpaper()
	data["Warnings"], _ = o.params.Warnings()
	return json.Marshal(data)
}

func (o *apiOpenParams) Relationships() jsonapi.RelationshipMap { return nil }
func (o *apiOpenParams) Included() []jsonapi.Object             { return nil }
func (o *apiOpenParams) Links() *jsonapi.LinksList {
	return &jsonapi.LinksList{Self: "/apps/" + o.slug + "/open"}
}

// openWebapp handles GET /apps/:slug/open requests and returns the data useful
// for the flagship app to open the given the webapp in a webview.
func openWebapp(c echo.Context) error {
	if err := middlewares.AllowMaximal(c); err != nil {
		return wrapAppsError(err)
	}

	inst := middlewares.GetInstance(c)
	slug := c.Param("slug")
	webapp, err := app.GetWebappBySlug(inst, slug)
	if err != nil {
		return wrapAppsError(err)
	}

	var cookie *http.Cookie
	sess, err := session.FromCookie(c, inst)
	if err == nil {
		cookie, err = c.Cookie(session.CookieName(inst))
		if err != nil {
			return wrapAppsError(err)
		}
		cookie.MaxAge = 0
		cookie.Path = "/"
		cookie.Domain = session.CookieDomain(inst)
		cookie.Secure = !build.IsDevRelease()
		cookie.HttpOnly = true
		cookie.SameSite = http.SameSiteLaxMode
	} else {
		sess, err = session.New(inst, session.NormalRun, "")
		if err != nil {
			return wrapAppsError(err)
		}
		cookie, err = sess.ToCookie()
		if err != nil {
			return wrapAppsError(err)
		}
	}

	isLoggedIn := true
	params := buildServeParams(c, inst, webapp, nil, isLoggedIn, sess.ID())
	obj := &apiOpenParams{
		slug:   slug,
		cookie: cookie.String(),
		params: params,
	}
	return jsonapi.Data(c, http.StatusOK, obj, nil)
}

// WebappsRoutes sets the routing for the web apps service
func WebappsRoutes(router *echo.Group) {
	router.GET("/", listWebappsHandler)
	router.GET("/:slug", getHandler(consts.WebappType))
	router.POST("/:slug", installHandler(consts.WebappType))
	router.PUT("/:slug", updateHandler(consts.WebappType))
	router.DELETE("/:slug", deleteHandler(consts.WebappType))
	router.GET("/:slug/icon", iconHandler(consts.WebappType))
	router.GET("/:slug/icon/:version", iconHandler(consts.WebappType))
	router.GET("/:slug/open", openWebapp)
	router.GET("/:slug/download", downloadHandler(consts.WebappType))
	router.GET("/:slug/download/:version", downloadHandler(consts.WebappType))
	router.POST("/:slug/logs", logsHandler(consts.WebappType))
}

// KonnectorRoutes sets the routing for the konnectors service
func KonnectorRoutes(router *echo.Group) {
	router.GET("/", listKonnectorsHandler)
	router.GET("/:slug", getHandler(consts.KonnectorType))
	router.POST("/:slug", installHandler(consts.KonnectorType))
	router.PUT("/:slug", updateHandler(consts.KonnectorType))
	router.DELETE("/:slug", deleteHandler(consts.KonnectorType))
	router.GET("/:slug/icon", iconHandler(consts.KonnectorType))
	router.GET("/:slug/icon/:version", iconHandler(consts.KonnectorType))
	router.POST("/:slug/trigger", createTrigger)
	router.GET("/:slug/download", downloadHandler(consts.KonnectorType))
	router.GET("/:slug/download/:version", downloadHandler(consts.KonnectorType))
	router.POST("/:slug/logs", logsHandler(consts.KonnectorType))
}

func wrapAppsError(err error) error {
	switch err {
	case app.ErrInvalidSlugName:
		return jsonapi.InvalidParameter("slug", err)
	case app.ErrAlreadyExists:
		return jsonapi.Conflict(err)
	case app.ErrNotFound:
		return jsonapi.NotFound(err)
	case app.ErrNotSupportedSource:
		return jsonapi.InvalidParameter("Source", err)
	case app.ErrManifestNotReachable:
		return jsonapi.NotFound(err)
	case app.ErrSourceNotReachable:
		return jsonapi.BadRequest(err)
	case app.ErrBadManifest:
		return jsonapi.BadRequest(err)
	case app.ErrMissingSource:
		return jsonapi.BadRequest(err)
	case app.ErrLinkedAppExists:
		return jsonapi.BadRequest(err)
	case limits.ErrRateLimitReached,
		limits.ErrRateLimitExceeded:
		return jsonapi.BadRequest(err)
	}
	if _, ok := err.(*url.Error); ok {
		return jsonapi.InvalidParameter("Source", err)
	}
	return err
}
