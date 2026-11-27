package search

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/opensearch"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdminWithoutSearch(t *testing.T) {
	require.NoError(t, config.UseViper(viper.New())) // no search section
	inst := &instance.Instance{Domain: "alice.example.net"}
	assert.ErrorIs(t, Enable(context.Background(), inst), ErrNotConfigured)
	assert.ErrorIs(t, Reset(context.Background(), inst), ErrNotConfigured)
	_, err := GetStatus(inst)
	assert.ErrorIs(t, err, ErrNotConfigured)
}

func TestNoTriggerWithoutSearch(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	t.Setenv("COZY_SEARCH_OPENSEARCH_URL", "")
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	require.Nil(t, config.GetConfig().Search)
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	assert.False(t, job.HasSearchTrigger(inst), "an instance created without search has no trigger")
}

func TestAdmin(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}
	osURL := os.Getenv("COZY_TEST_OPENSEARCH_URL")
	if osURL == "" {
		t.Skip("COZY_TEST_OPENSEARCH_URL is not set")
	}
	t.Setenv("COZY_SEARCH_OPENSEARCH_URL", osURL)
	t.Setenv("COZY_SEARCH_OPENSEARCH_INDEX_PREFIX", fmt.Sprintf("test-%d", time.Now().UnixNano()))
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	// Created while search is configured: it gets the trigger
	inst := testutils.NewSetup(t, t.Name()).GetTestInstance()
	client := config.GetConfig().Search
	ctx := context.Background()
	log := logger.WithNamespace("search-test")

	var pushes int
	pushJob = func(*instance.Instance) error { pushes++; return nil }
	t.Cleanup(func() { pushJob = defaultPushJob })

	t.Run("new instances get the trigger", func(t *testing.T) {
		assert.True(t, job.HasSearchTrigger(inst))
	})

	t.Run("the first job creates the missing indices with their mappings", func(t *testing.T) {
		createTextFile(t, inst, consts.RootDirID, "Quarterly report.pdf")
		require.NoError(t, Index(ctx, inst, log))
		query := map[string]any{"query": map[string]any{"match": map[string]any{"name.exact": "quarterly report.pdf"}}}
		require.Eventually(t, func() bool {
			var res struct {
				Hits struct {
					Hits []any `json:"hits"`
				} `json:"hits"`
			}
			// name.exact only exists with our mapping, not a dynamic one
			return client.Search(ctx, FilesIndex, query, &res) == nil && len(res.Hits.Hits) == 1
		}, 5*time.Second, 200*time.Millisecond)
	})

	t.Run("status", func(t *testing.T) {
		status, err := GetStatus(inst)
		require.NoError(t, err)
		assert.True(t, status.Trigger)
		assert.NotEmpty(t, status.LastSeq)
	})

	searchTriggers := func() []job.Trigger {
		triggers, err := job.System().GetAllTriggers(inst)
		require.NoError(t, err)
		var found []job.Trigger
		for _, tr := range triggers {
			if tr.Infos().WorkerType == WorkerType {
				found = append(found, tr)
			}
		}
		return found
	}

	t.Run("enable adds the trigger once to an instance created before search", func(t *testing.T) {
		for _, tr := range searchTriggers() {
			require.NoError(t, job.System().DeleteTrigger(inst, tr.ID()))
		}
		require.False(t, job.HasSearchTrigger(inst))
		pushes = 0
		require.NoError(t, Enable(ctx, inst))
		require.NoError(t, Enable(ctx, inst))
		assert.Equal(t, 2, pushes)
		assert.Len(t, searchTriggers(), 1)
	})

	t.Run("reset drops the documents and indexes the instance again", func(t *testing.T) {
		count := func() int {
			var res struct {
				Hits struct {
					Hits []any `json:"hits"`
				} `json:"hits"`
			}
			q := map[string]any{"query": map[string]any{"term": map[string]any{"owner_instance_id": inst.Domain}}}
			require.NoError(t, client.Search(ctx, FilesIndex, q, &res))
			return len(res.Hits.Hits)
		}
		pushes = 0
		require.NoError(t, Reset(ctx, inst))
		assert.Equal(t, 1, pushes)
		status, err := GetStatus(inst)
		require.NoError(t, err)
		assert.Empty(t, status.LastSeq)
		require.Eventually(t, func() bool { return count() == 0 }, 10*time.Second, 200*time.Millisecond)

		// The job pushed by the reset
		require.NoError(t, Index(ctx, inst, log))
		require.Eventually(t, func() bool { return count() > 0 }, 5*time.Second, 200*time.Millisecond)
	})

	t.Run("a purge that fails in part still drops the checkpoint", func(t *testing.T) {
		lastSeq := func() string {
			cp, err := couchdb.LoadCheckpoint(inst, consts.Files, checkpointDocID)
			require.NoError(t, err)
			return cp.LastSeq
		}
		require.NotEmpty(t, lastSeq())
		// The files delete is accepted, the scopes one fails
		fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, ScopesIndex) && strings.HasSuffix(r.URL.Path, "/_delete_by_query") {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"acknowledged":true,"task":"node:1"}`))
		}))
		t.Cleanup(fake.Close)
		u, _ := url.Parse(fake.URL)
		config.GetConfig().Search = opensearch.NewClient(opensearch.Options{URL: u})
		t.Cleanup(func() { config.GetConfig().Search = client })
		pushes = 0
		assert.Error(t, Reset(ctx, inst))
		assert.Equal(t, 0, pushes)
		assert.Empty(t, lastSeq(), "the next job indexes the instance again")
	})
}
