package client

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/client/auth"
	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	weberrors "github.com/cozy/cozy-stack/web/errors"
	webfiles "github.com/cozy/cozy-stack/web/files"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type testAssertReq func(*http.Request)
type testTransport struct {
	assertFn testAssertReq
}

func (t *testTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.assertFn(req)
	return nil, errors.New("ok")
}

func testClient(assertFn testAssertReq) *http.Client {
	return &http.Client{Transport: &testTransport{assertFn}}
}

func TestClientWithOAuth(t *testing.T) {
	c := &Client{
		Domain: "foobar",
		Scheme: "https",
		Client: testClient(func(req *http.Request) {
			header := req.Header
			assert.Equal(t, "", header.Get("Authorization"))
			assert.Equal(t, "application/json", header.Get("Content-Type"))
			assert.Equal(t, "application/json", header.Get("Accept"))
			assert.Equal(t, &url.URL{
				Scheme: "https",
				Host:   "foobar",
				Path:   "/auth/register",
			}, req.URL)
			var v auth.Client
			err := json.NewDecoder(req.Body).Decode(&v)
			assert.NoError(t, err)
			assert.EqualValues(t, v, auth.Client{
				RedirectURIs: []string{"http://redirectto/"},
				ClientName:   "name",
				ClientKind:   "kind",
				ClientURI:    "uri",
				SoftwareID:   "github.com/cozy/cozy-stack",
			})
		}),
		AuthClient: &auth.Client{
			RedirectURIs: []string{"http://redirectto/"},
			ClientName:   "name",
			ClientKind:   "kind",
			ClientURI:    "uri",
		},
	}
	_, err := c.Req(&request.Options{
		Method: "PUT",
		Path:   "/p/a/t/h",
	})
	assert.Error(t, err)
}

func TestClientWithoutOAuth(t *testing.T) {
	type testjson struct {
		Key string `json:"key"`
	}
	c := &Client{
		Domain:     "foobar",
		Scheme:     "https",
		UserAgent:  "user/agent",
		Authorizer: &request.BearerAuthorizer{Token: "token"},
		Client: testClient(func(req *http.Request) {
			header := req.Header
			assert.Equal(t, "Bearer token", header.Get("Authorization"))
			assert.Equal(t, "user/agent", header.Get("User-Agent"))
			assert.Equal(t, "application/json", header.Get("Content-Type"))
			assert.Equal(t, "application/json", header.Get("Accept"))
			assert.Equal(t, &url.URL{
				Scheme:   "https",
				Host:     "foobar",
				Path:     "/p/a/t/h",
				RawQuery: "q=value",
			}, req.URL)
			var v testjson
			err := json.NewDecoder(req.Body).Decode(&v)
			assert.NoError(t, err)
			assert.Equal(t, v.Key, "Value")
		}),
	}

	body, err := request.WriteJSON(&testjson{Key: "Value"})
	assert.NoError(t, err)

	_, err = c.Req(&request.Options{
		Method:  "PUT",
		Path:    "/p/a/t/h",
		Queries: url.Values{"q": {"value"}},
		Headers: request.Headers{
			"Content-Type": "application/json",
			"Accept":       "application/json",
		},
		Body: body,
	})
	assert.Error(t, err)
}

func TestReadInstanceStorageState(t *testing.T) {
	in, err := readInstance(&http.Response{Body: io.NopCloser(strings.NewReader(
		`{"data":{"id":"alice.example","attributes":{"fs_scheme":"s3","blocked":true,"blocking_reason":"MOVING"}}}`,
	))})
	require.NoError(t, err)
	assert.Equal(t, "s3", in.Attrs.FsScheme)
	assert.True(t, in.Attrs.Blocked)
	assert.Equal(t, "MOVING", in.Attrs.BlockingReason)

	output, err := json.Marshal(in)
	require.NoError(t, err)
	assert.Contains(t, string(output), `"fs_scheme":"s3"`)
	assert.Contains(t, string(output), `"blocking_reason":"MOVING"`)
}

// filesTestClient creates a minimal live files server and an authenticated
// client ready to call files routes.
func filesTestClient(t *testing.T) *Client {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())
	config.GetConfig().Fs.URL = &url.URL{Scheme: "file", Host: "localhost", Path: t.TempDir()}

	inst := setup.GetTestInstance()
	_, token := setup.GetTestClient(consts.Files + " " + consts.CertifiedCarbonCopy + " " + consts.CertifiedElectronicSafe)
	ts := setup.GetTestServer("/files", webfiles.Routes, func(r *echo.Echo) *echo.Echo {
		secure := middlewares.Secure(&middlewares.SecureConfig{CSPDefaultSrc: []middlewares.CSPSource{middlewares.CSPSrcSelf}, CSPFrameAncestors: []middlewares.CSPSource{middlewares.CSPSrcNone}})
		r.Use(secure)
		return r
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = weberrors.ErrorHandler
	t.Cleanup(ts.Close)

	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	hostPort := u.Host
	if _, _, e := net.SplitHostPort(hostPort); e != nil {
		hostPort = u.Host
	}

	return &Client{
		Addr:       hostPort,
		Domain:     inst.Domain,
		Scheme:     u.Scheme,
		Client:     &http.Client{Timeout: 10 * time.Second},
		Authorizer: &request.BearerAuthorizer{Token: token},
	}
}

// withListChildrenPageSize temporarily overrides ListChildrenPageSize.
func withListChildrenPageSize(t *testing.T, size string) {
	old := ListChildrenPageSize
	ListChildrenPageSize = size
	t.Cleanup(func() { ListChildrenPageSize = old })
}

func TestListChildrenByDirID_Pagination(t *testing.T) {
	if testing.Short() {
		t.Skip("requires instance; skipped with --short")
	}

	c := filesTestClient(t)

	parent, err := c.Mkdir("/client-pagination-root")
	require.NoError(t, err)

	withListChildrenPageSize(t, "2")

	names := []string{"a.txt", "b.txt", "c.txt"}
	for _, name := range names {
		_, err := c.Upload(&Upload{
			Name:        name,
			DirID:       parent.ID,
			Contents:    strings.NewReader("foo"),
			ContentType: "text/plain",
		})
		require.NoError(t, err)
	}

	_, err = c.Req(&request.Options{
		Method:  "POST",
		Path:    "/files/" + url.PathEscape(parent.ID),
		Queries: url.Values{"Type": {"directory"}, "Name": {"child"}},
	})
	require.NoError(t, err)

	children, err := c.ListChildrenByDirID(parent.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, len(children))
	gotNames := make(map[string]bool)
	for _, ch := range children {
		gotNames[ch.Attrs.Name] = true
	}
	assert.True(t, gotNames["a.txt"])
	assert.True(t, gotNames["b.txt"])
	assert.True(t, gotNames["c.txt"])
	assert.True(t, gotNames["child"])
}
