package job

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkReportScanFreshManager(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and Redis")
	}
	config.UseTestFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cutoff := time.Date(2025, 11, 24, 2, 0, 0, 0, time.UTC)
	clock := func() time.Time { return cutoff }
	const ctxA, ctxB = "report-manager-a", "report-manager-b"
	type query struct{ context, sets string }
	enabled := map[query]bool{{ctxA, "team blue"}: true, {ctxA, "blue,team"}: true}
	var phase atomic.Int32
	var mu sync.Mutex
	calls := map[query]int{}
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/features" {
			t.Errorf("unexpected Manager request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		q := query{r.URL.Query().Get("context"), r.URL.Query().Get("sets")}
		mu.Lock()
		calls[q]++
		mu.Unlock()
		p := phase.Load()
		if p == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintf(w, `{"flags":{"%s":%t}}`, publiclink.FlagEnabled, p == 2 && enabled[q])
	}))
	defer manager.Close()
	conf := config.GetConfig()
	original := conf.Clouderies
	api := config.ClouderyConfig{API: config.ClouderyAPI{URL: manager.URL, Token: "fixture"}}
	conf.Clouderies = map[string]config.ClouderyConfig{ctxA: api, ctxB: api}
	defer func() { conf.Clouderies = original }()
	add := func(context string, sets []string, override *bool) *instance.Instance {
		inst := reportScanInstance(t, false)
		inst.ContextName = context
		inst.FeatureSets = sets
		inst.FeatureFlags = nil
		if override != nil {
			inst.FeatureFlags = map[string]interface{}{publiclink.FlagEnabled: *override}
		}
		require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, inst))
		addReportScanLink(t, inst, "expired", cutoff, true)
		return inst
	}
	on, off := true, false
	first := add(ctxA, []string{"team blue"}, nil)
	second := add(ctxA, []string{"team blue"}, nil)
	add(ctxA, []string{"team", "blue"}, nil)
	ordered := add(ctxA, []string{"blue", "team"}, nil)
	add(ctxB, []string{"team blue"}, nil)
	override := add(ctxB, []string{"do-not-query"}, &on)
	add(ctxA, []string{"team blue"}, &off)
	require.False(t, feature.Enabled(first, publiclink.FlagEnabled), "prime the normal Manager cache")

	b := newRedisReportScanBroker(t, ctx)
	q := redisPrefix + publiclink.ReportWorker
	marker := q + "/scanned/" + cutoff.Format(time.DateOnly)
	t.Cleanup(func() { require.NoError(t, b.client.Del(context.Background(), q, marker).Err()) })
	phase.Store(1)
	done, err := scanExpiredLinks(ctx, b, clock)
	require.EqualError(t, err, "resolve instance report flag")
	require.False(t, done)
	require.Zero(t, b.client.Exists(ctx, marker).Val())
	item, err := b.client.RPop(ctx, q).Result()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(item, override.DBPrefix()+"/"), item)
	require.Zero(t, b.client.LLen(ctx, q).Val())
	setReportScanState(t, override, "expired", cutoff, publiclink.DeliverySent, time.Time{})

	phase.Store(2)
	done, err = scanExpiredLinks(ctx, b, clock)
	require.NoError(t, err)
	require.True(t, done)
	require.EqualValues(t, 1, b.client.Exists(ctx, marker).Val())
	items, err := b.client.LRange(ctx, q, 0, -1).Result()
	require.NoError(t, err)
	prefixes := make([]string, len(items))
	for i, item := range items {
		prefixes[i], _, _ = strings.Cut(item, "/")
	}
	require.ElementsMatch(t, []string{first.DBPrefix(), second.DBPrefix(), ordered.DBPrefix()}, prefixes)
	require.False(t, feature.Enabled(first, publiclink.FlagEnabled), "the scanner did not replace the normal Manager cache")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[query]int{{ctxA, "team blue"}: 3, {ctxA, "team,blue"}: 2, {ctxA, "blue,team"}: 2, {ctxB, "team blue"}: 2}, calls)
}
