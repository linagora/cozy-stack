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
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanInstance(t *testing.T) {
	t.Run("nothing to do when search is off", func(t *testing.T) {
		require.NoError(t, config.UseViper(viper.New()))
		require.NoError(t, purge.CleanInstance(context.Background(), "alice.example.net"))
	})

	t.Run("starts deleting the docs of the instance in both indices", func(t *testing.T) {
		var mu sync.Mutex
		var paths, queries, bodies []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			defer mu.Unlock()
			paths, queries, bodies = append(paths, r.URL.Path), append(queries, r.URL.RawQuery), append(bodies, string(data))
			_, _ = w.Write([]byte(`{"task":"node:1"}`))
		}))
		t.Cleanup(srv.Close)
		v := viper.New()
		v.Set("search.opensearch.url", srv.URL)
		v.Set("search.opensearch.index_prefix", "prod")
		require.NoError(t, config.UseViper(v))

		require.NoError(t, purge.CleanInstance(context.Background(), "alice.example.net"))
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"/prod-files_v1/_delete_by_query", "/prod-share_scopes_v1/_delete_by_query"}, paths)
		require.Len(t, bodies, 2)
		for i, body := range bodies {
			assert.Equal(t, "conflicts=proceed&wait_for_completion=false&requests_per_second=1000", queries[i])
			assert.JSONEq(t, `{"query":{"term":{"owner_instance_id":"alice.example.net"}}}`, body)
		}
	})

	t.Run("a failure on the files index still purges the scopes", func(t *testing.T) {
		var mu sync.Mutex
		var paths []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.URL.Path)
			mu.Unlock()
			if r.URL.Path == "/files_v1/_delete_by_query" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"task":"node:1"}`))
		}))
		t.Cleanup(srv.Close)
		v := viper.New()
		v.Set("search.opensearch.url", srv.URL)
		require.NoError(t, config.UseViper(v))
		require.Error(t, purge.CleanInstance(context.Background(), "alice.example.net"))
		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{"/files_v1/_delete_by_query", "/share_scopes_v1/_delete_by_query"}, paths)
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
	client := config.GetConfig().Search
	ctx := context.Background()
	require.NoError(t, search.EnsureIndices(ctx, client))

	// A file and the scope of a drive for each instance
	var files, scopes bytes.Buffer
	for _, domain := range []string{"alice.example.net", "bob.example.net"} {
		file := search.FileDocument{ResourceKey: search.Key(domain, "f1"), OwnerInstanceID: domain, DocID: "f1", Type: "file", Name: "a.txt"}
		enc := json.NewEncoder(&files)
		require.NoError(t, enc.Encode(map[string]any{"index": map[string]string{"_id": file.ResourceKey}}))
		require.NoError(t, enc.Encode(file))
		scope := search.ScopeDocument{SharingID: "s-" + domain, OwnerInstanceID: domain, RootDirID: "d1"}
		enc = json.NewEncoder(&scopes)
		require.NoError(t, enc.Encode(map[string]any{"index": map[string]string{"_id": scope.SharingID}}))
		require.NoError(t, enc.Encode(scope))
	}
	require.NoError(t, client.Bulk(ctx, search.FilesIndex, files.Bytes()))
	require.NoError(t, client.Bulk(ctx, search.ScopesIndex, scopes.Bytes()))

	count := func(index, domain string) int {
		var res struct {
			Hits struct {
				Hits []any `json:"hits"`
			} `json:"hits"`
		}
		query := map[string]any{"query": map[string]any{"term": map[string]any{"owner_instance_id": domain}}}
		require.NoError(t, client.Search(ctx, index, query, &res))
		return len(res.Hits.Hits)
	}
	// _delete_by_query only sees the docs after a refresh (1s by default)
	require.Eventually(t, func() bool {
		return count(search.FilesIndex, "alice.example.net") == 1 && count(search.ScopesIndex, "alice.example.net") == 1
	}, 5*time.Second, 200*time.Millisecond)

	require.NoError(t, purge.CleanInstance(ctx, "alice.example.net"))
	require.Eventually(t, func() bool {
		return count(search.FilesIndex, "alice.example.net") == 0 && count(search.ScopesIndex, "alice.example.net") == 0
	}, 10*time.Second, 200*time.Millisecond, "the tasks delete the docs of the instance")
	assert.Equal(t, 1, count(search.FilesIndex, "bob.example.net"), "the docs of other instances are kept")
	assert.Equal(t, 1, count(search.ScopesIndex, "bob.example.net"))
}
