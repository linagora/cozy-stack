package mails

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/move"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/safehttp"
	"github.com/cozy/cozy-stack/tests/testutils"
	webinstances "github.com/cozy/cozy-stack/web/instances"
	"github.com/cozy/cozy-stack/worker/moves"
	"github.com/cozy/gomail"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func reportJobIDs(t *testing.T, inst *instance.Instance) []string {
	t.Helper()
	ids := []string{}
	err := couchdb.ForeachDocs(inst, consts.Jobs, func(id string, raw json.RawMessage) error {
		var j job.Job
		if err := json.Unmarshal(raw, &j); err != nil {
			return err
		}
		if j.WorkerType == "expired-link-report" {
			ids = append(ids, id)
		}
		return nil
	})
	require.NoError(t, err)
	sort.Strings(ids)
	return ids
}

func freshReportAuthority(t *testing.T, inst *instance.Instance) *instance.Instance {
	t.Helper()
	var latest instance.Instance
	require.NoError(t, couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, inst.ID(), &latest))
	return &latest
}

// The importer receives real HTTP manifest/ZIP bytes and writes real Couch/VFS
// state. Its client is loopback-only test infrastructure, not a mocked import.
func reportImportSource(t *testing.T, beforeManifest func(http.ResponseWriter) bool, beforeData func() bool) move.ImportOptions {
	t.Helper()
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	contents := []byte("actual imported bytes")
	sum := md5.Sum(contents)
	file, err := vfs.NewFileDoc("imported.txt", consts.RootDirID, int64(len(contents)), sum[:], "text/plain", "text", time.Now(), false, false, false, nil)
	require.NoError(t, err)
	file.SetID("r2-import-file")
	encoded, err := json.Marshal(file)
	require.NoError(t, err)
	for _, entry := range []struct{ name, content string }{
		{move.ExportDataDir + "/io.cozy.notes/r2-note.json", `{"_id":"r2-note","body":"actually imported"}`},
		{move.ExportDataDir + "/" + consts.Files + "/r2-import-file.json", string(encoded)},
		{"My Cozy/Files/imported.txt", string(contents)},
	} {
		w, err := zw.Create(entry.name)
		require.NoError(t, err)
		_, err = io.WriteString(w, entry.content)
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/move/exports/r2":
			if beforeManifest != nil && !beforeManifest(w) {
				return
			}
			w.Header().Set("Content-Type", "application/vnd.api+json")
			_, _ = io.WriteString(w, `{"data":{"type":"io.cozy.exports","attributes":{"state":"done","parts_cursors":[]}}}`)
		case "/move/exports/data/r2":
			if beforeData != nil && !beforeData() {
				_, _ = io.WriteString(w, "not a zip")
				return
			}
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	previous := safehttp.ClientWithKeepAlive
	safehttp.ClientWithKeepAlive = server.Client()
	t.Cleanup(func() { safehttp.ClientWithKeepAlive = previous })
	return move.ImportOptions{SettingsURL: server.URL + "/#/exports/r2", ManifestURL: server.URL + "/move/exports/r2"}
}

// reportExportArchive keeps the real exporter output under t.TempDir, even
// when the instance's VFS uses the in-memory backend.
type reportExportArchive string

func (a reportExportArchive) OpenArchive(*instance.Instance, *move.ExportDoc) (io.ReadCloser, error) {
	return os.Open(string(a))
}

func (a reportExportArchive) CreateArchive(*move.ExportDoc) (io.WriteCloser, error) {
	return os.Create(string(a))
}

func (a reportExportArchive) RemoveArchives([]*move.ExportDoc) error {
	return os.Remove(string(a))
}

func testMovedExpiredLinkReport(t *testing.T, expiry time.Time, messages <-chan capturedReportMail) {
	source := testutils.NewSetup(t, t.Name()+"-source").GetTestInstance(&lifecycle.Options{Email: "source-owner@example.net", Locale: "en"})
	futureExpiry := expiry.Add(48 * time.Hour)
	past := newReportLink(t, source, expiry)
	future := newReportLink(t, source, futureExpiry)
	event := &publiclink.Event{LinkID: past.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Minute), IP: "192.0.2.42"}
	require.NoError(t, couchdb.CreateDoc(source, event))

	// Publish a real CSV but leave its delivery pending on the source.
	func() {
		previous := config.GetConfig().Mail
		config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
		defer func() { config.GetConfig().Mail = previous }()
		require.Error(t, runReport(t, source, past, expiry))
	}()
	state := getReport(t, source, past, expiry)
	require.Equal(t, "file-ready", state.Status)
	rows, _ := readReport(t, source, state)
	require.Len(t, rows, 2)
	sourceDir, err := source.ReportsDirectoryID()
	require.NoError(t, err)

	archiver := reportExportArchive(filepath.Join(t.TempDir(), "export.tar.gz"))
	exported, err := move.CreateExport(source, move.ExportOptions{WithDoctypes: []string{
		consts.Files, consts.Permissions, consts.PublicLinkEvents, consts.PublicLinkReports,
	}}, archiver)
	require.NoError(t, err)
	require.Empty(t, exported.PartsCursors, "the small fixture must fit in one part")
	cursor, err := move.ParseCursor(exported, "")
	require.NoError(t, err)
	var archive bytes.Buffer
	require.NoError(t, move.ExportCopyData(&archive, source, exported, archiver, cursor))
	zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
	require.NoError(t, err)
	reportPrefix := move.ExportDataDir + "/" + consts.PublicLinkReports + "/"
	var reportEntries []string
	for _, file := range zr.File {
		if strings.HasPrefix(file.Name, reportPrefix) {
			reportEntries = append(reportEntries, strings.TrimPrefix(file.Name, reportPrefix))
		}
	}
	require.ElementsMatch(t, []string{state.ID() + ".json", "reports-directory.json"}, reportEntries)

	e := echo.New()
	e.GET("/move/exports/report", func(c echo.Context) error {
		return jsonapi.Data(c, http.StatusOK, exported, nil)
	})
	e.GET("/move/exports/data/report", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/zip", archive.Bytes())
	})
	server := httptest.NewServer(e)
	t.Cleanup(server.Close)
	previous := safehttp.ClientWithKeepAlive
	safehttp.ClientWithKeepAlive = server.Client()
	t.Cleanup(func() { safehttp.ClientWithKeepAlive = previous })

	for _, mode := range []string{"move", "restore"} {
		t.Run(mode, func(t *testing.T) {
			dest := testutils.NewSetup(t, t.Name()).GetTestInstance(&lifecycle.Options{Email: "moved-owner@example.net", Locale: "en"})
			opts := move.ImportOptions{ManifestURL: server.URL + "/move/exports/report"}
			if mode == "move" {
				opts.MoveFrom = &move.FromOptions{URL: server.URL}
			}
			_, err := move.Import(context.Background(), dest, opts)
			require.NoError(t, err)
			paused, err := dest.ReportsPaused()
			require.NoError(t, err)
			require.False(t, paused)

			importedRows, imported := readReport(t, dest, state)
			require.Equal(t, sourceDir, imported.DirID)
			require.Equal(t, rows, importedRows)
			var historical publiclink.Event
			require.NoError(t, couchdb.GetDoc(dest, consts.PublicLinkEvents, event.ID(), &historical))
			require.Equal(t, event.LinkID, historical.LinkID)
			require.Equal(t, event.IP, historical.IP)
			require.Equal(t, event.Action, historical.Action)
			require.True(t, event.OccurredAt.Equal(historical.OccurredAt))
			_, err = dest.ReportsDirectoryID()
			require.True(t, couchdb.IsNotFoundError(err), "imported binding must not be authoritative: %v", err)
			var checkpoint publiclink.Report
			err = couchdb.GetDoc(dest, consts.PublicLinkReports, state.ID(), &checkpoint)
			require.True(t, couchdb.IsNotFoundError(err), "imported checkpoint must not be authoritative: %v", err)

			for _, link := range []*permission.Permission{past, future} {
				p, err := permission.GetPermissionByIDIncludingExpired(dest, link.ID())
				if mode == "restore" {
					require.True(t, couchdb.IsNotFoundError(err), "a restore must not import public links: %v", err)
					continue
				}
				require.NoError(t, err)
				require.Equal(t, link.ExpiresAt.(time.Time).Format(time.RFC3339Nano), p.ExpiresAt)
				require.True(t, strings.HasPrefix(p.Rev(), "1-"), "opt-in must be removed on the first write, not in a later update")
				require.Nil(t, p.ActivityReportEnabled)
				require.NotEmpty(t, p.Codes["link"])
				require.NotEqual(t, link.Codes["link"], p.Codes["link"])
				_, enabled := p.ActivityReportExpiry()
				require.False(t, enabled, "no moved link may opt in automatically, including future expirations")
			}

			// Sweep past both expirations, then execute the message a job could
			// have queued before reset. Neither path creates new authority.
			jobs := reportJobIDs(t, dest)
			require.NoError(t, job.QueueExpiredLinkReports(context.Background(), job.System(), dest, futureExpiry.Add(time.Second)))
			require.Equal(t, jobs, reportJobIDs(t, dest))
			require.NoError(t, runReport(t, dest, past, expiry))
			unchanged, err := dest.VFS().FileByID(imported.ID())
			require.NoError(t, err)
			require.Equal(t, imported.Rev(), unchanged.Rev())
			require.NoError(t, dest.VFS().DestroyFile(unchanged))
			require.NoError(t, runReport(t, dest, past, expiry))
			_, err = dest.VFS().FileByID(imported.ID())
			require.True(t, os.IsNotExist(err), "a removed imported CSV must not be regenerated: %v", err)
			err = couchdb.GetDoc(dest, consts.PublicLinkReports, state.ID(), &checkpoint)
			require.True(t, couchdb.IsNotFoundError(err))
			select {
			case got := <-messages:
				t.Fatalf("inherited report sent mail: %s", got.recipient)
			default:
			}

			local := newReportLink(t, dest, expiry)
			require.NoError(t, runReport(t, dest, local, expiry))
			localState := getReport(t, dest, local, expiry)
			require.Equal(t, "sent", localState.Status)
			require.NotEqual(t, sourceDir, localState.DirectoryID, "a new local report must not adopt an imported directory")
			select {
			case got := <-messages:
				require.Contains(t, got.recipient, "moved-owner@example.net")
				_, body := reportMailText(t, got.raw)
				require.Contains(t, body, localState.FileID)
			case <-time.After(5 * time.Second):
				t.Fatal("new locally authorized report was not delivered over SMTP")
			}
		})
	}
}

func assertReportImportPaused(t *testing.T, inst *instance.Instance) {
	t.Helper()
	latest := freshReportAuthority(t, inst)
	require.True(t, latest.ReportImportPending)
	// A fresh process/old job cannot override persistent state with cached fields.
	latest.ReportImportPending = false
	latest.ReportImportID = ""
	paused, err := latest.ReportsPaused()
	require.NoError(t, err)
	require.True(t, paused)
}

func testExpiredLinkReportImport(t *testing.T) {
	// Reset installs the normal apps opportunistically. No registry or package
	// download belongs to these tests, so that unrelated path has no registry.
	registries := config.GetConfig().Registries
	config.GetConfig().Registries = map[string][]*url.URL{}
	t.Cleanup(func() { config.GetConfig().Registries = registries })
	dialer, messages := reportSMTP(t)
	previous := config.GetConfig().Mail
	config.GetConfig().Mail = dialer
	t.Cleanup(func() { config.GetConfig().Mail = previous })
	expiry := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)

	t.Run("move preserves CSV and history but never inherits automatic report mail", func(t *testing.T) {
		testMovedExpiredLinkReport(t, expiry, messages)
	})

	t.Run("legacy importing is durable and unreadable settings fail closed", func(t *testing.T) {
		host := testutils.NewSetup(t, t.Name()).GetTestInstance()
		settings, err := host.SettingsDocument()
		require.NoError(t, err)
		settings.M["importing"] = true
		require.NoError(t, couchdb.UpdateDoc(host, settings))
		paused, err := host.ReportsPaused()
		require.NoError(t, err)
		require.True(t, paused)
		delete(settings.M, "importing")
		require.NoError(t, couchdb.UpdateDoc(host, settings))
		assertReportImportPaused(t, host)
		// An old already-queued import may establish the first local generation.
		admission := host.ReportAdmissionLock()
		require.NoError(t, admission.Lock())
		err = host.AdmitReportImport("")
		admission.Unlock()
		require.NoError(t, err)
		require.NotEmpty(t, freshReportAuthority(t, host).ReportImportID)

		malformed := testutils.NewSetup(t, t.Name()+"-malformed").GetTestInstance()
		settings, err = malformed.SettingsDocument()
		require.NoError(t, err)
		settings.M["importing"] = "unknown"
		require.NoError(t, couchdb.UpdateDoc(malformed, settings))
		paused, err = malformed.ReportsPaused()
		require.Error(t, err)
		require.True(t, paused)

		other := testutils.NewSetup(t, t.Name()+"-unreadable").GetTestInstance()
		p := newReportLink(t, other, expiry)
		settings, err = other.SettingsDocument()
		require.NoError(t, err)
		require.NoError(t, couchdb.DeleteDoc(other, settings))
		jobs := reportJobIDs(t, other)
		require.Error(t, job.QueueExpiredLinkReports(context.Background(), job.System(), other, time.Now()))
		require.Error(t, runReport(t, other, p, expiry))
		require.Equal(t, jobs, reportJobIDs(t, other))
		missing := other.Clone().(*instance.Instance)
		missing.DocID = "missing-report-authority"
		require.Error(t, job.QueueExpiredLinkReports(context.Background(), job.System(), missing, time.Now()))
		require.Error(t, runReport(t, missing, p, expiry))
		require.Equal(t, jobs, reportJobIDs(t, other))
		var checkpoint publiclink.Report
		err = couchdb.GetDoc(other, consts.PublicLinkReports, publiclink.ReportID(p.ID(), expiry), &checkpoint)
		require.True(t, couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err))
	})

	t.Run("real queued import pauses old report jobs and resumes only after success", func(t *testing.T) {
		host := testutils.NewSetup(t, t.Name()).GetTestInstance(&lifecycle.Options{Email: "import-owner@example.net", Locale: "en"})
		p := newReportLink(t, host, expiry)
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		opts := reportImportSource(t, func(http.ResponseWriter) bool { close(entered); <-release; return true }, nil)
		t.Cleanup(unblock)
		require.NoError(t, move.ScheduleImport(context.Background(), host, opts))
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("the real import worker did not fetch its manifest")
		}
		generation := freshReportAuthority(t, host).ReportImportID
		require.NotEmpty(t, generation)
		jobs := reportJobIDs(t, host)
		require.NoError(t, job.QueueExpiredLinkReports(context.Background(), job.System(), host, time.Now()))
		require.NoError(t, runReport(t, host, p, expiry))
		require.Equal(t, jobs, reportJobIDs(t, host))
		var checkpoint publiclink.Report
		err := couchdb.GetDoc(host, consts.PublicLinkReports, publiclink.ReportID(p.ID(), expiry), &checkpoint)
		require.True(t, couchdb.IsNotFoundError(err) || couchdb.IsNoDatabaseError(err))
		unblock()
		require.Eventually(t, func() bool {
			paused, err := host.ReportsPaused()
			return err == nil && !paused
		}, 20*time.Second, 20*time.Millisecond)
		latest := freshReportAuthority(t, host)
		require.Equal(t, generation, latest.ReportImportID)
		require.False(t, latest.ReportImportPending)
		require.Eventually(t, func() bool {
			done := false
			err := couchdb.ForeachDocs(host, consts.Jobs, func(_ string, raw json.RawMessage) error {
				var j job.Job
				if err := json.Unmarshal(raw, &j); err != nil {
					return err
				}
				if j.WorkerType == "import" && j.State == job.Done {
					done = true
				}
				return nil
			})
			return err == nil && done
		}, 5*time.Second, 10*time.Millisecond)
		settings, err := host.SettingsDocument()
		require.NoError(t, err)
		require.NotContains(t, settings.M, "importing")
		require.False(t, move.GetStore().AllowDeleteAccounts(host))
		var imported couchdb.JSONDoc
		require.NoError(t, couchdb.GetDoc(host, "io.cozy.notes", "r2-note", &imported))
		require.Equal(t, "actually imported", imported.M["body"])
		file, err := host.VFS().FileByID("r2-import-file")
		require.NoError(t, err)
		in, err := host.VFS().OpenFile(file)
		require.NoError(t, err)
		body, err := io.ReadAll(in)
		require.NoError(t, err)
		require.NoError(t, in.Close())
		require.Equal(t, "actual imported bytes", string(body))
		_, err = permission.GetPermissionByIDIncludingExpired(host, p.ID())
		require.True(t, couchdb.IsNotFoundError(err), "the real reset removed the old permission")

		// A new locally authorized report proves scheduling resumed. This does
		// NOT claim that an imported pending report was reanchored (R3).
		local := newReportLink(t, host, expiry)
		require.NoError(t, job.QueueExpiredLinkReports(context.Background(), job.System(), host, time.Now()))
		require.Eventually(t, func() bool {
			var state publiclink.Report
			err := couchdb.GetDoc(host, consts.PublicLinkReports, publiclink.ReportID(local.ID(), expiry), &state)
			return err == nil && state.Status == "sent"
		}, 10*time.Second, 20*time.Millisecond)
		state := getReport(t, host, local, expiry)
		require.Equal(t, 1, state.Attempts)
		for {
			select {
			case mail := <-messages:
				_, text := reportMailText(t, mail.raw)
				if strings.Contains(text, state.FileID) {
					require.Contains(t, mail.recipient, "import-owner@example.net")
					return
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resumed report was not delivered over SMTP")
			}
		}
	})

	t.Run("admin manifest imports can repeat after an earlier generation", func(t *testing.T) {
		host := testutils.NewSetup(t, t.Name()).GetTestInstance()
		opts := reportImportSource(t, nil, nil)
		// Exercise the production admin handler's wiring, not admin auth, which
		// belongs to the outer server middleware and is unchanged here.
		e := echo.New()
		webinstances.Routes(e.Group("/instances"))
		server := httptest.NewServer(e)
		t.Cleanup(server.Close)
		previousGeneration := ""
		for attempt := 0; attempt < 2; attempt++ {
			marker, err := vfs.Create(host.VFS(), "/before-import.txt")
			require.NoError(t, err)
			require.NoError(t, marker.Close())
			resp, err := http.Post(server.URL+"/instances/"+host.Domain+"/import?manifest_url="+url.QueryEscape(opts.ManifestURL), "", nil)
			require.NoError(t, err)
			require.NoError(t, resp.Body.Close())
			require.Equal(t, http.StatusNoContent, resp.StatusCode)
			require.Eventually(t, func() bool {
				latest := freshReportAuthority(t, host)
				return latest.ReportImportID != previousGeneration && !latest.ReportImportPending
			}, 20*time.Second, 20*time.Millisecond)
			previousGeneration = freshReportAuthority(t, host).ReportImportID
			_, err = host.VFS().FileByPath("/before-import.txt")
			require.True(t, os.IsNotExist(err), "each admin import really reset the VFS: %v", err)
			var imported couchdb.JSONDoc
			require.NoError(t, couchdb.GetDoc(host, "io.cozy.notes", "r2-note", &imported))
			require.Equal(t, "actually imported", imported.M["body"])
		}
	})

	for _, scenario := range []string{"manifest failure", "cancelled", "partial import", "cleanup failure", "panic"} {
		t.Run(scenario+" never clears the persistent pause", func(t *testing.T) {
			host := testutils.NewSetup(t, t.Name()).GetTestInstance()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := reportImportSource(t, func(w http.ResponseWriter) bool {
				if scenario == "manifest failure" {
					http.Error(w, "fixture failure", http.StatusServiceUnavailable)
					return false
				}
				if scenario == "cancelled" {
					cancel()
				}
				return true
			}, func() bool {
				if scenario == "cleanup failure" {
					settings, err := host.SettingsDocument()
					require.NoError(t, err)
					require.NoError(t, couchdb.DeleteDoc(host, settings))
				}
				return scenario != "partial import"
			})
			client := safehttp.ClientWithKeepAlive
			if scenario == "panic" {
				base := client.Transport
				if base == nil {
					base = http.DefaultTransport
				}
				client.Transport = &reportTransport{base: base, before: func(*http.Request) { panic("fixture import panic") }}
			}
			var importErr error
			func() {
				defer func() {
					if r := recover(); r != nil {
						require.Equal(t, "panic", scenario)
						importErr = errors.New("fixture panic")
					}
				}()
				_, importErr = move.Import(ctx, host, opts)
			}()
			require.Error(t, importErr)
			assertReportImportPaused(t, host)
			require.False(t, move.GetStore().AllowDeleteAccounts(host))
		})
	}

	t.Run("cancellation during the completion read leaves reports paused", func(t *testing.T) {
		host := testutils.NewSetup(t, t.Name()).GetTestInstance()
		var dataStarted, unblocked atomic.Bool
		opts := reportImportSource(t, nil, func() bool { dataStarted.Store(true); return true })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		entered, gate := make(chan struct{}), make(chan struct{})
		var once, releaseOnce sync.Once
		release := func() { releaseOnce.Do(func() { close(gate) }) }
		client := config.CouchClient()
		base := client.Transport
		client.Transport = &reportTransport{base: base, before: func(req *http.Request) {
			if req.Method != http.MethodPut || req.GetBody == nil || !dataStarted.Load() || !strings.HasSuffix(req.URL.Path, host.ID()) {
				return
			}
			in, err := req.GetBody()
			if err != nil {
				return
			}
			defer in.Close()
			var outgoing instance.Instance
			if json.NewDecoder(in).Decode(&outgoing) == nil && !outgoing.Blocked && outgoing.ReportImportPending {
				unblocked.Store(true)
			}
		}, after: func(req *http.Request) {
			if req.Method == http.MethodGet && unblocked.Load() && strings.HasSuffix(req.URL.Path, host.ID()) {
				once.Do(func() { close(entered); <-gate })
			}
		}}
		t.Cleanup(func() { client.Transport = base })
		done, finished := make(chan error, 1), make(chan struct{})
		go func() { defer close(finished); _, err := move.Import(ctx, host, opts); done <- err }()
		t.Cleanup(func() {
			cancel()
			release()
			select {
			case <-finished:
			case <-time.After(15 * time.Second):
				t.Error("import did not stop after releasing completion I/O")
			}
		})
		select {
		case <-entered:
		case <-time.After(20 * time.Second):
			t.Fatal("real completion CouchDB read was not reached")
		}
		cancel()
		release()
		require.ErrorIs(t, <-done, context.Canceled)
		assertReportImportPaused(t, host)
	})

	t.Run("completion CAS cannot overwrite a newer pending generation", func(t *testing.T) {
		host := testutils.NewSetup(t, t.Name()).GetTestInstance()
		opts := reportImportSource(t, nil, nil)
		client := config.CouchClient()
		base := client.Transport
		client.Transport = &reportTransport{base: base, before: func(req *http.Request) {
			if req.Method != http.MethodPut || req.GetBody == nil || !strings.HasSuffix(req.URL.Path, host.ID()) {
				return
			}
			in, err := req.GetBody()
			require.NoError(t, err)
			defer in.Close()
			var outgoing instance.Instance
			if json.NewDecoder(in).Decode(&outgoing) != nil || outgoing.ReportImportPending || outgoing.ReportImportID == "" {
				return
			}
			latest := freshReportAuthority(t, host)
			latest.ReportImportID += "-concurrent"
			require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, latest))
		}}
		defer func() { client.Transport = base }()
		_, err := move.Import(context.Background(), host, opts)
		require.Error(t, err)
		latest := freshReportAuthority(t, host)
		require.True(t, strings.HasSuffix(latest.ReportImportID, "-concurrent"))
		assertReportImportPaused(t, host)
	})

	t.Run("superseded and completed imports cannot reset or notify", func(t *testing.T) {
		host := testutils.NewSetup(t, t.Name()).GetTestInstance()
		opts := reportImportSource(t, nil, nil)
		var callbacks atomic.Int64
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			callbacks.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(peer.Close)
		previous := safehttp.DefaultClient
		safehttp.DefaultClient = peer.Client()
		t.Cleanup(func() { safehttp.DefaultClient = previous })
		opts.MoveFrom = &move.FromOptions{URL: peer.URL, Token: "fixture-only"}
		admission := host.ReportAdmissionLock()
		require.NoError(t, admission.Lock())
		old, err := host.PauseReportsForImport()
		require.NoError(t, err)
		current, err := host.PauseReportsForImport()
		admission.Unlock()
		require.NoError(t, err)
		for _, generation := range []string{old, ""} {
			opts.ReportGeneration = generation
			msg, err := job.NewMessage(opts)
			require.NoError(t, err)
			j := job.NewJob(host, &job.JobRequest{WorkerType: "import", Message: msg})
			ctx, cancel := job.NewTaskContext("stale-import", j, host)
			before := freshReportAuthority(t, host)
			jobs, err := couchdb.CountNormalDocs(host, consts.Jobs)
			require.NoError(t, err)
			require.NoError(t, moves.ImportWorker(ctx))
			cancel()
			require.Equal(t, before, freshReportAuthority(t, host))
			afterJobs, err := couchdb.CountNormalDocs(host, consts.Jobs)
			require.NoError(t, err)
			require.Equal(t, jobs, afterJobs, "stale jobs must not enqueue failure mail")
			require.Zero(t, callbacks.Load(), "stale jobs must not abort/finalize the peer")
		}
		require.NoError(t, admission.Lock())
		require.NoError(t, host.CompleteReportImport(context.Background(), current))
		admission.Unlock()
		opts.ReportGeneration = current
		_, err = move.Import(context.Background(), host, opts)
		require.ErrorIs(t, err, instance.ErrStaleReportImport)
		require.False(t, freshReportAuthority(t, host).ReportImportPending)
	})
}
