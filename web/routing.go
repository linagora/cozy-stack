//go:generate statik -f -src=../assets -dest=. -externals=../assets/.externals

package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/stack"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/metrics"
	"github.com/cozy/cozy-stack/web/accounts"
	"github.com/cozy/cozy-stack/web/ai"
	"github.com/cozy/cozy-stack/web/apps"
	"github.com/cozy/cozy-stack/web/auth"
	"github.com/cozy/cozy-stack/web/bitwarden"
	"github.com/cozy/cozy-stack/web/compat"
	"github.com/cozy/cozy-stack/web/conncheck"
	"github.com/cozy/cozy-stack/web/contacts"
	"github.com/cozy/cozy-stack/web/data"
	"github.com/cozy/cozy-stack/web/editor"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/instances"
	"github.com/cozy/cozy-stack/web/intents"
	"github.com/cozy/cozy-stack/web/jobs"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/cozy/cozy-stack/web/move"
	"github.com/cozy/cozy-stack/web/notes"
	"github.com/cozy/cozy-stack/web/notifications"
	"github.com/cozy/cozy-stack/web/oauth"
	"github.com/cozy/cozy-stack/web/office"
	"github.com/cozy/cozy-stack/web/oidc"
	"github.com/cozy/cozy-stack/web/permissions"
	"github.com/cozy/cozy-stack/web/public"
	"github.com/cozy/cozy-stack/web/realtime"
	"github.com/cozy/cozy-stack/web/registry"
	"github.com/cozy/cozy-stack/web/remote"
	"github.com/cozy/cozy-stack/web/settings"
	"github.com/cozy/cozy-stack/web/sharings"
	"github.com/cozy/cozy-stack/web/shortcuts"
	"github.com/cozy/cozy-stack/web/statik"
	"github.com/cozy/cozy-stack/web/status"
	"github.com/cozy/cozy-stack/web/swift"
	"github.com/cozy/cozy-stack/web/tools"
	"github.com/cozy/cozy-stack/web/version"
	"github.com/cozy/cozy-stack/web/wellknown"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/idna"
)

const (
	// cspScriptSrcAllowList is an allowlist for default allowed domains in CSP.
	cspScriptSrcAllowList = "https://matomo.cozycloud.cc https://errors.cozycloud.cc https://api.pwnedpasswords.com"

	// cspImgSrcAllowList is an allowlist of images domains that are allowed in
	// CSP.
	cspImgSrcAllowList = "https://matomo.cozycloud.cc https://*.tile.openstreetmap.org https://*.tile.osm.org"

	// cspFrameSrcAllowList is an allowlist of custom protocols that are allowed
	// in the CSP. We are using iframes on these custom protocols to open
	// deeplinks to them and have a fallback if the mobile apps are not
	// available.
	cspFrameSrcAllowList = "cozydrive: cozybanks:"
)

var hstsMaxAge = 365 * 24 * time.Hour // 1 year

// SetupAppsHandler adds all the necessary middlewares for the application
// handler.
func SetupAppsHandler(appsHandler echo.HandlerFunc) echo.HandlerFunc {
	mws := []echo.MiddlewareFunc{
		middlewares.LoadSession,
		middlewares.CheckUserAgent,
		middlewares.Accept(middlewares.AcceptOptions{
			DefaultContentTypeOffer: echo.MIMETextHTML,
		}),
		middlewares.CheckInstanceBlocked,
		middlewares.CheckInstanceDeleting,
		middlewares.CheckTOSDeadlineExpired,
	}

	if !config.GetConfig().CSPDisabled {
		// Add CSP exceptions for loading the OnlyOffice editor (script + frame)
		perContext := config.GetConfig().CSPPerContext
		scriptSrc := cspScriptSrcAllowList
		frameSrc := cspFrameSrcAllowList
		for ctxName, office := range config.GetConfig().Office {
			oo := office.OnlyOfficeURL
			if oo == "" {
				continue
			}
			if !strings.HasSuffix(oo, "/") {
				oo += "/"
			}
			if ctxName == config.DefaultInstanceContext {
				scriptSrc = oo + " " + scriptSrc
				frameSrc = oo + " " + frameSrc
			} else {
				cfg := perContext[ctxName]
				if cfg == nil {
					cfg = make(map[string]string)
				}
				cfg["script"] = oo + " " + cfg["script"]
				cfg["frame"] = oo + " " + cfg["frame"]
				perContext[ctxName] = cfg
			}
		}

		// Add CSO exception for starting a move from settings
		formAction := config.GetConfig().Move.URL

		secure := middlewares.Secure(&middlewares.SecureConfig{
			HSTSMaxAge:        hstsMaxAge,
			CSPDefaultSrc:     []middlewares.CSPSource{middlewares.CSPSrcSelf, middlewares.CSPSrcParent, middlewares.CSPSrcWS},
			CSPScriptSrc:      []middlewares.CSPSource{middlewares.CSPWasmUnsafeEval},
			CSPConnectSrc:     []middlewares.CSPSource{middlewares.CSPSrcBlob},
			CSPStyleSrc:       []middlewares.CSPSource{middlewares.CSPUnsafeInline},
			CSPFontSrc:        []middlewares.CSPSource{middlewares.CSPSrcData},
			CSPImgSrc:         []middlewares.CSPSource{middlewares.CSPSrcData, middlewares.CSPSrcBlob},
			CSPObjectSrc:      []middlewares.CSPSource{middlewares.CSPSrcNone},
			CSPFrameSrc:       []middlewares.CSPSource{middlewares.CSPSrcSiblings},
			CSPWorkerSrc:      []middlewares.CSPSource{middlewares.CSPSrcSelf, middlewares.CSPSrcBlob},
			CSPFrameAncestors: []middlewares.CSPSource{middlewares.CSPSrcSelf},
			CSPBaseURI:        []middlewares.CSPSource{middlewares.CSPSrcSelf},
			CSPFormAction:     []middlewares.CSPSource{middlewares.CSPSrcParent},

			CSPDefaultSrcAllowList: config.GetConfig().CSPAllowList["default"],
			CSPImgSrcAllowList:     config.GetConfig().CSPAllowList["img"] + " " + cspImgSrcAllowList,
			CSPScriptSrcAllowList:  config.GetConfig().CSPAllowList["script"] + " " + scriptSrc,
			CSPConnectSrcAllowList: config.GetConfig().CSPAllowList["connect"] + " " + cspScriptSrcAllowList,
			CSPStyleSrcAllowList:   config.GetConfig().CSPAllowList["style"],
			CSPFontSrcAllowList:    config.GetConfig().CSPAllowList["font"],
			CSPMediaSrcAllowList:   config.GetConfig().CSPAllowList["media"],
			CSPFrameSrcAllowList:   config.GetConfig().CSPAllowList["frame"] + " " + frameSrc,
			CSPFormActionAllowList: config.GetConfig().CSPAllowList["form"] + " " + formAction,

			CSPPerContext: perContext,
		})
		mws = append([]echo.MiddlewareFunc{secure}, mws...)
	}

	return middlewares.Compose(appsHandler, mws...)
}

// SetupAssets add assets routing and handling to the given router. It also
// adds a Renderer to render templates.
func SetupAssets(router *echo.Echo, assetsPath string) (err error) {
	var r statik.AssetRenderer
	if assetsPath != "" {
		r, err = statik.NewDirRenderer(assetsPath)
	} else {
		r, err = statik.NewRenderer()
	}
	if err != nil {
		return err
	}
	middlewares.BuildTemplates()
	apps.BuildTemplates()

	router.Renderer = r
	router.HEAD("/assets/*", echo.WrapHandler(r))
	router.GET("/assets/*", echo.WrapHandler(r))
	router.GET("/favicon.ico", echo.WrapHandler(r))
	router.GET("/robots.txt", echo.WrapHandler(r))
	router.GET("/security.txt", echo.WrapHandler(r))
	return nil
}

// SetupRoutes sets the routing for HTTP endpoints
func SetupRoutes(router *echo.Echo, services *stack.Services) error {
	router.Use(timersMiddleware)

	if !config.GetConfig().CSPDisabled {
		secure := middlewares.Secure(&middlewares.SecureConfig{
			HSTSMaxAge:        hstsMaxAge,
			CSPDefaultSrc:     []middlewares.CSPSource{middlewares.CSPSrcNone},
			CSPFrameSrc:       []middlewares.CSPSource{middlewares.CSPSrcNone},
			CSPFrameAncestors: []middlewares.CSPSource{middlewares.CSPSrcNone},
			CSPBaseURI:        []middlewares.CSPSource{middlewares.CSPSrcNone},
		})
		router.Use(secure)
	}

	router.Use(middlewares.CORS(middlewares.CORSOptions{
		BlockList: []string{"/auth/"},
	}))

	// non-authentified HTML routes for authentication (login, OAuth, ...)
	{
		mws := []echo.MiddlewareFunc{
			middlewares.NeedInstance,
			middlewares.LoadSession,
			middlewares.Accept(middlewares.AcceptOptions{
				DefaultContentTypeOffer: echo.MIMETextHTML,
			}),
			middlewares.CheckUserAgent,
			middlewares.CheckInstanceBlocked,
			middlewares.CheckInstanceDeleting,
		}

		router.GET("/", auth.Home, mws...)
		auth.Routes(router.Group("/auth", mws...))
		public.Routes(router.Group("/public", mws...))
		wellknown.Routes(router.Group("/.well-known", mws...))
	}

	// authentified JSON API routes
	{
		mwsNotBlocked := []echo.MiddlewareFunc{
			middlewares.NeedInstance,
			middlewares.LoadSession,
			middlewares.Accept(middlewares.AcceptOptions{
				DefaultContentTypeOffer: jsonapi.ContentType,
			}),
		}
		mws := append(mwsNotBlocked,
			middlewares.CheckInstanceBlocked,
			middlewares.CheckTOSDeadlineExpired,
		)
		registry.Routes(router.Group("/registry", mws...))
		data.Routes(router.Group("/data", mws...))
		files.Routes(router.Group("/files", mws...))
		contacts.Routes(router.Group("/contacts", mws...))
		intents.Routes(router.Group("/intents", mws...))
		jobs.NewHTTPHandler(services.Emailer).Register(router.Group("/jobs", mws...))
		notifications.Routes(router.Group("/notifications", mws...))
		move.Routes(router.Group("/move", mws...))
		permissions.Routes(router.Group("/permissions", mws...))
		realtime.Routes(router.Group("/realtime", mws...))
		notes.Routes(router.Group("/notes", mws...))
		office.Routes(router.Group("/office", mws...))
		editor.Routes(router.Group("/editor", mws...))
		remote.NewHTTPHandler(services.RabbitMQ).Register(router.Group("/remote", mws...))
		sharings.Routes(router.Group("/sharings", mws...))
		bitwarden.Routes(router.Group("/bitwarden", mws...))
		shortcuts.Routes(router.Group("/shortcuts", mws...))
		ai.Routes(router.Group("/ai", mws...))

		// The settings routes needs not to be blocked
		apps.WebappsRoutes(router.Group("/apps", mwsNotBlocked...))
		apps.KonnectorRoutes(router.Group("/konnectors", mwsNotBlocked...))

		// TODO: An init refacto will soon be required
		settings.NewHTTPHandler(services.Settings, services.RabbitMQ).Register(router.Group("/settings", mwsNotBlocked...))

		compat.Routes(router.Group("/compat", mwsNotBlocked...))

		// Careful, the normal middlewares NeedInstance and LoadSession are not
		// applied to these groups since they should not be used for oauth
		// redirection.
		accounts.Routes(router.Group("/accounts"))
		oidc.Routes(router.Group("/oidc"))
	}

	// other non-authentified routes
	{
		conncheck.Routes(router.Group("/connection_check"))
		status.Routes(router.Group("/status"))
		version.Routes(router.Group("/version"))
	}

	// dev routes
	if build.IsDevRelease() {
		router.GET("/dev/mails/:name", devMailsHandler, middlewares.NeedInstance)
		router.GET("/dev/templates/:name", devTemplatesHandler)
	}

	setupRecover(router)
	router.HTTPErrorHandler = errors.ErrorHandler
	return nil
}

func timersMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		timer := prometheus.NewTimer(prometheus.ObserverFunc(func(v float64) {
			status := strconv.Itoa(c.Response().Status)
			metrics.HTTPTotalDurations.
				WithLabelValues(c.Request().Method, status).
				Observe(v)
		}))
		defer timer.ObserveDuration()
		return next(c)
	}
}

// SetupAdminRoutes sets the routing for the administration HTTP endpoints
func SetupAdminRoutes(router *echo.Echo) error {
	var mws []echo.MiddlewareFunc
	if build.IsDevRelease() {
		mws = append(mws, middleware.LoggerWithConfig(middleware.LoggerConfig{
			Format: "time=${time_rfc3339}\tstatus=${status}\tmethod=${method}\thost=${host}\turi=${uri}\tbytes_out=${bytes_out}\n",
		}))
	} else {
		mws = append(mws, middlewares.BasicAuth(config.GetConfig().AdminSecretFileName))
	}

	instances.Routes(router.Group("/instances", mws...))
	apps.AdminRoutes(router.Group("/konnectors", mws...))
	version.Routes(router.Group("/version", mws...))
	metrics.Routes(router.Group("/metrics", mws...))
	oauth.Routes(router.Group("/oauth", mws...))
	oidc.AdminRoutes(router.Group("/oidc", mws...))
	realtime.Routes(router.Group("/realtime", mws...))
	swift.Routes(router.Group("/swift", mws...))
	tools.Routes(router.Group("/tools", mws...))
	conncheck.Routes(router.Group("/connection_check", mws...))

	setupRecover(router)

	router.HTTPErrorHandler = errors.ErrorHandler
	return nil
}

// CreateSubdomainProxy returns a new web server that will handle that apps
// proxy routing if the host of the request match an application, and route to
// the given router otherwise.
func CreateSubdomainProxy(router *echo.Echo, services *stack.Services, appsHandler echo.HandlerFunc) (*echo.Echo, error) {
	if err := SetupAssets(router, config.GetConfig().Assets); err != nil {
		return nil, err
	}

	if err := SetupRoutes(router, services); err != nil {
		return nil, err
	}

	appsHandler = SetupAppsHandler(appsHandler)

	main := echo.New()
	main.HideBanner = true
	main.HidePort = true
	main.Renderer = router.Renderer
	main.Any("/*", firstRouting(router, appsHandler))

	main.HTTPErrorHandler = errors.HTMLErrorHandler
	return main, nil
}

// firstRouting receives the requests and use the domain to decide if we should
// use the API router, serve an app, or use delegated authentication.
func firstRouting(router *echo.Echo, appsHandler echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		host, err := idna.ToUnicode(c.Request().Host)
		if err != nil {
			return err
		}
		if contextName, ok := oidc.FindLoginDomain(host); ok {
			return oidc.LoginDomainHandler(c, contextName)
		}

		if parent, slug, _ := config.SplitCozyHost(host); slug != "" {
			i, err := lifecycle.GetInstance(parent)
			if err == nil {
				c.Set("instance", i.WithContextualDomain(parent))
				c.Set("slug", slug)
				return appsHandler(c)
			} else if err == instance.ErrNotFound {
				if oldInstance, err := instance.FindByOldDomain(parent); err == nil {
					newURL := oldInstance.SubDomain(slug)
					newURL.Path = c.Request().URL.Path
					newURL.RawQuery = c.Request().URL.RawQuery
					return c.Redirect(http.StatusPermanentRedirect, newURL.String())
				}
			}
		}

		router.ServeHTTP(c.Response(), c.Request())
		return nil
	}
}

// setupRecover sets a recovering strategy of panics happening in handlers
func setupRecover(router *echo.Echo) {
	if !build.IsDevRelease() {
		recoverMiddleware := middlewares.RecoverWithConfig(middlewares.RecoverConfig{
			StackSize: 10 << 10, // 10KB
		})
		router.Use(recoverMiddleware)
	}
}
