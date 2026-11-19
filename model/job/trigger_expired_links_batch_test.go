package job_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

type scanTransport func(*http.Request) (*http.Response, error)

func (f scanTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExpiredLinkBatchScan(t *testing.T) {
	now := time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour)
	enabled := true
	inst := &instance.Instance{DocID: "instance-id", Domain: "scan.example", Prefix: "scan"}

	for _, tc := range []struct {
		name        string
		count       int
		mode        string
		want        []int
		wantBatches []int
		wantErr     bool
	}{
		{name: "terminal first page and mixed checkpoints", count: 205, wantBatches: []int{100, 100, 5}},
		{name: "entirely ineligible middle pages", count: 305, mode: "sparse", want: []int{300, 301, 302, 303, 304}, wantBatches: []int{100, 5}},
		{name: "no permissions"},
		{name: "database missing", count: 3, mode: "missing-db", want: []int{0, 1, 2}, wantBatches: []int{3}},
		{name: "couch error", count: 3, mode: "error", wantBatches: []int{3}, wantErr: true},
		{name: "decode error", count: 3, mode: "decode", wantBatches: []int{3}, wantErr: true},
		{name: "cancelled", count: 3, mode: "cancel", wantErr: true},
		{name: "cancelled during checkpoint read", count: 3, mode: "cancel-read", wantBatches: []int{3}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := viper.New()
			v.Set("couchdb.url", "http://couch.invalid/")
			v.Set("fs.url", "mem://test")
			previousConfig := config.GetConfig()
			require.NoError(t, config.UseViper(v))
			cfg := config.GetConfig()
			previousClient := cfg.CouchDB.Client
			t.Cleanup(func() {
				cfg.CouchDB.Client = previousClient
				if previousConfig != nil {
					*cfg = *previousConfig
				}
			})
			instance.Init()
			states := make(map[string]int)
			rows := make([]map[string]interface{}, 0, tc.count)
			for i := 0; i < tc.count; i++ {
				id := fmt.Sprintf("link-%03d", i)
				p := &permission.Permission{PID: id, Type: permission.TypeShareByLink, ActivityReportEnabled: &enabled, ExpiresAt: at, Codes: map[string]string{"link": "secret"}, Permissions: permission.Set{{Type: consts.Files}}}
				if tc.mode == "sparse" && i >= 100 && i < 300 {
					p.ExpiresAt = now.Add(time.Hour)
				}
				rows = append(rows, map[string]interface{}{"id": id, "doc": p})
				states[publiclink.ReportID(id, at)] = i
			}
			if tc.count == 205 {
				disabled := false
				for i, p := range []*permission.Permission{
					{PID: "skip-disabled", Type: permission.TypeShareByLink, ActivityReportEnabled: &disabled, ExpiresAt: at, Codes: map[string]string{"link": "secret"}, Permissions: permission.Set{{Type: consts.Files}}},
					{PID: "skip-future", Type: permission.TypeShareByLink, ActivityReportEnabled: &enabled, ExpiresAt: now.Add(time.Hour), Codes: map[string]string{"link": "secret"}, Permissions: permission.Set{{Type: consts.Files}}},
				} {
					rows = append(rows, map[string]interface{}{"id": fmt.Sprintf("skip-%d", i), "doc": p})
				}
				for i := 100; i < 205; i++ {
					if i != 101 {
						tc.want = append(tc.want, i)
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.mode == "cancel" {
				cancel()
			}
			page := 0
			var batches []int
			var created atomic.Int32
			config.GetConfig().CouchDB.Client = &http.Client{Transport: scanTransport(func(r *http.Request) (*http.Response, error) {
				status := http.StatusOK
				var body interface{}
				path := r.URL.Path
				switch {
				case strings.Contains(path, "_design"):
					body = map[string]interface{}{"rows": []interface{}{map[string]interface{}{"doc": inst}}}
				case strings.Contains(path, "/instances/"):
					body = inst
				case strings.Contains(path, "io-cozy-settings"):
					body = map[string]interface{}{"_id": "io.cozy.settings"}
				case strings.Contains(path, "io-cozy-permissions"):
					query := r.URL.Query()
					if r.Method != http.MethodGet || query.Get("limit") != "100" || (page > 0 && (query.Get("skip") != "1" || query.Get("startkey") != fmt.Sprintf("\"%s\"", rows[page*100-1]["id"]))) {
						return nil, fmt.Errorf("invalid permission pagination: %s", r.URL.RawQuery)
					}
					start := page * 100
					end := min(start+100, len(rows))
					if start > len(rows) {
						start = len(rows)
					}
					body = map[string]interface{}{"rows": rows[start:end]}
					page++
				case strings.Contains(path, "io-cozy-public_links-reports"):
					var req struct {
						Keys []string `json:"keys"`
					}
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						return nil, err
					}
					if r.Method != http.MethodPost || len(req.Keys) == 0 || len(req.Keys) > 100 {
						return nil, fmt.Errorf("invalid checkpoint request")
					}
					batches = append(batches, len(req.Keys))
					switch tc.mode {
					case "missing-db":
						status, body = 404, map[string]string{"error": "not_found", "reason": "no_db_file"}
					case "error":
						status, body = 500, map[string]string{"error": "internal_server_error", "reason": "failure"}
					case "cancel-read":
						cancel()
						body = map[string]interface{}{"rows": []interface{}{}}
					case "decode":
						body = map[string]interface{}{"rows": []interface{}{map[string]interface{}{"doc": map[string]interface{}{"next_attempt": 42}}}}
					default:
						out := make([]interface{}, 0, len(req.Keys))
						for i := len(req.Keys) - 1; i >= 0; i-- {
							key := req.Keys[i]
							n := states[key]
							switch {
							case n < 100:
								out = append(out, map[string]interface{}{"id": key, "doc": &publiclink.Report{DocID: key, Status: []string{"sent", "no-email", "cancelled", "failed"}[n%4]}})
							case n == 101:
								out = append(out, map[string]interface{}{"id": key, "doc": &publiclink.Report{DocID: key, NextAttempt: now.Add(time.Hour)}})
							case n == 102:
								out = append(out, map[string]interface{}{"id": key, "doc": &publiclink.Report{DocID: key, NextAttempt: now}})
							case n == 103:
								out = append(out, map[string]interface{}{"key": key, "error": "not_found"})
							case n == 104:
								out = append(out, map[string]interface{}{"id": key, "value": map[string]interface{}{"deleted": true}, "doc": nil})
							default:
								out = append(out, map[string]interface{}{"id": key, "doc": nil})
							}
						}
						body = map[string]interface{}{"rows": out}
					}
				case strings.Contains(path, "io-cozy-jobs"):
					if r.Method == http.MethodPost {
						created.Add(1)
					}
					body = map[string]interface{}{"ok": true, "id": "job-id", "rev": "1-abc"}
				default:
					return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
				}
				data, err := json.Marshal(body)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data))), Request: r}, nil
			})}
			broker := job.NewMemBroker()
			delivered := make(chan publiclink.ReportMessage, 300)
			require.NoError(t, broker.StartWorkers(job.WorkersList{{WorkerType: "expired-link-report", Concurrency: 1, MaxExecCount: 1, Timeout: time.Minute, WorkerFunc: func(ctx *job.TaskContext) error {
				var msg publiclink.ReportMessage
				if err := ctx.UnmarshalMessage(&msg); err != nil {
					return err
				}
				delivered <- msg
				return nil
			}}}))
			t.Cleanup(func() { _ = broker.ShutdownWorkers(context.Background()) })
			err := job.QueueExpiredLinkReports(ctx, broker, inst, now)
			if tc.wantErr {
				require.Error(t, err)
				if tc.mode == "error" {
					require.ErrorContains(t, err, "failure")
				}
				if tc.mode == "decode" {
					var reports []*publiclink.Report
					decodeErr := json.Unmarshal([]byte(`[{"next_attempt":42}]`), &reports)
					require.Error(t, decodeErr)
					require.EqualError(t, err, decodeErr.Error())
				}
				if tc.mode == "cancel" || tc.mode == "cancel-read" {
					require.ErrorIs(t, err, context.Canceled)
				}
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantBatches, batches, "checkpoint batch sizes")
			require.Equal(t, int32(len(tc.want)), created.Load(), "persisted job count")
			seen := make(map[string]bool)
			for range tc.want {
				select {
				case msg := <-delivered:
					require.False(t, seen[msg.LinkID])
					seen[msg.LinkID] = true
					require.True(t, msg.Expiry.Equal(at))
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for delivered job")
				}
			}
			for _, n := range tc.want {
				require.True(t, seen[fmt.Sprintf("link-%03d", n)])
			}
			select {
			case msg := <-delivered:
				t.Fatalf("unexpected job: %+v", msg)
			default:
			}
		})
	}
}
