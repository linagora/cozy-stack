package job

import (
	"context"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkReportScanMarkerACL(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and Redis")
	}
	config.UseTestFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cutoff := time.Date(2025, 11, 25, 2, 0, 0, 0, time.UTC)
	inst := reportScanInstance(t, true)
	addReportScanLink(t, inst, "expired", cutoff, true)
	admin := newRedisReportScanBroker(t, ctx).client
	q := redisPrefix + publiclink.ReportWorker
	marker := q + "/scanned/" + cutoff.Format(time.DateOnly)
	const user = "report-scan-no-marker"
	lease := "locks:" + prefixer.GlobalPrefixer.DBPrefix() + "/expired-link-report-scan"
	// Allow the existing broker operations, but initially not the marker key.
	require.NoError(t, admin.Do(ctx, "ACL", "SETUSER", user, "reset", "on", ">fixture", "+@all", "~"+lease, "~"+q, "~"+q+redisHighPrioritySuffix).Err())
	t.Cleanup(func() {
		require.NoError(t, admin.Do(context.Background(), "ACL", "DELUSER", user).Err())
		require.NoError(t, admin.Del(context.Background(), q, marker).Err())
	})
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 15, Username: user, Password: "fixture"})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	b := NewRedisBroker(client).(*redisBroker)
	require.NoError(t, b.StartWorkers(WorkersList{{WorkerType: publiclink.ReportWorker, Reserved: true}}))
	t.Cleanup(func() { require.NoError(t, b.ShutdownWorkers(context.Background())) })

	clock := func() time.Time { return cutoff }
	done, err := scanExpiredLinks(ctx, b, clock)
	require.NoError(t, err)
	require.True(t, done)
	require.EqualValues(t, 1, admin.LLen(ctx, q).Val())
	require.Zero(t, admin.Exists(ctx, marker).Val())
	require.NoError(t, admin.Del(ctx, q).Err())

	require.NoError(t, admin.Do(ctx, "ACL", "SETUSER", user, "~"+marker).Err())
	done, err = scanExpiredLinks(ctx, b, clock)
	require.NoError(t, err)
	require.True(t, done)
	require.EqualValues(t, 1, admin.Exists(ctx, marker).Val())
}
