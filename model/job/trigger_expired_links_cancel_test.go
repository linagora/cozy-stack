package job

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkReportScanCancellation(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_all_docs") && strings.Contains(r.URL.Path, "permissions"):
			fmt.Fprint(w, `{"rows":[{"id":"fixture","doc":{"_id":"fixture","type":"share","expires_at":"2025-01-01T00:00:00Z","activity_report_enabled":true,"codes":{"link":"fixture"},"permissions":{"files":{"type":"io.cozy.files"}}}}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/_all_docs") && strings.Contains(r.URL.Path, "public_links"):
			fmt.Fprint(w, `{"rows":[]}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "jobs"):
			_, _ = io.Copy(io.Discard, r.Body)
			close(entered)
			select {
			case <-r.Context().Done():
				close(canceled)
			case <-release:
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"ok":true,"id":"late-job","rev":"1-fixture"}`)
			}
		default:
			t.Errorf("unexpected CouchDB request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer s.Close()
	defer close(release)
	t.Setenv("COZY_COUCHDB_URL", s.URL+"/")
	config.UseTestFile(t)

	b := NewMemBroker().(*memBroker)
	q := newMemQueue(publiclink.ReportWorker)
	q.Jobs = make(chan *Job, 1)
	b.queues[publiclink.ReportWorker] = q
	b.workers = []*Worker{NewWorker(&WorkerConfig{WorkerType: publiclink.ReportWorker, Reserved: true})}
	b.running = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- queueExpiredLinks(ctx, b, &instance.Instance{Domain: "fixture.example.test"}, time.Date(2025, 2, 1, 2, 0, 0, 0, time.UTC))
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("scanner did not reach job creation: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("scanner did not reach job creation")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("job creation did not stop after cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("CouchDB request was not canceled")
	}
	require.Empty(t, q.Jobs, "a canceled scan must not publish to the broker")
	require.Zero(t, q.Len())
}
