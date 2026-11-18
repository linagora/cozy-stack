package job

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

type sweepTransport func(*http.Request) (*http.Response, error)

func (f sweepTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func sweepFixture(t *testing.T, onEnumerate func()) *atomic.Int32 {
	t.Helper()
	v := viper.New()
	v.Set("couchdb.url", "http://couch.invalid/")
	v.Set("fs.url", "mem://test")
	previous := config.GetConfig()
	require.NoError(t, config.UseViper(v))
	cfg := config.GetConfig()
	oldClient := cfg.CouchDB.Client
	t.Cleanup(func() {
		cfg.CouchDB.Client = oldClient
		if previous != nil {
			*cfg = *previous
		}
	})
	instances := []*instance.Instance{
		{DocID: "a", Domain: "a.example", Prefix: "a"},
		{DocID: "b", Domain: "b.example", Prefix: "b"},
	}
	enabled := true
	at := time.Now().Add(-time.Hour).UTC()
	var created atomic.Int32
	cfg.CouchDB.Client = &http.Client{Transport: sweepTransport(func(r *http.Request) (*http.Response, error) {
		path := r.URL.Path
		var body interface{}
		switch {
		case strings.HasSuffix(path, "/instances/_all_docs"):
			onEnumerate()
			body = map[string]interface{}{"rows": []interface{}{map[string]interface{}{"id": "a", "doc": instances[0]}, map[string]interface{}{"id": "b", "doc": instances[1]}}}
		case strings.HasSuffix(path, "/instances/a"):
			body = instances[0]
		case strings.HasSuffix(path, "/instances/b"):
			body = instances[1]
		case strings.Contains(path, "io-cozy-settings"):
			body = map[string]interface{}{"_id": "io.cozy.settings"}
		case strings.Contains(path, "io-cozy-permissions"):
			id := "a-link"
			if strings.Contains(path, "/b/") {
				id = "b-link"
			}
			p := &permission.Permission{PID: id, Type: permission.TypeShareByLink, ActivityReportEnabled: &enabled, ExpiresAt: at, Codes: map[string]string{"link": "secret"}, Permissions: permission.Set{{Type: consts.Files}}}
			body = map[string]interface{}{"rows": []interface{}{map[string]interface{}{"id": id, "doc": p}}}
		case strings.Contains(path, "io-cozy-public_links-reports"):
			body = map[string]interface{}{"rows": []interface{}{}}
		case strings.Contains(path, "io-cozy-jobs") && r.Method == http.MethodPost:
			id := fmt.Sprintf("job-%d", created.Add(1))
			body = map[string]interface{}{"ok": true, "id": id, "rev": "1-abc"}
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", r.Method, path)
		}
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
	})}

	return &created
}

func TestExpiredLinkSweepWaves(t *testing.T) {
	created := sweepFixture(t, func() {})
	q := newMemQueue("expired-link-report")
	b := &memBroker{running: 1, queues: map[string]*memQueue{"expired-link-report": q}, workers: []*Worker{NewWorker(&WorkerConfig{WorkerType: "expired-link-report"})}}
	t.Cleanup(q.close)
	trigger := NewExpiredLinksTrigger(b)
	require.NoError(t, trigger.scan(t.Context()))
	// The sender may have removed one job before blocking on handoff.
	require.Eventually(t, func() bool { return q.Len() == 1 }, time.Second, time.Millisecond)
	require.EqualValues(t, 2, created.Load())
	for i := 0; i < 3; i++ {
		require.NoError(t, trigger.scan(t.Context()))
	}
	require.EqualValues(t, 2, created.Load())

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case j := <-q.Jobs:
			var msg publiclink.ReportMessage
			require.NoError(t, json.Unmarshal(j.Message, &msg))
			seen[j.Domain+"/"+msg.LinkID] = true
		case <-time.After(5 * time.Second):
			t.Fatal("job not delivered")
		}
	}
	require.Equal(t, map[string]bool{"a.example/a-link": true, "b.example/b-link": true}, seen)
	// Simulate loss after handoff: no checkpoint was written, so both are rediscovered.
	require.NoError(t, trigger.scan(t.Context()))
	require.EqualValues(t, 4, created.Load())
	require.Eventually(t, func() bool { return q.Len() == 1 }, time.Second, time.Millisecond)
	// Leave one job in flight and remove the other. An empty list permits a new wave.
	select {
	case <-q.Jobs:
	case <-time.After(5 * time.Second):
		t.Fatal("job not handed off")
	}
	require.Eventually(t, func() bool { return q.Len() == 0 }, time.Second, time.Millisecond)
	require.NoError(t, trigger.scan(t.Context()))
	require.EqualValues(t, 6, created.Load())
}

// Redis jobs may be shared even when the configured instance locks are in memory.
func TestExpiredLinkSweepRedis(t *testing.T) {
	path, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("requires redis-server for a private Unix socket")
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "redis.sock")
	cmd := exec.CommandContext(t.Context(), path, "--port", "0", "--unixsocket", socket, "--save", "", "--appendonly", "no", "--dir", dir)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	client := func(name string) *redis.Client {
		c := redis.NewClient(&redis.Options{Network: "unix", Addr: socket, MaxRetries: -1, ClientName: name})
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	ctx := t.Context()
	inspector, jobs := client("inspection"), client("scanner-b")
	require.Eventually(t, func() bool { return inspector.Ping(ctx).Err() == nil }, 3*time.Second, 10*time.Millisecond)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	var once sync.Once
	created := sweepFixture(t, func() { once.Do(func() { close(entered); <-release }) })
	scanner := func(c *redis.Client) *ExpiredLinksTrigger {
		b := NewRedisBroker(c)
		// Both stacks enqueue, neither consumes the wave.
		require.NoError(t, b.StartWorkers(WorkersList{{WorkerType: "expired-link-report", Concurrency: 0}}))
		return NewExpiredLinksTrigger(b)
	}
	a, b := scanner(client("scanner-a")), scanner(jobs)
	results := make(chan error, 2)
	var scans sync.WaitGroup
	defer func() { unblock(); scans.Wait() }()
	scans.Go(func() { results <- a.scan(ctx) })
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("scanner A did not start")
	}
	scans.Go(func() { results <- b.scan(ctx) })
	// Observe B trying SET NX on Redis while A is inside the empty-queue sweep.
	require.Eventually(t, func() bool {
		clients, err := inspector.ClientList(ctx).Result()
		if err != nil {
			return false
		}
		for _, line := range strings.Split(clients, "\n") {
			if strings.Contains(line, " name=scanner-b ") && strings.Contains(line, " cmd=set ") {
				return true
			}
		}
		return false
	}, 3*time.Second, 10*time.Millisecond)
	unblock()
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	queue := redisPrefix + "expired-link-report"
	for range 3 {
		require.NoError(t, b.scan(ctx))
	}
	list, err := inspector.LRange(ctx, queue, 0, -1).Result()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"a/job-1", "b/job-2"}, list)
	require.EqualValues(t, 2, created.Load())
	// Lost after dequeue, with no checkpoint. A pending priority entry still gates the wave.
	require.NoError(t, inspector.RPopCount(ctx, queue, 2).Err())
	priority := queue + redisHighPrioritySuffix
	require.NoError(t, inspector.LPush(ctx, priority, "pending").Err())
	require.NoError(t, b.scan(ctx))
	require.EqualValues(t, 2, created.Load())
	require.Equal(t, "pending", inspector.RPop(ctx, priority).Val())
	require.NoError(t, b.scan(ctx))
	require.EqualValues(t, 4, created.Load())
	// Depth failure and lock acquisition failure must both fail closed.
	require.NoError(t, inspector.Set(ctx, priority, "not-a-list", 0).Err())
	require.ErrorContains(t, b.scan(ctx), "WRONGTYPE")
	require.NoError(t, jobs.Close())
	require.Error(t, b.scan(ctx))
	require.EqualValues(t, 4, created.Load())
}
