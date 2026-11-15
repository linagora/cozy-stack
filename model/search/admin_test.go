package search

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/logger"
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

	t.Run("enable does not add a second trigger", func(t *testing.T) {
		pushes = 0
		require.NoError(t, Enable(ctx, inst))
		require.NoError(t, Enable(ctx, inst))
		assert.Equal(t, 2, pushes)
		triggers, err := job.System().GetAllTriggers(inst)
		require.NoError(t, err)
		n := 0
		for _, tr := range triggers {
			if tr.Infos().WorkerType == WorkerType {
				n++
			}
		}
		assert.Equal(t, 1, n)
	})

	t.Run("reset drops the checkpoint and the documents", func(t *testing.T) {
		pushes = 0
		require.NoError(t, Reset(ctx, inst))
		assert.Equal(t, 1, pushes)
		status, err := GetStatus(inst)
		require.NoError(t, err)
		assert.Empty(t, status.LastSeq)
		require.Eventually(t, func() bool {
			var res struct {
				Hits struct {
					Hits []any `json:"hits"`
				} `json:"hits"`
			}
			q := map[string]any{"query": map[string]any{"term": map[string]any{"owner_instance_id": inst.Domain}}}
			return client.Search(ctx, FilesIndex, q, &res) == nil && len(res.Hits.Hits) == 0
		}, 5*time.Second, 200*time.Millisecond)
	})
}
