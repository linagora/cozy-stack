package job

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkReportScanRetriesIncompleteWindow(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and Redis")
	}
	config.UseTestFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cutoff := time.Date(2025, 11, 23, 5, 59, 0, 0, time.UTC)
	clock := func() time.Time { return cutoff }
	bad := reportScanInstance(t, true)
	good := reportScanInstance(t, true)
	addReportScanLink(t, bad, "retry", cutoff, true)
	addReportScanLink(t, good, "healthy", cutoff, true)

	b1, b2 := newRedisReportScanBroker(t, ctx), newRedisReportScanBroker(t, ctx)
	q := redisPrefix + publiclink.ReportWorker
	marker := q + "/scanned/" + cutoff.Format(time.DateOnly)
	t.Cleanup(func() {
		require.NoError(t, b1.client.Del(context.Background(), q, marker).Err())
	})
	require.Zero(t, b1.client.LLen(ctx, q).Val())
	require.Zero(t, b1.client.Exists(ctx, marker).Val())

	// Leave CouchDB real except for the exact request that fails or stalls.
	cfg := config.GetConfig()
	original := cfg.CouchDB.Client
	defer func() { cfg.CouchDB.Client = original }()
	transport := original.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	withFault := func(intercept func(*http.Request) (*http.Response, error)) {
		client := *original
		client.Transport = scanFaultTransport(intercept)
		cfg.CouchDB.Client = &client
	}
	permissionsPath := "/" + couchdb.EscapeCouchdbName(bad.DBPrefix()+"/"+consts.Permissions) + "/_all_docs"
	withFault(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == permissionsPath {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Body:       io.NopCloser(strings.NewReader(`{"error":"unavailable","reason":"injected failure"}`)),
			}, nil
		}
		return transport.RoundTrip(req)
	})
	done, err := scanExpiredLinks(ctx, b1, clock)
	require.EqualError(t, err, "read permissions from CouchDB")
	require.True(t, couchdb.IsInternalServerError(err), "%v", err)
	require.False(t, done)
	require.Zero(t, b1.client.Exists(ctx, marker).Val())
	goodJob, err := b1.client.RPop(ctx, q).Result()
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(goodJob, good.DBPrefix()+"/"), goodJob)
	require.Zero(t, b1.client.LLen(ctx, q).Val())
	setReportScanState(t, good, "healthy", cutoff, publiclink.DeliverySent, time.Time{})

	entered := make(chan struct{})
	jobsPath := "/" + couchdb.EscapeCouchdbName(bad.DBPrefix()+"/"+consts.Jobs) + "/"
	withFault(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost && req.URL.Path == jobsPath {
			close(entered)
			<-req.Context().Done()
			return nil, req.Context().Err()
		}
		return transport.RoundTrip(req)
	})
	scanCtx, stopScan := context.WithCancel(ctx)
	defer stopScan()
	result := make(chan error, 1)
	go func() {
		_, err := scanExpiredLinks(scanCtx, b2, clock)
		result <- err
	}()
	select {
	case <-entered:
	case err := <-result:
		t.Fatalf("scanner returned before job creation: %v", err)
	case <-ctx.Done():
		t.Fatal("scanner did not reach job creation")
	}
	stopScan()
	require.ErrorIs(t, <-result, context.Canceled)
	require.Zero(t, b1.client.Exists(ctx, marker).Val())
	require.Zero(t, b1.client.LLen(ctx, q).Val())

	cfg.CouchDB.Client = original
	finishAt := func(finished time.Time) func() time.Time {
		at := cutoff
		return func() time.Time {
			now := at
			at = finished
			return now
		}
	}
	done, err = scanExpiredLinks(ctx, b2, finishAt(cutoff.Add(time.Minute)))
	require.NoError(t, err)
	require.True(t, done)
	require.EqualValues(t, 1, b1.client.LLen(ctx, q).Val(), "the scan completed at close")
	require.Zero(t, b1.client.Exists(ctx, marker).Val(), "06:00 is outside the window")
	require.NoError(t, b1.client.Del(ctx, q).Err())

	done, err = scanExpiredLinks(ctx, b2, finishAt(cutoff.Add(45*time.Second)))
	require.NoError(t, err)
	require.True(t, done)
	items, err := b1.client.LRange(ctx, q, 0, -1).Result()
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.True(t, strings.HasPrefix(items[0], bad.DBPrefix()+"/"), items[0])
	ttl, err := b1.client.PTTL(ctx, marker).Result()
	require.NoError(t, err)
	require.Positive(t, ttl)
	require.LessOrEqual(t, ttl, 15*time.Second)
}

func TestExpiredLinkReportScanBackoff(t *testing.T) {
	var backoff reportScanBackoff
	for _, minutes := range []time.Duration{5, 10, 20, 40, 60, 60} {
		require.Equal(t, minutes*time.Minute, backoff.nextDelay(true))
		// A nonempty queue does not count as a failure or erase prior failures.
		require.Equal(t, 5*time.Minute, backoff.nextDelay(false))
	}
}

type scanFaultTransport func(*http.Request) (*http.Response, error)

func (f scanFaultTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
