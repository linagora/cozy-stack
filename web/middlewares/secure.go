package middlewares

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/labstack/echo/v4"
	"golang.org/x/net/idna"
)

type (
	// CSPSource type are the different types of CSP headers sources definitions.
	// Each source type defines a different acess policy.
	CSPSource int

	// SecureConfig defines the config for Secure middleware.
	SecureConfig struct {
		HSTSMaxAge time.Duration

		CSPDefaultSrc     []CSPSource
		CSPScriptSrc      []CSPSource
		CSPFrameSrc       []CSPSource
		CSPConnectSrc     []CSPSource
		CSPFontSrc        []CSPSource
		CSPImgSrc         []CSPSource
		CSPManifestSrc    []CSPSource
		CSPMediaSrc       []CSPSource
		CSPObjectSrc      []CSPSource
		CSPStyleSrc       []CSPSource
		CSPWorkerSrc      []CSPSource
		CSPFrameAncestors []CSPSource
		CSPBaseURI        []CSPSource
		CSPFormAction     []CSPSource

		CSPDefaultSrcAllowList     string
		CSPScriptSrcAllowList      string
		CSPFrameSrcAllowList       string
		CSPConnectSrcAllowList     string
		CSPFontSrcAllowList        string
		CSPImgSrcAllowList         string
		CSPManifestSrcAllowList    string
		CSPMediaSrcAllowList       string
		CSPObjectSrcAllowList      string
		CSPStyleSrcAllowList       string
		CSPWorkerSrcAllowList      string
		CSPFrameAncestorsAllowList string
		CSPBaseURIAllowList        string
		CSPFormActionAllowList     string

		// context_name -> source -> allow_list
		CSPPerContext map[string]map[string]string
	}
)

const (
	// CSPSrcSelf is the 'self' option of a CSP source.
	CSPSrcSelf CSPSource = iota
	// CSPSrcNone is the 'none' option. It denies all domains as an eligible
	// source.
	CSPSrcNone
	// CSPSrcData is the 'data:' option of a CSP source.
	CSPSrcData
	// CSPSrcBlob is the 'blob:' option of a CSP source.
	CSPSrcBlob
	// CSPSrcParent adds the parent domain as an eligible CSP source.
	CSPSrcParent
	// CSPSrcWS adds the parent domain eligible for websocket.
	CSPSrcWS
	// CSPSrcSiblings adds all the siblings subdomains as eligibles CSP
	// sources.
	CSPSrcSiblings
	// CSPSrcAny is the '*' option. It allows any domain as an eligible source.
	CSPSrcAny
	// CSPUnsafeInline is the  'unsafe-inline' option. It allows to have inline
	// styles or scripts to be injected in the page.
	CSPUnsafeInline
	// CSPWasmUnsafeEval allows WebAssembly compilation without allowing
	// JavaScript eval-like sinks.
	CSPWasmUnsafeEval
	// CSPAllowList inserts a allowList of domains.
	CSPAllowList
)

// Secure returns a Middlefunc that can be used to define all the necessary
// secure headers. It is configurable with a SecureConfig object.
func Secure(conf *SecureConfig) echo.MiddlewareFunc {
	var hstsHeader string
	if conf.HSTSMaxAge > 0 {
		hstsHeader = fmt.Sprintf("max-age=%.f; includeSubDomains",
			conf.HSTSMaxAge.Seconds())
	}

	conf.CSPDefaultSrc, conf.CSPDefaultSrcAllowList =
		validCSPList(conf.CSPDefaultSrc, conf.CSPDefaultSrc, conf.CSPDefaultSrcAllowList)
	conf.CSPScriptSrc, conf.CSPScriptSrcAllowList =
		validCSPList(conf.CSPScriptSrc, conf.CSPDefaultSrc, conf.CSPScriptSrcAllowList)
	conf.CSPFrameSrc, conf.CSPFrameSrcAllowList =
		validCSPList(conf.CSPFrameSrc, conf.CSPDefaultSrc, conf.CSPFrameSrcAllowList)
	conf.CSPConnectSrc, conf.CSPConnectSrcAllowList =
		validCSPList(conf.CSPConnectSrc, conf.CSPDefaultSrc, conf.CSPConnectSrcAllowList)
	conf.CSPFontSrc, conf.CSPFontSrcAllowList =
		validCSPList(conf.CSPFontSrc, conf.CSPDefaultSrc, conf.CSPFontSrcAllowList)
	conf.CSPImgSrc, conf.CSPImgSrcAllowList =
		validCSPList(conf.CSPImgSrc, conf.CSPDefaultSrc, conf.CSPImgSrcAllowList)
	conf.CSPManifestSrc, conf.CSPManifestSrcAllowList =
		validCSPList(conf.CSPManifestSrc, conf.CSPDefaultSrc, conf.CSPManifestSrcAllowList)
	conf.CSPMediaSrc, conf.CSPMediaSrcAllowList =
		validCSPList(conf.CSPMediaSrc, conf.CSPDefaultSrc, conf.CSPMediaSrcAllowList)
	conf.CSPObjectSrc, conf.CSPObjectSrcAllowList =
		validCSPList(conf.CSPObjectSrc, nil, conf.CSPObjectSrcAllowList)
	conf.CSPStyleSrc, conf.CSPStyleSrcAllowList =
		validCSPList(conf.CSPStyleSrc, conf.CSPDefaultSrc, conf.CSPStyleSrcAllowList)
	conf.CSPWorkerSrc, conf.CSPWorkerSrcAllowList =
		validCSPList(conf.CSPWorkerSrc, nil, conf.CSPWorkerSrcAllowList)
	conf.CSPFormAction, conf.CSPFormActionAllowList =
		validCSPList(conf.CSPFormAction, nil, conf.CSPFormActionAllowList)

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			isSecure := !build.IsDevRelease()
			h := c.Response().Header()
			if isSecure && hstsHeader != "" {
				h.Set(echo.HeaderStrictTransportSecurity, hstsHeader)
			}
			var cspHeader string
			host, err := idna.ToUnicode(c.Request().Host)
			if err != nil {
				return err
			}
			parent, _, siblings := config.SplitCozyHost(host)
			parent, err = idna.ToASCII(parent)
			if err != nil {
				return err
			}
			var contextName string
			var inst *instance.Instance
			inst, ok := GetInstanceSafe(c)
			if ok {
				if conf.CSPPerContext != nil {
					contextName = inst.ContextName
				}
			}
			b := cspBuilder{
				parent:      parent,
				siblings:    siblings,
				isSecure:    isSecure,
				contextName: contextName,
				perContext:  conf.CSPPerContext,
				instance:    inst,
			}
			cspHeader += b.makeCSPHeader("default-src", conf.CSPDefaultSrcAllowList, conf.CSPDefaultSrc)
			cspHeader += b.makeCSPHeader("script-src", conf.CSPScriptSrcAllowList, conf.CSPScriptSrc)
			cspHeader += b.makeCSPHeader("frame-src", conf.CSPFrameSrcAllowList, conf.CSPFrameSrc)
			cspHeader += b.makeCSPHeader("connect-src", conf.CSPConnectSrcAllowList, conf.CSPConnectSrc)
			cspHeader += b.makeCSPHeader("font-src", conf.CSPFontSrcAllowList, conf.CSPFontSrc)
			cspHeader += b.makeCSPHeader("img-src", conf.CSPImgSrcAllowList, conf.CSPImgSrc)
			cspHeader += b.makeCSPHeader("manifest-src", conf.CSPManifestSrcAllowList, conf.CSPManifestSrc)
			cspHeader += b.makeCSPHeader("media-src", conf.CSPMediaSrcAllowList, conf.CSPMediaSrc)
			cspHeader += b.makeCSPHeader("object-src", conf.CSPObjectSrcAllowList, conf.CSPObjectSrc)
			cspHeader += b.makeCSPHeader("style-src", conf.CSPStyleSrcAllowList, conf.CSPStyleSrc)
			cspHeader += b.makeCSPHeader("worker-src", conf.CSPWorkerSrcAllowList, conf.CSPWorkerSrc)
			cspHeader += b.makeCSPHeader("frame-ancestors", conf.CSPFrameAncestorsAllowList, conf.CSPFrameAncestors)
			cspHeader += b.makeCSPHeader("base-uri", conf.CSPBaseURIAllowList, conf.CSPBaseURI)
			cspHeader += b.makeCSPHeader("form-action", conf.CSPFormActionAllowList, conf.CSPFormAction)
			if cspHeader != "" {
				h.Set(echo.HeaderContentSecurityPolicy, cspHeader)
			}
			h.Set(echo.HeaderXContentTypeOptions, "nosniff")
			return next(c)
		}
	}
}

func validCSPList(sources, defaults []CSPSource, allowList string) ([]CSPSource, string) {
	allowListFields := strings.Fields(allowList)
	allowListFilter := allowListFields[:0]
	for _, s := range allowListFields {
		u, err := url.Parse(s)
		if err != nil {
			continue
		}
		if !build.IsDevRelease() {
			if u.Scheme == "ws" {
				u.Scheme = "wss"
			} else if u.Scheme == "http" {
				u.Scheme = "https"
			}
		}
		if u.Path == "" {
			u.Path = "/"
		}
		// For custom links like cozydrive:, we want to allow the whole protocol
		// Same for blob:
		if !strings.HasPrefix(u.Scheme, "cozy") && u.Scheme != "blob" {
			s = u.String()
		}
		allowListFilter = append(allowListFilter, s)
	}

	if len(allowListFilter) > 0 {
		allowList = strings.Join(allowListFilter, " ")
		sources = append(sources, CSPAllowList)
	} else {
		allowList = ""
	}

	if len(sources) == 0 && allowList == "" {
		return nil, ""
	}

	sources = append(sources, defaults...)
	sourcesUnique := sources[:0]
	for _, source := range sources {
		var found bool
		for _, s := range sourcesUnique {
			if s == source {
				found = true
				break
			}
		}
		if !found {
			sourcesUnique = append(sourcesUnique, source)
		}
	}

	return sourcesUnique, allowList
}

type cspBuilder struct {
	parent      string
	siblings    string
	contextName string
	perContext  map[string]map[string]string
	instance    *instance.Instance
	isSecure    bool
}

func (b cspBuilder) makeCSPHeader(header, cspAllowList string, sources []CSPSource) string {
	headers := make([]string, len(sources), len(sources)+1)
	for i, src := range sources {
		switch src {
		case CSPSrcSelf:
			headers[i] = "'self'"
		case CSPSrcNone:
			headers[i] = "'none'"
		case CSPSrcData:
			headers[i] = "data:"
		case CSPSrcBlob:
			headers[i] = "blob:"
		case CSPSrcParent:
			if b.isSecure {
				headers[i] = "https://" + b.parent
			} else {
				headers[i] = "http://" + b.parent
			}
		case CSPSrcWS:
			if b.isSecure {
				headers[i] = "wss://" + b.parent
			} else {
				headers[i] = "ws://" + b.parent
			}
		case CSPSrcSiblings:
			if b.isSecure {
				headers[i] = "https://" + b.siblings
			} else {
				headers[i] = "http://" + b.siblings
			}
		case CSPSrcAny:
			headers[i] = "*"
		case CSPUnsafeInline:
			headers[i] = "'unsafe-inline'"
		case CSPWasmUnsafeEval:
			headers[i] = "'wasm-unsafe-eval'"
		case CSPAllowList:
			headers[i] = cspAllowList
		}
	}
	if b.contextName != "" {
		if context, ok := b.perContext[b.contextName]; ok {
			var src string
			switch header {
			case "default-src":
				src = "default"
			case "img-src":
				src = "img"
			case "script-src":
				src = "script"
			case "connect-src":
				src = "connect"
			case "style-src":
				src = "style"
			case "font-src":
				src = "font"
			case "media-src":
				src = "font"
			case "frame-src":
				src = "frame"
			}
			if list, ok := context[src]; ok && list != "" {
				headers = append(headers, list)
			}
		}
	}
	// Add org related domains to frame-src directive if present (for iframes).
	// OrgDomain is validated to contain only safe characters before being
	// injected into the header.
	if header == "frame-src" && b.instance != nil {
		if isSafeDomain(b.instance.OrgDomain) {
			headers = append(headers, "matrix."+b.instance.OrgDomain)
		}
	}
	// Add org related domains to connect-src directive if present.
	// OrgID, OrgDomain, and domain are all validated before being injected
	// into the header.
	if header == "connect-src" && b.instance != nil && isSafeDomain(b.instance.OrgID) {
		_, domain, found := strings.Cut(b.instance.Domain, ".")
		if found && isSafeDomain(domain) {
			orgDomain := b.instance.OrgID + "." + domain
			headers = append(headers, orgDomain)
			headers = append(headers, "wss://"+orgDomain)
			headers = append(headers, "chat-"+b.instance.OrgID+".tc-apps."+domain)
			headers = append(headers, "api-login-"+b.instance.OrgID+".tc-apps."+domain)
		}
		if isSafeDomain(b.instance.OrgDomain) {
			orgAltDomain := b.instance.OrgID + "." + b.instance.OrgDomain
			headers = append(headers, orgAltDomain)
			headers = append(headers, "wss://"+orgAltDomain)
		}
	}
	// Add org domain to img-src to load sharing recipient avatars.
	if header == "img-src" && b.instance != nil && isSafeDomain(b.instance.OrgID) {
		_, domain, found := strings.Cut(b.instance.Domain, ".")
		if found && isSafeDomain(domain) {
			headers = append(headers, b.instance.OrgID+"."+domain)
		}
		if isSafeDomain(b.instance.OrgDomain) {
			headers = append(headers, b.instance.OrgID+"."+b.instance.OrgDomain)
		}
	}
	if len(headers) == 0 {
		return ""
	}
	return header + " " + strings.Join(headers, " ") + ";"
}

// AppendCSPRule allows to patch inline the CSP headers to add a new rule.
func AppendCSPRule(c echo.Context, ruleType string, appendedValues ...string) {
	currentRules := c.Response().Header().Get(echo.HeaderContentSecurityPolicy)
	newRules := appendCSPRule(currentRules, ruleType, appendedValues...)
	c.Response().Header().Set(echo.HeaderContentSecurityPolicy, newRules)
}

func appendCSPRule(currentRules, ruleType string, appendedValues ...string) (newRules string) {
	ruleIndex := strings.Index(currentRules, ruleType)
	if ruleIndex >= 0 {
		ruleTerminationIndex := strings.Index(currentRules[ruleIndex:], ";")
		if ruleTerminationIndex <= 0 {
			return
		}
		ruleFields := strings.Fields(currentRules[ruleIndex : ruleIndex+ruleTerminationIndex])
		if len(ruleFields) == 2 && ruleFields[1] == "'none'" {
			ruleFields = ruleFields[:1]
		}
		ruleFields = append(ruleFields, appendedValues...)
		newRules = currentRules[:ruleIndex] + strings.Join(ruleFields, " ") +
			currentRules[ruleIndex+ruleTerminationIndex:]
	} else {
		newRules = currentRules + ruleType + " " + strings.Join(appendedValues, " ") + ";"
	}
	return
}

// safeDomainRe matches hostname values safe to embed in a CSP header
// (alphanumeric, hyphens, dots — no spaces, semicolons, or other
// CSP-significant characters that could break or inject new directives).
var safeDomainRe = regexp.MustCompile(`^[a-zA-Z0-9.-]+$`)

// isSafeDomain returns true if s is a non-empty value safe for embedding in a
// CSP header as a hostname or hostname component.
func isSafeDomain(s string) bool {
	return s != "" && safeDomainRe.MatchString(s)
}
