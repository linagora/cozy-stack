package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/filetype"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/cozy/httpcache"
	"github.com/labstack/echo/v4"
)

var (
	// ErrNotFoundRemote is used when no request is defined for a doctype
	ErrNotFoundRemote = errors.New("the doctype has no request defined")
	// ErrInvalidRequest is used when we can't use the request defined by the
	// developer
	ErrInvalidRequest = errors.New("the request is not valid")
	// ErrRequestFailed is used when the connexion to the remote website can't
	// be established
	ErrRequestFailed = errors.New("can't connect to the remote host")
	// ErrInvalidVariables is used when the variables can't be extracted from
	// the request
	ErrInvalidVariables = errors.New("the variables are not valid")
	// ErrMissingVar is used when trying to use a variable that has not been defined
	ErrMissingVar = errors.New("a variable is used in the template, but no value was given")
	// ErrInvalidContentType is used when the response has a content-type that
	// we deny for security reasons
	ErrInvalidContentType = errors.New("the content-type for the response is not authorized")
	// ErrRemoteAssetNotFound is used when the wanted remote asset is not part of
	// our defined list.
	ErrRemoteAssetNotFound = errors.New("wanted remote asset is not part of our asset list")
)

const rawURL = "https://raw.githubusercontent.com/cozy/cozy-doctypes/master/%s/request"

var remoteClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: safehttp.DefaultClient.Transport,
}

var assetsClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: httpcache.NewMemoryCacheTransport(32),
}

// Doctype is used to describe a doctype, its request for a remote doctype for example
type Doctype struct {
	DocID     string    `json:"_id,omitempty"`
	DocRev    string    `json:"_rev,omitempty"`
	Request   string    `json:"request"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ID is used to implement the couchdb.Doc interface
func (d *Doctype) ID() string { return d.DocID }

// Rev is used to implement the couchdb.Doc interface
func (d *Doctype) Rev() string { return d.DocRev }

// SetID is used to implement the couchdb.Doc interface
func (d *Doctype) SetID(id string) { d.DocID = id }

// SetRev is used to implement the couchdb.Doc interface
func (d *Doctype) SetRev(rev string) { d.DocRev = rev }

// DocType implements couchdb.Doc
func (d *Doctype) DocType() string { return consts.Doctypes }

// Clone implements couchdb.Doc
func (d *Doctype) Clone() couchdb.Doc { cloned := *d; return &cloned }

// Request is used to log in couchdb a call to a remote website
type Request struct {
	DocID         string            `json:"_id,omitempty"`
	DocRev        string            `json:"_rev,omitempty"`
	RemoteDoctype string            `json:"doctype"`
	Verb          string            `json:"verb"`
	URL           string            `json:"url"`
	ResponseCode  int               `json:"response_code"`
	ContentType   string            `json:"content_type"`
	Variables     map[string]string `json:"variables"`
	CreatedAt     time.Time         `json:"created_at"`
	CozyMetadata  CozyMetadata      `json:"cozyMetadata"`
}

type CozyMetadata struct {
	CreatedByApp string `json:"createdByApp,omitempty"`
}

// ID is used to implement the couchdb.Doc interface
func (r *Request) ID() string { return r.DocID }

// Rev is used to implement the couchdb.Doc interface
func (r *Request) Rev() string { return r.DocRev }

// SetID is used to implement the couchdb.Doc interface
func (r *Request) SetID(id string) { r.DocID = id }

// SetRev is used to implement the couchdb.Doc interface
func (r *Request) SetRev(rev string) { r.DocRev = rev }

// DocType implements couchdb.Doc
func (r *Request) DocType() string { return consts.RemoteRequests }

// Clone implements couchdb.Doc
func (r *Request) Clone() couchdb.Doc {
	cloned := *r
	cloned.Variables = make(map[string]string)
	for k, v := range r.Variables {
		cloned.Variables[k] = v
	}
	return &cloned
}

// Remote is the struct used to call a remote website for a doctype
type Remote struct {
	Doctype string
	Verb    string
	URL     *url.URL
	Headers map[string]string
	Body    string
}

var log = logger.WithNamespace("remote")

// ParseRawRequest takes a string and parse it as a remote struct.
// First line is verb and URL.
// Then, we have the headers.
// And for a POST, we have a blank line, and then the body.
func ParseRawRequest(doctype, raw string) (*Remote, error) {
	lines := strings.Split(raw, "\n")
	parts := strings.SplitN(lines[0], " ", 2)
	if len(parts) != 2 {
		log.Infof("%s cannot be used as a remote doctype", doctype)
		return nil, ErrInvalidRequest
	}
	remote := Remote{Doctype: doctype}
	remote.Verb = parts[0]
	if remote.Verb != echo.GET && remote.Verb != echo.POST {
		log.Infof("Invalid verb for remote doctype %s: %s", doctype, remote.Verb)
		return nil, ErrInvalidRequest
	}
	u, err := url.Parse(parts[1])
	if err != nil {
		log.Infof("Invalid URL for remote doctype %s: %s", doctype, parts[1])
		return nil, ErrInvalidRequest
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		log.Infof("Invalid scheme for remote doctype %s: %s", doctype, u.Scheme)
		return nil, ErrInvalidRequest
	}
	remote.URL = u
	remote.Headers = make(map[string]string)
	for i, line := range lines[1:] {
		if line == "" {
			if remote.Verb == echo.GET {
				continue
			}
			remote.Body = strings.Join(lines[i+2:], "\n")
			break
		}
		parts = strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			log.Infof("Invalid header for remote doctype %s: %s", doctype, line)
			return nil, ErrInvalidRequest
		}
		remote.Headers[parts[0]] = strings.TrimSpace(parts[1])
	}
	return &remote, nil
}

func lockDoctype(inst *instance.Instance, docID string) func() {
	mu := config.Lock().ReadWrite(inst, docID)
	_ = mu.Lock()
	return mu.Unlock
}

// Find finds the request defined for the given doctype
func Find(ins *instance.Instance, doctype string) (*Remote, error) {
	var raw string

	if config.GetConfig().Doctypes == "" {
		dt := Doctype{
			DocID: consts.Doctypes + "/" + doctype,
		}
		defer lockDoctype(ins, dt.DocID)()
		err := couchdb.GetDoc(ins, consts.Doctypes, dt.DocID, &dt)
		if err != nil || dt.UpdatedAt.Add(24*time.Hour).Before(time.Now()) {
			rev := dt.Rev()
			u := fmt.Sprintf(rawURL, doctype)
			req, err := http.NewRequest(http.MethodGet, u, nil)
			if err != nil {
				return nil, err
			}
			log.Debugf("Fetch remote doctype from %s\n", doctype)
			res, err := remoteClient.Do(req)
			if err != nil {
				log.Infof("Request not found for remote doctype %s: %s", doctype, err)
				return nil, ErrNotFoundRemote
			}
			defer res.Body.Close()
			b, err := io.ReadAll(res.Body)
			if err != nil {
				log.Infof("Request not found for remote doctype %s: %s", doctype, err)
				return nil, ErrNotFoundRemote
			}
			dt.Request = string(b)
			dt.UpdatedAt = time.Now()
			if rev == "" {
				err = couchdb.CreateNamedDocWithDB(ins, &dt)
			} else {
				dt.SetRev(rev)
				err = couchdb.UpdateDoc(ins, &dt)
			}
			if err != nil {
				log.Infof("Cannot save remote doctype %s: %s", doctype, err)
			}
		}
		raw = dt.Request
	} else {
		filename := path.Join(config.GetConfig().Doctypes, doctype, "request")
		bytes, err := os.ReadFile(filename)
		if err != nil {
			log.Infof("Cannot read file %s: %s", filename, err)
			return nil, ErrNotFoundRemote
		}
		raw = string(bytes)
	}

	return ParseRawRequest(doctype, raw)
}

// extractVariables extracts the variables:
// - from the query string for a GET
// - from the body formatted as JSON for a POST
func extractVariables(verb string, in *http.Request) (map[string]string, error) {
	vars := make(map[string]string)
	if verb == echo.GET {
		for k, v := range in.URL.Query() {
			vars[k] = v[0]
		}
	} else {
		err := json.NewDecoder(in.Body).Decode(&vars)
		if err != nil {
			return nil, err
		}
	}
	return vars, nil
}

func findSecret(doctype, secretName string) (string, bool) {
	var doc couchdb.JSONDoc
	err := couchdb.GetDoc(prefixer.SecretsPrefixer, consts.RemoteSecrets, doctype, &doc)
	if err != nil {
		return "", false
	}
	secret, ok := doc.M[secretName].(string)
	return secret, ok
}

var injectionRegexp = regexp.MustCompile(`{{[0-9A-Za-z_ ]+}}`)

func injectVar(src string, vars map[string]string, defautFunc, doctype string) (string, error) {
	var err error
	result := injectionRegexp.ReplaceAllStringFunc(src, func(m string) string {
		m = strings.TrimSpace(m[2 : len(m)-2])

		var funname string
		var varname string
		if defautFunc == "" {
			ms := strings.SplitN(m, " ", 2)
			if len(ms) == 1 {
				varname = ms[0]
			} else {
				funname = ms[0]
				varname = ms[1]
			}
		} else {
			varname = m
			funname = defautFunc
		}

		val, ok := vars[varname]
		if !ok && strings.HasPrefix(varname, "secret_") {
			val, ok = findSecret(doctype, strings.TrimPrefix(varname, "secret_"))
		}
		if !ok {
			err = ErrMissingVar
			return ""
		}

		switch funname {
		case "":
			return val
		case "query":
			return url.QueryEscape(val)
		case "path":
			return url.PathEscape(val)
		case "header":
			return strings.ReplaceAll(val, "\n", "\\n")
		case "json":
			var b []byte
			b, err = json.Marshal(val)
			if err != nil {
				return ""
			}
			return string(b[1 : len(b)-1])
		case "html":
			return html.EscapeString(val)
		default:
			err = fmt.Errorf("remote: unknown template function %s", funname)
			return ""
		}
	})
	return result, err
}

// injectVariables replaces {{variable}} by its value in some fields of the
// remote struct
func injectVariables(remote *Remote, vars map[string]string) error {
	var err error
	if strings.Contains(remote.URL.Path, "{{") {
		remote.URL.Path, err = injectVar(remote.URL.Path, vars, "path", remote.Doctype)
		if err != nil {
			return err
		}
	}
	if strings.Contains(remote.URL.RawQuery, "{{") {
		remote.URL.RawQuery, err = injectVar(remote.URL.RawQuery, vars, "query", remote.Doctype)
		if err != nil {
			return err
		}
	}
	for k, v := range remote.Headers {
		if strings.Contains(v, "{{") {
			remote.Headers[k], err = injectVar(v, vars, "header", remote.Doctype)
			if err != nil {
				return err
			}
		}
	}
	if strings.Contains(remote.Body, "{{") {
		remote.Body, err = injectVar(remote.Body, vars, "", remote.Doctype)
	}
	return err
}

// ProxyTo calls the external website and proxy the response
func (remote *Remote) ProxyTo(
	ins *instance.Instance,
	rw http.ResponseWriter,
	in *http.Request,
	slug string,
) error {
	vars, err := extractVariables(remote.Verb, in)
	if err != nil {
		log.Infof("Error on extracting variables: %s", err)
		return ErrInvalidVariables
	}
	if err = injectVariables(remote, vars); err != nil {
		return err
	}

	// Sanitize the remote URL
	if !config.GetConfig().RemoteAllowCustomPort {
		if strings.Contains(remote.URL.Host, ":") {
			log.Infof("Invalid host for remote doctype %s: %s", remote.Doctype, remote.URL.Host)
			return ErrInvalidRequest
		}
	}
	remote.URL.User = nil
	remote.URL.Fragment = ""

	var body io.Reader
	if remote.Verb != "GET" && remote.Verb != "DELETE" {
		body = strings.NewReader(remote.Body)
	}
	req, err := http.NewRequest(remote.Verb, remote.URL.String(), body)
	if err != nil {
		return ErrInvalidRequest
	}

	req.Header.Set("User-Agent", build.UserAgent())
	for k, v := range remote.Headers {
		req.Header.Set(k, v)
	}

	res, err := remoteClient.Do(req)
	if err != nil {
		log.Infof("Error on request %s: %s", remote.URL.String(), err)
		return ErrRequestFailed
	}
	defer res.Body.Close()

	ctype, _, err := mime.ParseMediaType(res.Header.Get(echo.HeaderContentType))
	if err != nil {
		log.Infof("request %s has an invalid content-type", remote.URL.String())
		return ErrInvalidContentType
	}
	if ctype != "application/json" &&
		ctype != "text/xml" &&
		ctype != "text/plain" &&
		ctype != "application/xml" &&
		ctype != "application/vnd.api+json" &&
		ctype != "application/sparql-results+json" {
		class := strings.SplitN(ctype, "/", 2)[0]
		if class != "image" && class != "audio" && class != "video" {
			log.Infof("request %s has a content-type that is not allowed: %s",
				remote.URL.String(), ctype)
			return ErrInvalidContentType
		}
	}

	logged := &Request{
		RemoteDoctype: remote.Doctype,
		Verb:          remote.Verb,
		URL:           remote.URL.String(),
		ResponseCode:  res.StatusCode,
		ContentType:   ctype,
		Variables:     vars,
		CreatedAt:     time.Now(),
		CozyMetadata:  CozyMetadata{CreatedByApp: slug},
	}
	err = couchdb.CreateDoc(ins, logged)
	if err != nil {
		log.Errorf("Can't save remote request: %s", err)
	}
	log.Debugf("Remote request: %#v\n", logged)

	copyHeader(rw.Header(), res.Header)
	rw.WriteHeader(res.StatusCode)
	_, err = io.Copy(rw, res.Body)
	if err != nil {
		log.Infof("Error on copying response from %s: %s", remote.URL.String(), err)
	}
	return nil
}

// ProxyRemoteAsset proxy the given http request to fetch an asset from our
// list of available asset list.
func ProxyRemoteAsset(name string, w http.ResponseWriter) error {
	assetURL, ok := config.GetConfig().RemoteAssets[name]
	if !ok {
		return ErrRemoteAssetNotFound
	}

	if build.IsDevRelease() && strings.HasPrefix(assetURL, "file:") {
		return serveLocalRemoteAsset(assetURL, w)
	}

	req, err := http.NewRequest(http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", build.UserAgent())

	res, err := assetsClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	copyHeader(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)

	_, err = io.Copy(w, res.Body)
	return err
}

func serveLocalRemoteAsset(assetURL string, w http.ResponseWriter) error {
	u, err := url.Parse(assetURL)
	if err != nil {
		return err
	}

	f, err := os.Open(u.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	ext := path.Ext(u.Path)
	mimetype := filetype.ByExtension(ext)
	w.Header().Set(echo.HeaderContentType, mimetype)
	w.WriteHeader(http.StatusOK)

	_, err = io.Copy(w, f)
	return err
}

var doNotCopyHeaders = []string{
	echo.HeaderSetCookie,
	echo.HeaderAccessControlAllowOrigin,
	echo.HeaderAccessControlAllowMethods,
	echo.HeaderAccessControlAllowCredentials,
	echo.HeaderAccessControlAllowHeaders,
	echo.HeaderAccessControlMaxAge,
	echo.HeaderContentSecurityPolicy,
	echo.HeaderContentSecurityPolicyReportOnly,
	echo.HeaderStrictTransportSecurity,
	echo.HeaderXFrameOptions,
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		copy := true
		for _, h := range doNotCopyHeaders {
			if k == h {
				copy = false
				break
			}
		}
		if copy {
			for _, v := range vv {
				dst.Add(k, v)
			}
		}
	}
}

var (
	_ couchdb.Doc = (*Doctype)(nil)
	_ couchdb.Doc = (*Request)(nil)
)
