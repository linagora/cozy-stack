package job

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkReportScan(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and Redis")
	}
	config.UseTestFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cutoff := time.Date(2025, time.November, 20, 2, 0, 0, 0, time.UTC)
	clock := func() time.Time { return cutoff }
	inst := reportScanInstance(t, true)
	disabled := reportScanInstance(t, false)
	old := cutoff.AddDate(0, -1, 0)

	// The second page must be visited even when every link on the first page
	// is already terminal. Use different exact expiry times for the same link.
	for idx := range 102 {
		id := fmt.Sprintf("page-%03d", idx)
		addReportScanLink(t, inst, id, old, true)
		setReportScanState(t, inst, id, old, publiclink.DeliverySent, time.Time{})
	}
	badPermission := &couchdb.JSONDoc{Type: consts.Permissions, M: map[string]interface{}{
		"_id": "bad-opt-in", "type": permission.TypeShareByLink, "activity_report_enabled": "true",
	}}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, badPermission))
	addReportScanLink(t, inst, "catch-up", old, true)
	addReportScanLink(t, inst, "pending", old, true)
	setReportScanState(t, inst, "pending", old, publiclink.DeliveryPending, cutoff)
	addReportScanLink(t, inst, "retry-tomorrow", old, true)
	setReportScanState(t, inst, "retry-tomorrow", old, publiclink.DeliveryFileOnly, cutoff.AddDate(0, 0, 1))
	addReportScanLink(t, inst, "z-at-cutoff", cutoff, true)
	setReportScanState(t, inst, "z-at-cutoff", cutoff.Add(-time.Nanosecond), publiclink.DeliverySent, time.Time{})
	addReportScanLink(t, inst, "z-future", cutoff.Add(time.Nanosecond), true)
	addReportScanLink(t, inst, "z-opt-out", old, false)
	addReportScanLink(t, inst, "z-submitting", old, true)
	setReportScanState(t, inst, "z-submitting", old, publiclink.DeliverySubmitting, time.Time{})
	addReportScanLink(t, disabled, "all-disabled", old, true)

	b1, b2 := newRedisReportScanBroker(t, ctx), newRedisReportScanBroker(t, ctx)
	q := redisPrefix + publiclink.ReportWorker
	tomorrow := cutoff.AddDate(0, 0, 1)
	marker := q + "/scanned/" + cutoff.Format(time.DateOnly)
	nextMarker := q + "/scanned/" + tomorrow.Format(time.DateOnly)
	depth, err := b1.WorkerQueueLen(publiclink.ReportWorker)
	require.NoError(t, err)
	require.Zero(t, depth, "report queues must be unused outside this test")
	t.Cleanup(func() {
		require.NoError(t, b1.client.Del(context.Background(), q, q+redisHighPrioritySuffix, marker, nextMarker).Err())
	})

	// A priority job also prevents a new normal wave.
	priority, err := b1.PushJob(inst, &JobRequest{WorkerType: publiclink.ReportWorker, Manual: true})
	require.NoError(t, err)
	scanned, err := scanExpiredLinks(ctx, b2, clock)
	require.NoError(t, err)
	require.False(t, scanned)
	require.Zero(t, b1.client.Exists(ctx, marker).Val())
	require.Zero(t, b1.client.LLen(ctx, q).Val())
	item, err := b1.client.RPop(ctx, q+redisHighPrioritySuffix).Result()
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(item, "/"+priority.ID()))

	// Two independent producers contend on the Redis jobs backend, while
	// consumers are stopped. Only one wave can reach the normal queue.
	start := make(chan struct{})
	type result struct {
		scanned bool
		err     error
	}
	results := make(chan result, 2)
	for _, b := range []*redisBroker{b1, b2} {
		go func() {
			<-start
			didScan, err := scanExpiredLinks(ctx, b, clock)
			results <- result{didScan, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	require.True(t, a.scanned)
	require.True(t, b.scanned)

	want := []publiclink.ReportMessage{{LinkID: "catch-up", Expiry: old}, {LinkID: "pending", Expiry: old}, {LinkID: "z-at-cutoff", Expiry: cutoff}}
	readWave := func() []publiclink.ReportMessage {
		t.Helper()
		items, err := b1.client.LRange(ctx, q, 0, -1).Result()
		require.NoError(t, err)
		var got []publiclink.ReportMessage
		for _, item := range items {
			prefix, jobID, ok := strings.Cut(item, "/")
			require.True(t, ok)
			require.Equal(t, inst.DBPrefix(), prefix)
			j, err := Get(inst, jobID)
			require.NoError(t, err)
			var msg publiclink.ReportMessage
			require.NoError(t, j.Message.Unmarshal(&msg))
			got = append(got, msg)
		}
		return got
	}
	require.ElementsMatch(t, want, readWave())

	ttl, err := b1.client.PTTL(ctx, marker).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 3*time.Hour+59*time.Minute)
	require.LessOrEqual(t, ttl, 4*time.Hour)

	// A new producer on the same Redis cannot repeat a completed scan even
	// after drainage. No delivery claims have been made for the enqueued jobs.
	require.NoError(t, b1.client.Del(ctx, q).Err())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRedisReportScanRestartHelper$")
	cmd.Env = append(os.Environ(), "COZY_REPORT_SCAN_TEST_CUTOFF="+cutoff.Format(time.RFC3339Nano))
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	require.Empty(t, readWave())

	// Each window is independent even if yesterday's marker is still present.
	scanned, err = scanExpiredLinks(ctx, b2, func() time.Time { return cutoff.Add(4 * time.Hour) })
	require.NoError(t, err)
	require.False(t, scanned)
	require.Empty(t, readWave())
	scanned, err = scanExpiredLinks(ctx, b2, func() time.Time { return tomorrow })
	require.NoError(t, err)
	require.True(t, scanned)
	want = append(want, publiclink.ReportMessage{LinkID: "retry-tomorrow", Expiry: old}, publiclink.ReportMessage{LinkID: "z-future", Expiry: cutoff.Add(time.Nanosecond)})
	require.ElementsMatch(t, want, readWave())
}

func TestRedisReportScanRestartHelper(t *testing.T) {
	raw, ok := os.LookupEnv("COZY_REPORT_SCAN_TEST_CUTOFF")
	if !ok {
		return
	}
	cutoff, err := time.Parse(time.RFC3339Nano, raw)
	require.NoError(t, err)
	config.UseTestFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	done, err := scanExpiredLinks(ctx, newRedisReportScanBroker(t, ctx), func() time.Time { return cutoff })
	require.NoError(t, err)
	require.True(t, done)
}

func newRedisReportScanBroker(t *testing.T, ctx context.Context) *redisBroker {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	require.NoError(t, client.Ping(ctx).Err())
	b := NewRedisBroker(client).(*redisBroker)
	// Producers may have no consumer configured in this process.
	require.NoError(t, b.StartWorkers(WorkersList{{WorkerType: publiclink.ReportWorker, Reserved: true}}))
	t.Cleanup(func() { require.NoError(t, b.ShutdownWorkers(context.Background())) })
	return b
}

func TestExpiredLinkReportScanMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB")
	}
	config.UseTestFile(t)
	instance.Init()
	inst := reportScanInstance(t, true)
	require.NoError(t, couchdb.DefineView(prefixer.GlobalPrefixer, couchdb.DomainAndAliasesView))
	cutoff := time.Date(2025, time.November, 20, 2, 0, 0, 0, time.UTC)
	addReportScanLink(t, inst, "memory-catch-up", cutoff, true)
	consumed := make(chan publiclink.ReportMessage, 1)
	b := NewMemBroker().(*memBroker)
	require.NoError(t, b.StartWorkers(WorkersList{{
		WorkerType: publiclink.ReportWorker, Reserved: true, Concurrency: 1,
		WorkerFunc: func(ctx *TaskContext) error {
			var msg publiclink.ReportMessage
			if err := ctx.UnmarshalMessage(&msg); err != nil {
				return err
			}
			consumed <- msg
			return nil
		},
	}}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, b.ShutdownWorkers(ctx))
	})
	scanned, err := scanExpiredLinks(ctx, b, func() time.Time { return cutoff })
	require.NoError(t, err)
	require.True(t, scanned)
	select {
	case msg := <-consumed:
		require.Equal(t, publiclink.ReportMessage{LinkID: "memory-catch-up", Expiry: cutoff}, msg)
	case <-ctx.Done():
		t.Fatal("report was not delivered through the in-memory broker")
	}
}

func reportScanInstance(t *testing.T, enabled bool) *instance.Instance {
	t.Helper()
	inst := &instance.Instance{
		Domain:       "report-scan-" + strings.ToLower(utils.RandomString(10)) + ".example.test",
		FeatureFlags: map[string]interface{}{publiclink.FlagEnabled: enabled},
	}
	require.NoError(t, couchdb.CreateDoc(prefixer.GlobalPrefixer, inst))
	t.Cleanup(func() {
		require.NoError(t, couchdb.DeleteDoc(prefixer.GlobalPrefixer, inst))
		for _, dt := range []string{consts.Permissions, consts.PublicLinkReports, consts.Jobs} {
			if err := couchdb.DeleteDB(inst, dt); !couchdb.IsNotFoundError(err) {
				require.NoError(t, err)
			}
		}
	})
	return inst
}

func addReportScanLink(t *testing.T, inst *instance.Instance, id string, expiry time.Time, enabled bool) {
	t.Helper()
	p := &permission.Permission{
		Type: permission.TypeShareByLink, ExpiresAt: expiry.Format(time.RFC3339Nano), ActivityReportEnabled: &enabled,
		Permissions: permission.Set{{Type: consts.Files}}, Codes: map[string]string{"link": "fixture-sharecode"},
	}
	p.SetID(id)
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, p))
}

func setReportScanState(t *testing.T, inst *instance.Instance, linkID string, expiry time.Time, status string, next time.Time) {
	t.Helper()
	state := &publiclink.Delivery{
		DocID:  publiclink.DeliveryID(publiclink.ReportID(linkID, expiry)),
		Status: status, NextAttempt: next,
	}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, state))
}
