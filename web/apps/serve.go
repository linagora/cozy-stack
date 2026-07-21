package apps

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/intent"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/session"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/registry"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/intents"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/settings"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/labstack/echo/v4"
)

// Serve is an handler for serving files from the VFS for a client-side app
func Serve(c echo.Context) error {
	method := c.Request().Method
	if method != "GET" && method != "HEAD" {
		return echo.NewHTTPError(http.StatusMethodNotAllowed, "Method "+method+" not allowed")
	}

	i := middlewares.GetInstance(c)
	slug := c.Get("slug").(string)

	if !i.OnboardingFinished {
		return c.Redirect(http.StatusFound, i.PageURL("/", nil))
	}

	webapp, err := app.GetWebappBySlug(i, slug)
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			return handleAppNotFound(c, i, slug)
		}
		return err
	}

	route, file := webapp.FindRoute(path.Clean(c.Request().URL.Path))

	if webapp.FromAppsDir {
		// Save permissions in couchdb before loading an index page
		if file == "" && webapp.Permissions() != nil {
			_ = permission.ForceWebapp(i, webapp.Slug(), webapp.Permissions())
		}

		fs := app.FSForAppDir(slug)
		return ServeAppFile(c, i, fs, webapp)
	}

	if file == "" || file == route.Index {
		webapp = app.DoLazyUpdate(i, webapp, app.Copier(consts.WebappType, i), i.Registries()).(*app.WebappManifest)
	}

	switch webapp.State() {
	case app.Installed:
		// This legacy "installed" state is not used anymore with the addition
		// of the registry. Change the webapp state to "ready" and serve the app
		// file.
		webapp.SetState(app.Ready)
		if err := webapp.Update(i, nil); err != nil {
			return err
		}
		fallthrough
	case app.Ready:
		return ServeAppFile(c, i, app.AppsFileServer(i), webapp)
	default:
		return echo.NewHTTPError(http.StatusServiceUnavailable, "Application is not ready")
	}
}

// handleAppNotFound is used to render the error page when the user wants to
// access an app that is not yet installed
func handleAppNotFound(c echo.Context, i *instance.Instance, slug string) error {
	// Used for the "collect" => "home" renaming
	if slug == "collect" {
		return c.Redirect(http.StatusMovedPermanently, i.DefaultRedirection().String())
	}
	// Used for the deprecated "onboarding" app
	if slug == "onboarding" {
		return c.Redirect(http.StatusMovedPermanently, i.DefaultRedirection().String())
	}
	i.Logger().WithNamespace("apps").Infof("App not found: %s", slug)
	if _, err := registry.GetApplication(slug, i.Registries()); err != nil {
		return app.ErrNotFound
	}
	if _, err := app.GetWebappBySlug(i, consts.StoreSlug); err != nil {
		return app.ErrNotFound
	}
	u := i.SubDomain(consts.StoreSlug)
	u.Fragment = "/discover/" + slug
	return c.Redirect(http.StatusTemporaryRedirect, u.String())
}

// handleIntent will allow iframes from another app if the current app is
// opened as an intent. It returns the loaded intent so the caller can inline
// it into the served page. A nil return means the intent could not be used
// in that case the caller simply skips inlining, preserving the previous behavior.
func handleIntent(c echo.Context, i *instance.Instance, slug, intentID string) *intent.Intent {
	intent := &intent.Intent{}
	if err := couchdb.GetDoc(i, consts.Intents, intentID, intent); err != nil {
		return nil
	}
	allowed := false
	for _, service := range intent.Services {
		if slug == service.Slug {
			allowed = true
		}
	}
	if !allowed {
		return nil
	}
	parts := strings.SplitN(intent.Client, "/", 2)
	if len(parts) < 2 || parts[0] != consts.Apps {
		return nil
	}
	from := app.ResolveClientURL(i, parts[1])
	if !config.GetConfig().CSPDisabled {
		middlewares.AppendCSPRule(c, "frame-ancestors", from)
		// When the client app is hosted on a non-cozy domain (via its
		// client_url_flag), it can itself be framed by its cozy subdomain
		// parent. frame-ancestors must list every ancestor in the iframe
		// chain, so also allow the cozy subdomain.
		if defaultFrom := app.DefaultClientURL(i, parts[1]); defaultFrom != from {
			middlewares.AppendCSPRule(c, "frame-ancestors", defaultFrom)
		}
	}
	return intent
}

// ServeAppFile will serve the requested file using the specified application
// manifest and appfs.FileServer context.
//
// It can be used to serve file application in another context than the VFS,
// for instance for tests or development purposes where we want to serve an
// application that is not installed on the user's instance. However this
// procedure should not be used for standard applications, use the Serve method
// for that.
func ServeAppFile(c echo.Context, i *instance.Instance, fs appfs.FileServer, webapp *app.WebappManifest) error {
	slug := webapp.Slug()
	route, file := webapp.FindRoute(path.Clean(c.Request().URL.Path))
	if route.NotFound() {
		return echo.NewHTTPError(http.StatusNotFound, "Page not found")
	}
	if file == "" {
		file = route.Index
	}

	sess, isLoggedIn := middlewares.GetSession(c)
	if code := c.QueryParam("session_code"); code != "" {
		// XXX we should always clear the session code to avoid it being
		// reused, even if the user is already logged in and we don't want to
		// create a new session
		if checked := i.CheckAndClearSessionCode(code); checked && !isLoggedIn {
			sessionID, err := auth.SetCookieForNewSession(c, session.NormalRun, "")
			req := c.Request()
			if err == nil {
				if err = session.StoreNewLoginEntry(i, sessionID, "", req, "session_code"); err != nil {
					i.Logger().Errorf("Could not store session history %q: %s", sessionID, err)
				}
			}
			redirect := req.URL
			redirect.RawQuery = ""
			return c.Redirect(http.StatusSeeOther, redirect.String())
		}
	}

	filepath := path.Join("/", route.Folder, file)
	isRobotsTxt := filepath == "/robots.txt"

	if !route.Public && !isLoggedIn {
		if isRobotsTxt {
			if f, ok := assets.Get("/robots.txt", i.ContextName); ok {
				_, err := io.Copy(c.Response(), f.Reader())
				return err
			}
		}
		if file != route.Index {
			return echo.NewHTTPError(http.StatusUnauthorized, "You must be authenticated")
		}
		reqURL := c.Request().URL
		subdomain := i.SubDomain(slug)
		subdomain.Path = reqURL.Path
		subdomain.RawQuery = reqURL.RawQuery
		subdomain.Fragment = reqURL.Fragment
		params := url.Values{
			"redirect": {subdomain.String()},
		}
		if jwt := c.QueryParam("jwt"); jwt != "" {
			params.Add("jwt", jwt)
		}
		return c.Redirect(http.StatusFound, i.PageURL("/auth/login", params))
	}

	version := webapp.Version()
	shasum := webapp.Checksum()

	if file != route.Index {
		// If file is not the index, it is considered an asset of the application
		// (JS, image, ...). For theses assets we check if it contains an unique
		// identifier to help caching. In such case, a long cache (1 year) is set.
		//
		// A unique identifier is matched when the file base contains a "long"
		// hexadecimal subpart between '.', of at least 10 characters: for instance
		// "app.badf00dbadf00d.js".
		if _, id := statik.ExtractAssetID(file); id != "" {
			c.Response().Header().Set("Cache-Control", "max-age=31536000, immutable")
		}

		err := fs.ServeFileContent(c.Response(), c.Request(), slug, version, shasum, filepath)
		if os.IsNotExist(err) {
			if isRobotsTxt {
				if f, ok := assets.Get("/robots.txt", i.ContextName); ok {
					_, err = io.Copy(c.Response(), f.Reader())
					return err
				}
			}
			return echo.NewHTTPError(http.StatusNotFound, "Asset not found")
		}
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err)
		}

		return nil
	}

	if !isLoggedIn {
		doc, err := i.SettingsDocument()
		if err == nil {
			if to, ok := doc.M["moved_to"].(string); ok && to != "" {
				subdomainType, _ := doc.M["moved_to_subdomain_type"].(string)
				return renderMovedLink(c, i, to, subdomainType)
			}
		}
	}

	// For share by link, show the password page if it is password protected.
	code := c.QueryParam("sharecode")
	token, err := middlewares.TransformShortcodeToJWT(i, code)
	if err == nil {
		claims, err := middlewares.ExtractClaims(c, i, token)
		if err == nil && claims.AudienceString() == consts.ShareAudience {
			pdoc, err := permission.GetForShareCode(i, token)
			if err == nil && pdoc.Password != nil && !middlewares.HasCookieForPassword(c, i, pdoc.ID()) {
				return renderPasswordPage(c, i, pdoc.ID())
			}
		}
	}

	var intentDoc *intent.Intent
	if intentID := c.QueryParam("intent"); intentID != "" {
		intentDoc = handleIntent(c, i, slug, intentID)
	}

	if route.Public && slug == consts.DataProxySlug {
		// Allow to dataproxy to be embedded in a iframe from the login page of the
		// stack for cleaning.
		middlewares.AppendCSPRule(c, "frame-ancestors", i.PageURL("/", nil))
	}

	// For index file, we inject the locale, the stack domain, and a token if the
	// user is connected
	content, err := fs.Open(slug, version, shasum, filepath)
	if err != nil {
		return err
	}
	defer content.Close()

	buf, err := io.ReadAll(content)
	if err != nil {
		return err
	}

	// XXX: Force include Warnings template in all app indexes
	tmplText := string(buf)
	if closeTagIdx := strings.Index(tmplText, "</head>"); closeTagIdx >= 0 {
		tmplText = tmplText[:closeTagIdx] + "\n{{.Warnings}}\n{{.IntentData}}\n" + tmplText[closeTagIdx:]
	} else {
		needsOpenTag := true
		if openTagIdx := strings.Index(tmplText, "<head>"); openTagIdx >= 0 {
			needsOpenTag = false
		}

		if bodyTagIdx := strings.Index(tmplText, "<body>"); bodyTagIdx >= 0 {
			before := tmplText[:bodyTagIdx]
			after := tmplText[bodyTagIdx:]

			tmplText = before

			if needsOpenTag {
				tmplText += "\n<head>"
			}

			tmplText += "\n{{.Warnings}}\n{{.IntentData}}\n</head>\n" + after
		}
	}

	tmpl, err := template.New(file).Parse(tmplText)
	if err != nil {
		i.Logger().WithNamespace("apps").Warnf("%s cannot be parsed as a template: %s", file, err)
		return fs.ServeFileContent(c.Response(), c.Request(), slug, version, shasum, filepath)
	}

	sessID := ""
	if isLoggedIn {
		sessID = sess.ID()

		if file == "" || file == route.Index {
			if !route.Public {
				if handled, err := middlewares.CheckOAuthClientsLimitExceeded(c); handled {
					return err
				}
			}
		}
	}
	params := buildServeParams(c, i, webapp, intentDoc, isLoggedIn, sessID)

	generated := &bytes.Buffer{}
	if err := tmpl.Execute(generated, params); err != nil {
		i.Logger().WithNamespace("apps").Warnf("%s cannot be interpreted as a template: %s", file, err)
		return c.Render(http.StatusInternalServerError, "error.html", echo.Map{
			"Domain":         i.ContextualDomain(),
			"ContextName":    i.ContextName,
			"Locale":         i.Locale,
			"Title":          i.TemplateTitle(),
			"Favicon":        middlewares.Favicon(i),
			"Illustration":   "/images/generic-error.svg",
			"Error":          "Error Application not supported Message",
			"SupportEmail":   i.SupportEmailAddress(),
			"SupportPageURL": i.SupportPageURL(),
		})
	}

	res := c.Response()
	res.Header().Set(echo.HeaderContentType, echo.MIMETextHTMLCharsetUTF8)
	res.Header().Set("Cache-Control", "private, no-store, must-revalidate")
	res.WriteHeader(http.StatusOK)
	_, err = io.Copy(res, generated)
	return err
}

func buildServeParams(
	c echo.Context,
	inst *instance.Instance,
	webapp *app.WebappManifest,
	intentDoc *intent.Intent,
	isLoggedIn bool,
	sessID string,
) serveParams {
	token := getServeToken(c, inst, webapp, isLoggedIn, sessID)
	tracking := false
	settings, err := inst.SettingsDocument()
	if err == nil {
		if t, ok := settings.M["tracking"].(string); ok {
			tracking = t == "true"
		}
	}
	var subdomainsType string
	switch config.GetConfig().Subdomains {
	case config.FlatSubdomains:
		subdomainsType = "flat"
	case config.NestedSubdomains:
		subdomainsType = "nested"
	}

	return serveParams{
		Token:      token,
		SubDomain:  subdomainsType,
		Tracking:   tracking,
		intent:     intentDoc,
		webapp:     webapp,
		instance:   inst,
		isLoggedIn: isLoggedIn,
	}
}

func getServeToken(
	c echo.Context,
	inst *instance.Instance,
	webapp *app.WebappManifest,
	isLoggedIn bool,
	sessID string,
) string {
	sharecode := c.QueryParam("sharecode")
	if sharecode == "" {
		if isLoggedIn {
			return inst.BuildAppToken(webapp.Slug(), sessID)
		}
		return ""
	}

	// XXX The sharecode can be used for share by links, or for Cozy to Cozy
	// sharings. When it is used for a Cozy to Cozy sharing, it can be for a
	// preview token, and we need to upgrade it to an interact token if the
	// member has a known Cozy URL. We do this upgrade when serving the preview
	// page, not when sending the invitation link by mail, because we want the
	// same link to work (and be upgraded) after the user has accepted the
	// sharing.
	token, pdoc, err := permission.GetTokenAndPermissionsFromShortcode(inst, sharecode)
	if err != nil || pdoc.Type != permission.TypeSharePreview {
		return sharecode
	}
	sharingID := strings.Split(pdoc.SourceID, "/")
	sharingDoc, err := sharing.FindSharing(inst, sharingID[1])
	if err != nil || sharingDoc.ReadOnlyRules() {
		return token
	}
	m, err := sharingDoc.FindMemberBySharecode(inst, token)
	if err != nil {
		return token
	}
	if m.Instance != "" && !m.ReadOnly && m.Status != sharing.MemberStatusRevoked {
		memberIndex := 0
		for i := range sharingDoc.Members {
			if sharingDoc.Members[i].Instance == m.Instance {
				memberIndex = i
			}
		}
		interact, err := sharingDoc.GetInteractCode(inst, m, memberIndex)
		if err == nil {
			return interact
		}
	}
	return token
}

func renderMovedLink(c echo.Context, i *instance.Instance, to, subdomainType string) error {
	name, _ := csettings.PublicName(i)
	link := *c.Request().URL
	if u, err := url.Parse(to); err == nil {
		parts := strings.SplitN(c.Request().Host, ".", 2)
		app := parts[0]
		if config.GetConfig().Subdomains == config.FlatSubdomains {
			parts = strings.SplitN(app, "-", 2)
			app = parts[len(parts)-1]
		}
		if subdomainType == "nested" {
			link.Host = app + "." + u.Host
		} else {
			parts := strings.SplitN(u.Host, ".", 2)
			link.Host = parts[0] + "-" + app + "." + parts[1]
		}
		link.Scheme = u.Scheme
	}

	return c.Render(http.StatusGone, "move_link.html", echo.Map{
		"Domain":      i.ContextualDomain(),
		"ContextName": i.ContextName,
		"Locale":      i.Locale,
		"Title":       i.Translate("Move Link Title", name),
		"ThemeCSS":    middlewares.ThemeCSS(i),
		"Favicon":     middlewares.Favicon(i),
		"Link":        link.String(),
	})
}

func renderPasswordPage(c echo.Context, inst *instance.Instance, permID string) error {
	return c.Render(http.StatusUnauthorized, "share_by_link_password.html", echo.Map{
		"Action":      inst.PageURL("/auth/share-by-link/password", nil),
		"Domain":      inst.ContextualDomain(),
		"ContextName": inst.ContextName,
		"Locale":      inst.Locale,
		"Title":       inst.TemplateTitle(),
		"ThemeCSS":    middlewares.ThemeCSS(inst),
		"Favicon":     middlewares.Favicon(inst),
		"PermID":      permID,
	})
}

// serveParams is a struct used for rendering the index.html of webapps. A
// struct is used, and not a map, to have some methods declared on it. It
// allows to be lazy when constructing the paths of the assets: if an asset is
// not used in the template, the method won't be called and the stack can avoid
// checking if this asset is dynamically overridden in this instance context.
type serveParams struct {
	Token      string
	SubDomain  string
	Tracking   bool
	intent     *intent.Intent
	webapp     *app.WebappManifest
	instance   *instance.Instance
	isLoggedIn bool
}

func (s serveParams) CozyData() (string, error) {
	data := map[string]interface{}{
		"token":      s.Token,
		"domain":     s.Domain(),
		"subdomain":  s.SubDomain,
		"tracking":   s.Tracking,
		"locale":     s.Locale(),
		"isLoggedIn": s.isLoggedIn,
		"app": map[string]interface{}{
			"editor": s.AppEditor(),
			"name":   s.AppName(),
			"prefix": s.AppNamePrefix(),
			"slug":   s.AppSlug(),
			"icon":   s.IconPath(),
		},
		"flags":        s.GetFlags(),
		"capabilities": s.GetCapabilities(),
	}
	bytes, err := json.Marshal(data)

	if err != nil {
		return "", err
	}

	return string(bytes), nil
}

func (s serveParams) Domain() string {
	return s.instance.ContextualDomain()
}

func (s serveParams) ContextName() string {
	return s.instance.ContextName
}

func (s serveParams) Locale() string {
	return s.instance.Locale
}

func (s serveParams) AppSlug() string {
	return s.webapp.Slug()
}

func (s serveParams) AppName() string {
	return s.webapp.NameLocalized(s.instance.Locale)
}

func (s serveParams) AppEditor() string {
	return s.webapp.Editor()
}

func (s serveParams) AppNamePrefix() string {
	return s.webapp.NamePrefix()
}

func (s serveParams) IconPath() string {
	return s.webapp.Icon()
}

func (s serveParams) Capabilities() (string, error) {
	bytes, err := json.Marshal(s.GetCapabilities())
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func (s serveParams) GetCapabilities() jsonapi.Object {
	capabilities := settings.NewCapabilities(s.instance)
	capabilities.SetID("")
	return capabilities
}

func (s serveParams) Flags() (string, error) {
	flags, err := feature.GetFlags(s.instance)
	if err != nil {
		return "{}", err
	}
	bytes, err := json.Marshal(flags)
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

func (s serveParams) GetFlags() *feature.Flags {
	flags, err := feature.GetFlags(s.instance)
	if err != nil {
		flags = &feature.Flags{
			M: map[string]interface{}{},
		}
	}
	return flags
}

func (s serveParams) CozyBar() (template.HTML, error) {
	return cozybarHTML(s.instance, s.isLoggedIn)
}

func (s serveParams) CozyClientJS() (template.HTML, error) {
	return cozyclientjsHTML(s.instance)
}

func (s serveParams) CozyFonts() template.HTML {
	return middlewares.CozyFonts(s.instance)
}

func (s serveParams) ThemeCSS() template.HTML {
	return middlewares.ThemeCSS(s.instance)
}

func (s serveParams) Favicon() template.HTML {
	return middlewares.Favicon(s.instance)
}

func (s serveParams) DefaultWallpaper() string {
	return statik.AssetPath(
		s.instance.ContextualDomain(),
		"/images/default-wallpaper.jpg",
		s.instance.ContextName)
}

func (s serveParams) Warnings() (template.HTML, error) {
	return warningsHTML(s.instance, s.isLoggedIn)
}

func (s serveParams) IntentData() (template.HTML, error) {
	if s.intent == nil {
		return "", nil
	}
	api := intents.NewAPIIntent(s.intent, s.instance)
	raw, err := jsonapi.MarshalObject(api)
	if err != nil {
		return "", err
	}
	// jsonapi.MarshalObject uses json.Marshal that escapes <, >, & to \u003c, \u003e, \u0026, so the JSON is
	// safe to embed in a <script> block (no </script> breakout).
	return template.HTML(`<script type="application/json" id="cozy-intent">` + string(raw) + `</script>`), nil
}

var clientTemplate *template.Template
var barTemplate *template.Template
var warningsTemplate *template.Template

// BuildTemplates ensure that cozy-client-js and the bar can be injected in templates
func BuildTemplates() {
	clientTemplate = template.Must(template.New("cozy-client-js").Funcs(middlewares.FuncsMap).Parse(`` +
		`<script src="{{asset .Domain "/js/cozy-client.min.js" .ContextName}}"></script>`,
	))

	barTemplate = template.Must(template.New("cozy-bar").Funcs(middlewares.FuncsMap).Parse(`
<link rel="stylesheet" type="text/css" href="{{asset .Domain "/fonts/fonts.css" .ContextName}}">
<link rel="stylesheet" type="text/css" href="{{asset .Domain "/css/cozy-bar.min.css" .ContextName}}">
<script src="{{asset .Domain "/js/cozy-bar.min.js" .ContextName}}"></script>`,
	))

	warningsTemplate = template.Must(template.New("warnings").Funcs(middlewares.FuncsMap).Parse(`
{{if .LoggedIn}}
{{range .Warnings}}
<meta name="user-action-required" data-title="{{ .Title }}" data-code="{{ .Code }}" data-detail="{{ .Detail }}" {{with .Links}}{{with .Self}}data-links="{{ . }}"{{end}}{{end}} />
{{end}}
{{end}}`,
	))
}

func cozyclientjsHTML(i *instance.Instance) (template.HTML, error) {
	buf := new(bytes.Buffer)
	err := clientTemplate.Execute(buf, echo.Map{
		"Domain":      i.ContextualDomain(),
		"ContextName": i.ContextName,
	})
	if err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func cozybarHTML(i *instance.Instance, loggedIn bool) (template.HTML, error) {
	buf := new(bytes.Buffer)
	err := barTemplate.Execute(buf, echo.Map{
		"Domain":      i.ContextualDomain(),
		"Warnings":    middlewares.ListWarnings(i),
		"ContextName": i.ContextName,
		"LoggedIn":    loggedIn,
	})
	if err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

func warningsHTML(i *instance.Instance, loggedIn bool) (template.HTML, error) {
	buf := new(bytes.Buffer)
	err := warningsTemplate.Execute(buf, echo.Map{
		"Warnings": middlewares.ListWarnings(i),
		"LoggedIn": loggedIn,
	})
	if err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}
