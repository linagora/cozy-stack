package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
)

// ErrWrongPassphrase is used when the passphrase is wrong
var ErrWrongPassphrase = errors.New("Unauthorized: wrong passphrase")

// jsonAPIErrors is a group of errors. It is the error type returned by the
// API.
type jsonAPIErrors struct {
	Errors []*request.Error `json:"errors"`
}

// jsonAPIDocument is a simple JSONAPI document used to un-serialized
type jsonAPIDocument struct {
	Data     *json.RawMessage `json:"data"`
	Included *json.RawMessage `json:"included"`
	Links    *json.RawMessage `json:"links"`
}

// Client encapsulates the element representing a typical connection to the
// HTTP api of the cozy-stack.
//
// It holds the elements to authenticate a user, as well as the transport layer
// used for all the calls to the stack.
type Client struct {
	Addr   string
	Domain string
	Scheme string
	Client *http.Client

	AuthClient  *auth.Client
	AuthScopes  []string
	AuthAccept  auth.UserAcceptFunc
	AuthStorage auth.Storage
	Authorizer  request.Authorizer

	UserAgent string
	Retries   int
	Transport http.RoundTripper

	authed bool
	inited bool
	initMu sync.Mutex
	authMu sync.Mutex
	auth   *auth.Request

	// driveID, when set (see InDrive), routes file operations through the
	// /sharings/drives/<id> endpoints instead of /files.
	driveID string
}

func (c *Client) init() {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.inited {
		return
	}
	if c.Retries == 0 {
		c.Retries = 3
	}
	if c.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyFromEnvironment
		c.Transport = transport
	}
	if c.AuthStorage == nil {
		c.AuthStorage = auth.NewFileStorage()
	}
	if c.Client == nil {
		c.Client = &http.Client{
			Transport: c.Transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	c.inited = true
}

// Authenticate is used to authenticate a client via OAuth.
func (c *Client) Authenticate() (request.Authorizer, error) {
	c.authMu.Lock()
	defer c.authMu.Unlock()
	if c.authed {
		return c.auth, nil
	}
	if c.auth == nil {
		c.auth = &auth.Request{
			ClientParams: c.AuthClient,
			Scopes:       c.AuthScopes,
			Domain:       c.Domain,
			Scheme:       c.Scheme,
			HTTPClient:   c.Client,
			UserAgent:    c.UserAgent,
			UserAccept:   c.AuthAccept,
			Storage:      c.AuthStorage,
		}
	}
	if err := c.auth.Authenticate(); err != nil {
		return nil, err
	}
	c.authed = true
	return c.auth, nil
}

// Req is used to perform a request to the stack given the ReqOptions passed.
func (c *Client) Req(opts *request.Options) (*http.Response, error) {
	c.init()
	var err error
	if c.Authorizer != nil {
		opts.Authorizer = c.Authorizer
	} else {
		opts.Authorizer, err = c.Authenticate()
	}
	if err != nil {
		return nil, err
	}
	opts.Addr = c.Addr
	if opts.Domain == "" {
		opts.Domain = c.Domain
	}
	opts.Scheme = c.Scheme
	opts.Client = c.Client
	opts.UserAgent = c.UserAgent
	opts.ParseError = parseJSONAPIError
	return request.Req(opts)
}

func parseJSONAPIError(res *http.Response, b []byte) error {
	var errs jsonAPIErrors
	if err := json.Unmarshal(b, &errs); err != nil || errs.Errors == nil || len(errs.Errors) == 0 {
		return &request.Error{
			Status: http.StatusText(res.StatusCode),
			Title:  http.StatusText(res.StatusCode),
			Detail: string(b),
		}
	}
	return errs.Errors[0]
}

func readJSONAPI(r io.Reader, data interface{}) (err error) {
	defer func() {
		if rc, ok := r.(io.ReadCloser); ok {
			if cerr := rc.Close(); err == nil && cerr != nil {
				err = cerr
			}
		}
	}()
	var doc jsonAPIDocument
	decoder := json.NewDecoder(r)
	if err = decoder.Decode(&doc); err != nil {
		return err
	}
	if data != nil {
		return json.Unmarshal(*doc.Data, &data)
	}
	return nil
}

func readJSONAPILinks(r io.Reader, included, links interface{}) (err error) {
	defer func() {
		if rc, ok := r.(io.ReadCloser); ok {
			if cerr := rc.Close(); err == nil && cerr != nil {
				err = cerr
			}
		}
	}()
	var doc jsonAPIDocument
	decoder := json.NewDecoder(r)
	if err = decoder.Decode(&doc); err != nil {
		return err
	}
	if included != nil && doc.Included != nil {
		if err = json.Unmarshal(*doc.Included, &included); err != nil {
			return err
		}
	}
	if links != nil && doc.Links != nil {
		if err = json.Unmarshal(*doc.Links, &links); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONAPI(data interface{}) (io.Reader, error) {
	buf, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	doc := jsonAPIDocument{
		Data: (*json.RawMessage)(&buf),
	}
	buf, err = json.Marshal(doc)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(buf), nil
}
