package remote

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	build "github.com/cozy/cozy-stack/pkg/config"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const doctype = "org.example.request"

func TestParseRawRequest(t *testing.T) {
	config.UseTestFile(t)

	raw := `GET`
	_, err := ParseRawRequest(doctype, raw)
	assert.Equal(t, ErrInvalidRequest, err)

	raw = `PUT https://example.org/`
	_, err = ParseRawRequest(doctype, raw)
	assert.Equal(t, ErrInvalidRequest, err)

	raw = `GET ftp://example.org/`
	_, err = ParseRawRequest(doctype, raw)
	assert.Equal(t, ErrInvalidRequest, err)

	raw = `GET /etc/hosts`
	_, err = ParseRawRequest(doctype, raw)
	assert.Equal(t, ErrInvalidRequest, err)

	raw = `GET https://example.org/
Foo`
	_, err = ParseRawRequest(doctype, raw)
	assert.Equal(t, ErrInvalidRequest, err)

	// Allow a trailing \n after headers on GET
	raw = `GET https://www.wikidata.org/wiki/Special:EntityData/{{entity}}.json
Content-Type: application/json
`
	_, err = ParseRawRequest(doctype, raw)
	assert.NoError(t, err)

	raw = `GET https://www.wikidata.org/wiki/Special:EntityData/{{entity}}.json`
	r1, err := ParseRawRequest(doctype, raw)
	assert.NoError(t, err)
	assert.Equal(t, "GET", r1.Verb)
	assert.Equal(t, "https", r1.URL.Scheme)
	assert.Equal(t, "www.wikidata.org", r1.URL.Host)
	assert.Equal(t, "/wiki/Special:EntityData/{{entity}}.json", r1.URL.Path)
	assert.Equal(t, "", r1.URL.RawQuery)

	raw = `POST https://www.wikidata.org/w/api.php?action=wbsearchentities&search={{q}}&language=en&format=json
Accept-Language: fr-FR,en
Content-Type: application/json

one={{one}}
two={{two}}`
	r2, err := ParseRawRequest(doctype, raw)
	assert.NoError(t, err)
	assert.Equal(t, "POST", r2.Verb)
	assert.Equal(t, "https", r2.URL.Scheme)
	assert.Equal(t, "www.wikidata.org", r2.URL.Host)
	assert.Equal(t, "/w/api.php", r2.URL.Path)
	assert.Equal(t, "action=wbsearchentities&search={{q}}&language=en&format=json", r2.URL.RawQuery)
	assert.Equal(t, "fr-FR,en", r2.Headers["Accept-Language"])
	assert.Equal(t, "application/json", r2.Headers["Content-Type"])
	assert.Equal(t, `one={{one}}
two={{two}}`, r2.Body)
}

func TestExtractVariablesGET(t *testing.T) {
	config.UseTestFile(t)

	u, err := url.Parse("https://example.org/foo?one=un&two=deux")
	assert.NoError(t, err)
	in := &http.Request{URL: u}
	vars, err := extractVariables("GET", in)
	assert.NoError(t, err)
	assert.Equal(t, "un", vars["one"])
	assert.Equal(t, "deux", vars["two"])
}

func TestExtractVariablesPOST(t *testing.T) {
	config.UseTestFile(t)

	body := bytes.NewReader([]byte(`{"one": "un", "two": "deux"}`))
	in := httptest.NewRequest("POST", "https://example.com/bar", body)
	vars, err := extractVariables("POST", in)
	assert.NoError(t, err)
	assert.Equal(t, "un", vars["one"])
	assert.Equal(t, "deux", vars["two"])

	body = bytes.NewReader([]byte(`one=un&two=deux`))
	in = httptest.NewRequest("POST", "https://example.com/bar", body)
	_, err = extractVariables("POST", in)
	assert.Error(t, err)
}

func TestInjectVariables(t *testing.T) {
	config.UseTestFile(t)

	raw := `POST https://example.org/foo/{{bar}}?q={{q}}
Content-Type: {{contentType}}
Accept-Language: {{lang}},en

{ "one": "{{ json one }}", "two": "{{ json two }}" }
<p>{{html content}}</p>`
	r, err := ParseRawRequest(doctype, raw)
	require.NoError(t, err)

	vars := map[string]string{
		"contentType": "application/json",
		"bar":         "baz&/",
		"q":           "Q42&?",
		"lang":        "fr-FR\n",
		"one":         "un\"\n",
		"two":         "deux",
		"content":     "hey ! <<>>",
	}

	err = injectVariables(r, vars)
	assert.NoError(t, err)
	assert.Equal(t, "POST", r.Verb)
	assert.Equal(t, "https", r.URL.Scheme)
	assert.Equal(t, "example.org", r.URL.Host)
	assert.Equal(t, "/foo/baz&%2F", r.URL.Path)
	assert.Equal(t, "q=Q42%26%3F", r.URL.RawQuery)
	assert.Equal(t, "fr-FR\\n,en", r.Headers["Accept-Language"])
	assert.Equal(t, "application/json", r.Headers["Content-Type"])
	assert.Equal(t, `{ "one": "un\"\n", "two": "deux" }
<p>hey ! &lt;&lt;&gt;&gt;</p>`, r.Body)

	r, err = ParseRawRequest(doctype, `POST https://example.org/{{missing}}`)
	assert.NoError(t, err)
	err = injectVariables(r, vars)
	assert.Equal(t, ErrMissingVar, err)
}

func TestInjectSecret(t *testing.T) {
	if testing.Short() {
		t.Skip("a couchdb is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)

	doctype := "cc.cozycloud.foobar"
	doc := couchdb.JSONDoc{
		Type: consts.RemoteSecrets,
		M: map[string]interface{}{
			"_id":   doctype,
			"token": "123456789",
		},
	}
	err := couchdb.CreateNamedDocWithDB(prefixer.SecretsPrefixer, &doc)
	assert.NoError(t, err)
	defer func() {
		_ = couchdb.DeleteDoc(prefixer.SecretsPrefixer, &doc)
	}()

	raw := `POST https://example.org/foo/
Authorization: Bearer {{secret_token}}

{ "bar": "baz" }
`
	r, err := ParseRawRequest(doctype, raw)
	assert.NoError(t, err)
	err = injectVariables(r, map[string]string{})
	assert.NoError(t, err)
	assert.Equal(t, "Bearer 123456789", r.Headers["Authorization"])
}

func TestProxyToRefusesLoopback(t *testing.T) {
	config.UseTestFile(t)
	config.GetConfig().RemoteAllowCustomPort = true
	mode := build.BuildMode
	build.BuildMode = build.ModeProd
	t.Cleanup(func() { build.BuildMode = mode })

	called := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("internal"))
	}))
	t.Cleanup(ts.Close)

	r, err := ParseRawRequest(doctype, "GET "+ts.URL+"/\n")
	require.NoError(t, err)
	in := httptest.NewRequest(http.MethodGet, "/remote/"+doctype, nil)
	err = r.ProxyTo(nil, httptest.NewRecorder(), in, "app")
	assert.Equal(t, ErrRequestFailed, err)
	assert.False(t, called)
}
