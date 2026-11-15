package purge_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/search"
	"github.com/cozy/cozy-stack/model/search/purge"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanInstance(t *testing.T) {
	t.Run("nothing to do when search is off", func(t *testing.T) {
		require.NoError(t, config.UseViper(viper.New()))
		require.NoError(t, purge.CleanInstance("alice.example.net"))
	})

	t.Run("deletes the docs of the instance in both indices", func(t *testing.T) {
		var mu sync.Mutex
		var paths, bodies []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			paths, bodies = append(paths, r.URL.Path), append(bodies, string(data))
			mu.Unlock()
			_, _ = w.Write([]byte(`{"deleted":1}`))
		}))
		t.Cleanup(srv.Close)
		v := viper.New()
		v.Set("search.opensearch.url", srv.URL)
		v.Set("search.opensearch.index_prefix", "prod")
		require.NoError(t, config.UseViper(v))

		require.NoError(t, purge.CleanInstance("alice.example.net"))
		assert.Equal(t, []string{"/prod-files_v1/_delete_by_query", "/prod-share_scopes_v1/_delete_by_query"}, paths)
		for _, body := range bodies {
			assert.JSONEq(t, `{"query":{"term":{"owner_instance_id":"alice.example.net"}}}`, body)
		}
	})

	t.Run("an error is returned", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		t.Cleanup(srv.Close)
		v := viper.New()
		v.Set("search.opensearch.url", srv.URL)
		require.NoError(t, config.UseViper(v))
		require.Error(t, purge.CleanInstance("alice.example.net"))
	})
}

func TestCleanInstanceWithOpenSearch(t *testing.T) {
	osURL := os.Getenv("COZY_TEST_OPENSEARCH_URL")
	if osURL == "" {
		t.Skip("COZY_TEST_OPENSEARCH_URL is not set")
	}
	v := viper.New()
	v.Set("search.opensearch.url", osURL)
	v.Set("search.opensearch.index_prefix", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	require.NoError(t, config.UseViper(v))
	client := opensearch.New(config.GetConfig().Search)
	ctx := context.Background()
	require.NoError(t, search.EnsureIndices(ctx, client))

	var ndjson bytes.Buffer
	enc := json.NewEncoder(&ndjson)
	for _, domain := range []string{"alice.example.net", "bob.example.net"} {
		doc := search.FileDocument{ResourceKey: search.Key(domain, "f1"), OwnerInstanceID: domain, DocID: "f1", Type: "file", Name: "a.txt"}
		require.NoError(t, enc.Encode(map[string]any{"index": map[string]string{"_id": doc.ResourceKey}}))
		require.NoError(t, enc.Encode(doc))
	}
	require.NoError(t, client.Bulk(ctx, search.FilesIndex, ndjson.Bytes()))

	count := func(domain string) int {
		var res struct {
			Hits struct {
				Hits []any `json:"hits"`
			} `json:"hits"`
		}
		query := map[string]any{"query": map[string]any{"term": map[string]any{"owner_instance_id": domain}}}
		require.NoError(t, client.Search(ctx, search.FilesIndex, query, &res))
		return len(res.Hits.Hits)
	}
	// _delete_by_query only sees the docs after a refresh (1s by default)
	require.Eventually(t, func() bool { return count("alice.example.net") == 1 }, 5*time.Second, 200*time.Millisecond)

	require.NoError(t, purge.CleanInstance("alice.example.net"))
	require.Eventually(t, func() bool { return count("alice.example.net") == 0 }, 5*time.Second, 200*time.Millisecond)
	assert.Equal(t, 1, count("bob.example.net"), "the docs of other instances are kept")
}
