package opensearch

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recorded struct {
	method, path, query, contentType, user, pass string
	body                                         string
}

// newTestClient starts a fake OpenSearch that records the request and
// answers with the given status and body.
func newTestClient(t *testing.T, status int, answer string) (*Client, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.method, rec.path, rec.query = r.Method, r.URL.Path, r.URL.RawQuery
		rec.contentType, rec.body = r.Header.Get("Content-Type"), string(body)
		rec.user, rec.pass, _ = r.BasicAuth()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return newClientFromViper(t, map[string]string{
		"url":          srv.URL,
		"username":     "admin",
		"password":     "secret",
		"index_prefix": "test",
	}), rec
}

func newClientFromViper(t *testing.T, opts map[string]string) *Client {
	t.Helper()
	v := viper.New()
	for k, val := range opts {
		v.Set("search.opensearch."+k, val)
	}
	require.NoError(t, config.UseViper(v))
	c := NewClient(config.GetConfig().Search)
	require.NotNil(t, c)
	return c
}

func TestNewClientWithoutURL(t *testing.T) {
	assert.Nil(t, NewClient(config.Search{}))
}

func TestCreateIndex(t *testing.T) {
	c, rec := newTestClient(t, 200, `{"acknowledged":true}`)
	err := c.CreateIndex(context.Background(), "files_v1", map[string]any{"settings": map[string]int{"number_of_shards": 1}})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, rec.method)
	assert.Equal(t, "/test-files_v1", rec.path)
	assert.Equal(t, "application/json", rec.contentType)
	assert.Equal(t, "admin", rec.user)
	assert.Equal(t, "secret", rec.pass)
	assert.JSONEq(t, `{"settings":{"number_of_shards":1}}`, rec.body)
}

func TestSearch(t *testing.T) {
	c, rec := newTestClient(t, 200, `{"hits":{"hits":[{"_id":"a"}]}}`)
	var out struct {
		Hits struct {
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
	}
	require.NoError(t, c.Search(context.Background(), "files_v1", map[string]any{"size": 20}, &out))
	assert.Equal(t, "/test-files_v1/_search", rec.path)
	assert.JSONEq(t, `{"size":20}`, rec.body)
	require.Len(t, out.Hits.Hits, 1)
	assert.Equal(t, "a", out.Hits.Hits[0].ID)
}

func TestMget(t *testing.T) {
	c, rec := newTestClient(t, 200, `{"docs":[{"_id":"a","found":true}]}`)
	var out map[string]any
	require.NoError(t, c.Mget(context.Background(), "share_scopes_v1", []string{"a", "b"}, &out))
	assert.Equal(t, "/test-share_scopes_v1/_mget", rec.path)
	assert.JSONEq(t, `{"ids":["a","b"]}`, rec.body)
	assert.Contains(t, out, "docs")
}

func TestDeleteByQuery(t *testing.T) {
	c, rec := newTestClient(t, 200, `{"deleted":3,"version_conflicts":1,"failures":[]}`)
	query := map[string]any{"term": map[string]string{"owner_instance_id": "alice.example.net"}}
	require.NoError(t, c.DeleteByQuery(context.Background(), "files_v1", query))
	assert.Equal(t, "/test-files_v1/_delete_by_query", rec.path)
	assert.Equal(t, "conflicts=proceed", rec.query)
	assert.JSONEq(t, `{"query":{"term":{"owner_instance_id":"alice.example.net"}}}`, rec.body)
}

func TestDeleteByQueryFailures(t *testing.T) {
	const failedDeletes = `{"deleted":3,"failures":[
		{"id":"a","status":403,"cause":{"type":"security_exception","reason":"delete denied"}},
		{"id":"b","status":429,"cause":{"type":"rejected_execution_exception","reason":"queue full"}}
	]}`
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   Error
	}{
		{"partial success", 200, failedDeletes, Error{403, "security_exception", "delete denied"}},
		{"failed deletes", 429, failedDeletes, Error{403, "security_exception", "delete denied"}},
		{"failed shard", 503, `{"failures":[{"shard":0,"status":503,
			"reason":{"type":"search_context_missing_exception","reason":"scroll expired"}}]}`,
			Error{503, "search_context_missing_exception", "scroll expired"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.status, tc.body)
			err := c.DeleteByQuery(context.Background(), "files_v1", map[string]any{"match_all": map[string]any{}})
			var osErr *Error
			require.ErrorAs(t, err, &osErr)
			assert.Equal(t, tc.want, *osErr)
		})
	}
}

func TestBulk(t *testing.T) {
	ndjson := []byte("{\"index\":{\"_id\":\"a\"}}\n{\"name\":\"a.txt\"}\n")

	t.Run("success", func(t *testing.T) {
		c, rec := newTestClient(t, 200, `{"errors":false,"items":[{"index":{"status":201}}]}`)
		require.NoError(t, c.Bulk(context.Background(), "files_v1", ndjson))
		assert.Equal(t, "/test-files_v1/_bulk", rec.path)
		assert.Equal(t, "application/x-ndjson", rec.contentType)
		assert.Equal(t, string(ndjson), rec.body)
	})

	t.Run("a failed item is an error", func(t *testing.T) {
		c, _ := newTestClient(t, 200, `{"errors":true,"items":[
			{"index":{"status":201}},
			{"index":{"status":400,"error":{"type":"mapper_parsing_exception","reason":"failed to parse"}}}]}`)
		err := c.Bulk(context.Background(), "files_v1", ndjson)
		var osErr *Error
		require.True(t, errors.As(err, &osErr))
		assert.Equal(t, 400, osErr.Status)
		assert.Equal(t, "mapper_parsing_exception", osErr.Type)
		assert.Equal(t, "failed to parse", osErr.Reason)
	})
}

func TestErrors(t *testing.T) {
	cases := []struct {
		name, answer, typ, reason string
		status                    int
	}{
		{"object error", `{"error":{"type":"resource_already_exists_exception","reason":"index exists"},"status":400}`,
			"resource_already_exists_exception", "index exists", 400},
		{"string error", `{"error":"Incorrect HTTP method","status":405}`, "", "Incorrect HTTP method", 405},
		{"not JSON", `Too Many Requests`, "", "Too Many Requests", 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClient(t, tc.status, tc.answer)
			err := c.CreateIndex(context.Background(), "files_v1", map[string]any{})
			var osErr *Error
			require.True(t, errors.As(err, &osErr))
			assert.Equal(t, tc.status, osErr.Status)
			assert.Equal(t, tc.typ, osErr.Type)
			assert.Equal(t, tc.reason, osErr.Reason)
		})
	}
}

func TestRootCA(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"acknowledged": true})
	}))
	t.Cleanup(srv.Close)

	t.Run("unknown CA is refused", func(t *testing.T) {
		c := newClientFromViper(t, map[string]string{"url": srv.URL})
		err := c.CreateIndex(context.Background(), "files_v1", map[string]any{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "certificate")
	})

	t.Run("root_ca is trusted", func(t *testing.T) {
		caFile := filepath.Join(t.TempDir(), "ca.pem")
		pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		require.NoError(t, os.WriteFile(caFile, pemCert, 0o600))
		c := newClientFromViper(t, map[string]string{"url": srv.URL, "root_ca": caFile})
		require.NoError(t, c.CreateIndex(context.Background(), "files_v1", map[string]any{}))
	})
}

func TestPing(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		c, rec := newTestClient(t, 200, `{"version":{"number":"2.17.0"}}`)
		require.NoError(t, c.Ping(context.Background()))
		assert.Equal(t, http.MethodGet, rec.method)
		assert.Equal(t, "/", rec.path)
		assert.Equal(t, "admin", rec.user)
	})

	t.Run("bad credentials", func(t *testing.T) {
		c, _ := newTestClient(t, 401, `Unauthorized`)
		var osErr *Error
		require.True(t, errors.As(c.Ping(context.Background()), &osErr))
		assert.Equal(t, 401, osErr.Status)
	})

	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		c := newClientFromViper(t, map[string]string{"url": srv.URL})
		err := c.Ping(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "opensearch:")
	})
}
