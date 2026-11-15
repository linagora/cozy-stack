package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIndicesWithOpenSearch needs a real OpenSearch, given by
// COZY_TEST_OPENSEARCH_URL (it runs in CI).
func TestIndicesWithOpenSearch(t *testing.T) {
	url := os.Getenv("COZY_TEST_OPENSEARCH_URL")
	if url == "" {
		t.Skip("COZY_TEST_OPENSEARCH_URL is not set")
	}
	v := viper.New()
	v.Set("search.opensearch.url", url)
	v.Set("search.opensearch.index_prefix", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	require.NoError(t, config.UseViper(v))
	client := config.GetConfig().Search
	ctx := context.Background()

	require.NoError(t, EnsureIndices(ctx, client))
	require.NoError(t, EnsureIndices(ctx, client), "creating existing indices is not an error")

	doc := FileDocument{
		ResourceKey: "alice.example.net:f1", OwnerInstanceID: "alice.example.net",
		DocID: "f1", DirID: "d1", ScopedDirKey: "alice.example.net:d1",
		Type: "file", Name: "Quarterly Réport.pdf", Mime: "application/pdf", Class: "pdf",
		UpdatedAt: "2026-11-15T10:00:00Z",
	}
	var ndjson bytes.Buffer
	enc := json.NewEncoder(&ndjson)
	require.NoError(t, enc.Encode(map[string]any{"index": map[string]string{"_id": doc.ResourceKey}}))
	require.NoError(t, enc.Encode(doc))
	require.NoError(t, client.Bulk(ctx, FilesIndex, ndjson.Bytes()))

	count := func(text string) int {
		query := map[string]any{"query": map[string]any{"match": map[string]any{
			"name": map[string]any{"query": text, "minimum_should_match": "75%"},
		}}}
		var res struct {
			Hits struct {
				Hits []struct {
					ID string `json:"_id"`
				} `json:"hits"`
			} `json:"hits"`
		}
		require.NoError(t, client.Search(ctx, FilesIndex, query, &res))
		return len(res.Hits.Hits)
	}

	// Documents are searchable after the next refresh (1s by default)
	require.Eventually(t, func() bool { return count("report") == 1 }, 5*time.Second, 200*time.Millisecond,
		"a word longer than the grams, without its accent, matches the name")
	assert.Equal(t, 1, count("QUART"))
	assert.Equal(t, 0, count("zzz"))
	assert.Equal(t, 0, count("invoice"))
}
