package mails

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http"
	netmail "net/mail"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/sharing"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/files"
	webpermissions "github.com/cozy/cozy-stack/web/permissions"
	_ "github.com/cozy/cozy-stack/web/statik"
	"github.com/cozy/gomail"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

type capturedReportMail struct{ recipient, raw string }

type reportTransport struct {
	base   http.RoundTripper
	before func(*http.Request)
	after  func(*http.Request)
}

func (tr *reportTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tr.before != nil {
		tr.before(req)
	}
	resp, err := tr.base.RoundTrip(req)
	if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 && tr.after != nil {
		tr.after(req)
	}
	return resp, err
}

// A real local SMTP boundary: assertions below inspect MIME delivered over TCP,
// not an intercepted sendMail call or configured mock result.
func reportSMTP(t *testing.T) (*gomail.DialerOptions, <-chan capturedReportMail) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	messages := make(chan capturedReportMail, 30)
	var wg sync.WaitGroup
	done := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
				r := textproto.NewReader(bufio.NewReader(conn))
				w := bufio.NewWriter(conn)
				reply := func(s string) { _, _ = fmt.Fprintf(w, "%s\r\n", s); _ = w.Flush() }
				reply("220 report test")
				var to string
				for {
					line, err := r.ReadLine()
					if err != nil {
						return
					}
					switch {
					case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
						reply("250 report test")
					case strings.HasPrefix(line, "MAIL FROM:"):
						reply("250 OK")
					case strings.HasPrefix(line, "RCPT TO:"):
						to = line
						reply("250 OK")
					case line == "DATA":
						reply("354 Continue")
						raw, err := r.ReadDotBytes()
						if err != nil {
							return
						}
						select {
						case messages <- capturedReportMail{to, string(raw)}:
						case <-done:
							return
						}
						reply("250 Accepted")
					case line == "QUIT":
						reply("221 Bye")
						return
					default:
						reply("250 OK")
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { close(done); ln.Close(); wg.Wait() })
	addr := ln.Addr().(*net.TCPAddr)
	return &gomail.DialerOptions{Host: "127.0.0.1", Port: addr.Port, DisableTLS: true}, messages
}

func reportMailText(t *testing.T, raw string) (netmail.Header, string) {
	t.Helper()
	return reportMailPart(t, raw, "text/plain")
}

func reportMailPart(t *testing.T, raw, contentType string) (netmail.Header, string) {
	t.Helper()
	msg, err := netmail.ReadMessage(strings.NewReader(raw))
	require.NoError(t, err)
	media, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	require.NoError(t, err)
	if strings.HasPrefix(media, "multipart/") {
		parts := multipart.NewReader(msg.Body, params["boundary"])
		for {
			p, err := parts.NextPart()
			require.NoError(t, err)
			if strings.HasPrefix(p.Header.Get("Content-Type"), contentType) {
				b, err := io.ReadAll(p)
				require.NoError(t, err)
				return msg.Header, string(b)
			}
		}
	}
	body := io.Reader(msg.Body)
	if msg.Header.Get("Content-Transfer-Encoding") == "quoted-printable" {
		body = quotedprintable.NewReader(body)
	}
	b, err := io.ReadAll(body)
	require.NoError(t, err)
	return msg.Header, string(b)
}

func newReportLink(t *testing.T, inst *instance.Instance, at time.Time) *permission.Permission {
	t.Helper()
	enabled := true
	p := &permission.Permission{Type: permission.TypeShareByLink, ExpiresAt: at, ActivityReportEnabled: &enabled, Codes: map[string]string{"link": "not-a-real-secret"}, Permissions: permission.Set{{Type: consts.Files, Values: []string{"source-file"}}}, Metadata: &metadata.CozyMetadata{UpdatedByApps: []*metadata.UpdatedByAppEntry{{Instance: "recipient-other-instance.example"}}}}
	require.NoError(t, couchdb.CreateDoc(inst, p))
	return p
}

func runReport(t *testing.T, inst *instance.Instance, p *permission.Permission, at time.Time) error {
	t.Helper()
	msg, err := job.NewMessage(publiclink.ReportMessage{LinkID: p.ID(), Expiry: at})
	require.NoError(t, err)
	j := job.NewJob(inst, &job.JobRequest{WorkerType: "expired-link-report", Message: msg})
	ctx, cancel := job.NewTaskContext("report-test", j, inst)
	defer cancel()
	return expiredLinkReport(ctx)
}

func getReport(t *testing.T, inst *instance.Instance, p *permission.Permission, at time.Time) *publiclink.Report {
	t.Helper()
	var state publiclink.Report
	require.NoError(t, couchdb.GetDoc(inst, consts.PublicLinkReports, publiclink.ReportID(p.ID(), at), &state))
	return &state
}

func readReport(t *testing.T, inst *instance.Instance, state *publiclink.Report) ([][]string, *vfs.FileDoc) {
	t.Helper()
	file, err := inst.VFS().FileByID(state.FileID)
	require.NoError(t, err)
	in, err := inst.VFS().OpenFile(file)
	require.NoError(t, err)
	defer in.Close()
	rows, err := csv.NewReader(in).ReadAll()
	require.NoError(t, err)
	return rows, file
}

func retryReport(t *testing.T, inst *instance.Instance, state *publiclink.Report) {
	t.Helper()
	state.NextAttempt = time.Time{}
	require.NoError(t, couchdb.UpdateDoc(inst, state))
}

func TestExpiredLinkReport(t *testing.T) {
	if testing.Short() {
		t.Skip("requires an instance")
	}
	t.Setenv("COZY_SKIP_LOADING_TRIGGERS", "1")
	// Match web.LoadSupportedLocales without importing the server package into
	// its mail worker: use the real registered assets and translation parser.
	for _, locale := range []string{"en", "fr"} {
		po, err := assets.Open("/locales/"+locale+".po", config.DefaultInstanceContext)
		require.NoError(t, err)
		raw, err := io.ReadAll(po)
		require.NoError(t, err)
		i18n.LoadLocale(locale, "", raw)
	}
	node, err := exec.LookPath("node")
	require.NoError(t, err, "real HTML renderer requires Node")
	oldCmd := config.GetConfig().Konnectors.Cmd
	config.GetConfig().Konnectors.Cmd = node
	t.Cleanup(func() { config.GetConfig().Konnectors.Cmd = oldCmd })
	setup := testutils.NewSetup(t, t.Name())
	inst := setup.GetTestInstance(&lifecycle.Options{Email: "host-owner@example.net", Locale: "en"})
	dialer, messages := reportSMTP(t)
	original := config.GetConfig().Mail
	config.GetConfig().Mail = dialer
	t.Cleanup(func() { config.GetConfig().Mail = original })
	expiry := time.Now().Add(-48 * time.Hour).UTC().Truncate(time.Second)

	t.Run("HTTP collection scanner and actual mail worker end to end", func(t *testing.T) {
		setup := testutils.NewSetup(t, t.Name())
		host := setup.GetTestInstance(&lifecycle.Options{Email: "pipeline-owner@example.net", Locale: "en"})
		host.FeatureFlags = map[string]interface{}{publiclink.FlagEnabled: true}
		require.NoError(t, instance.Update(host))
		source, err := vfs.Create(host.VFS(), "/source.txt")
		require.NoError(t, err)
		_, err = io.WriteString(source, "real source bytes")
		require.NoError(t, err)
		require.NoError(t, source.Close())
		doc, err := host.VFS().FileByPath("/source.txt")
		require.NoError(t, err)
		_, ownerToken := setup.GetTestClient("io.cozy.files:GET")
		server := setup.GetTestServerMultipleRoutes(map[string]func(*echo.Group){
			"/permissions": webpermissions.Routes, "/files": files.Routes,
		})
		request := func(method, path, body, bearer string) (int, []byte) {
			req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
			require.NoError(t, err)
			req.Host = host.Domain
			req.Header.Set("Authorization", "Bearer "+bearer)
			req.Header.Set("Content-Type", "application/vnd.api+json")
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			return resp.StatusCode, raw
		}
		body := fmt.Sprintf(`{"data":{"type":"io.cozy.permissions","attributes":{"activity_report_enabled":true,"expires_at":%q,"permissions":{"files":{"type":"io.cozy.files","verbs":["GET"],"values":[%q]}}}}}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339), doc.ID())
		status, raw := request("POST", "/permissions?codes=link", body, ownerToken)
		require.Equal(t, http.StatusOK, status, string(raw))
		var created struct{ Data struct{ ID string } }
		require.NoError(t, json.Unmarshal(raw, &created))
		p, err := permission.GetPermissionByIDIncludingExpired(host, created.Data.ID)
		require.NoError(t, err)
		status, raw = request("GET", "/files/download/"+doc.ID()+"?Dl=1", "", p.Codes["link"])
		require.Equal(t, http.StatusOK, status)
		require.Equal(t, "real source bytes", string(raw))
		var events []publiclink.Event
		require.Eventually(t, func() bool {
			events = nil
			err := publiclink.ForEachEvent(context.Background(), host, p.ID(), func(e publiclink.Event) error { events = append(events, e); return nil })
			return err == nil && len(events) == 2
		}, 5*time.Second, 10*time.Millisecond)
		require.Equal(t, publiclink.Created, events[0].Action)
		require.Equal(t, publiclink.Downloaded, events[1].Action)
		at := time.Now().UTC()
		p.ExpiresAt = at
		require.NoError(t, couchdb.UpdateDoc(host, p))
		status, _ = request("GET", "/files/download/"+doc.ID()+"?Dl=1", "", p.Codes["link"])
		require.NotEqual(t, http.StatusOK, status)
		require.NoError(t, job.QueueExpiredLinkReports(context.Background(), job.System(), host, time.Now()))
		var state publiclink.Report
		require.Eventually(t, func() bool {
			err := couchdb.GetDoc(host, consts.PublicLinkReports, publiclink.ReportID(p.ID(), at), &state)
			return err == nil && state.Status == "sent"
		}, 10*time.Second, 20*time.Millisecond)
		rows, file := readReport(t, host, &state)
		require.Len(t, rows, 3)
		for i, event := range events {
			require.Equal(t, []string{event.OccurredAt.UTC().Format(time.RFC3339Nano), event.IP, event.Identity, string(event.Action)}, rows[i+1])
		}
		got := <-messages
		require.Contains(t, got.recipient, "pipeline-owner@example.net")
		_, text := reportMailText(t, got.raw)
		require.Contains(t, text, file.ID())
		require.NotContains(t, text, p.Codes["link"])
		status, raw = request("GET", "/files/download/"+file.ID(), "", ownerToken)
		require.Equal(t, http.StatusOK, status)
		exported, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
		require.NoError(t, err)
		require.Equal(t, rows, exported)
	})

	t.Run("retained events authenticated file and hosting owner mail", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		for i := 0; i < 6; i++ {
			require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(time.Duration(i-4) * time.Second), IP: "192.0.2." + strconv.Itoa(i), Identity: fmt.Sprintf("visitor-%d <tag>", i)}))
		}
		require.NoError(t, runReport(t, inst, p, expiry))
		state := getReport(t, inst, p, expiry)
		require.Equal(t, "sent", state.Status)
		rows, file := readReport(t, inst, state)
		require.Len(t, rows, 6)
		require.Equal(t, "192.0.2.4", rows[5][1])
		directory, err := inst.VFS().DirByID(file.DirID)
		require.NoError(t, err)
		require.Equal(t, "/Reports", directory.Fullpath)
		var got capturedReportMail
		select {
		case got = <-messages:
		case <-time.After(time.Second):
			t.Fatal("no SMTP mail")
		}
		header, body := reportMailText(t, got.raw)
		require.Contains(t, got.recipient, "host-owner@example.net")
		require.NotContains(t, got.recipient, "recipient-other-instance")
		require.NotEmpty(t, header.Get("Message-ID"))
		_, html := reportMailPart(t, got.raw, "text/html")
		require.Contains(t, html, `alt="Twake Workplace"`)
		require.Contains(t, html, "https://blog.cozy.io/")
		server := setup.GetTestServer("/files", files.Routes)
		_, ownerToken := setup.GetTestClient("io.cozy.files:GET")
		_, narrowToken := setup.GetTestClient("io.cozy.files:GET:unrelated-file")
		active := newReportLink(t, inst, time.Now().Add(time.Hour))
		for _, bearer := range []string{"", narrowToken, active.Codes["link"], ownerToken} {
			req, err := http.NewRequest("GET", server.URL+"/files/download/"+file.ID(), nil)
			require.NoError(t, err)
			req.Host = inst.Domain
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			resp, err := client.Do(req)
			require.NoError(t, err)
			raw, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			if bearer == ownerToken {
				require.Equal(t, http.StatusOK, resp.StatusCode)
				require.Contains(t, string(raw), "visitor-4")
			} else {
				require.NotEqual(t, http.StatusOK, resp.StatusCode)
				require.NotContains(t, string(raw), "visitor-4")
			}
		}
		require.Contains(t, body, "visitor-2")
		require.Contains(t, body, "visitor-4")
		require.Less(t, strings.Index(body, "visitor-4"), strings.Index(body, "visitor-3"))
		require.Less(t, strings.Index(body, "visitor-3"), strings.Index(body, "visitor-2"))
		require.Contains(t, body, "Opened")
		require.NotContains(t, body, "visitor-0")
		require.NotContains(t, body, "visitor-1")
		require.NotContains(t, body, "visitor-5")
		u := inst.SubDomain(consts.DriveSlug)
		u.Fragment = "/folder/" + file.DirID + "/file/" + file.ID()
		require.Contains(t, body, u.String())
		require.NotContains(t, body, "sharecode")
		require.NotContains(t, body, "not-a-real-secret")
		checkpoint, err := json.Marshal(state)
		require.NoError(t, err)
		require.NotContains(t, string(checkpoint), "visitor")
		require.NotContains(t, string(checkpoint), "192.0.2.")
		require.NoError(t, runReport(t, inst, p, expiry.In(time.FixedZone("offset", 3600))))
		require.Equal(t, state.FileID, getReport(t, inst, p, expiry).FileID)
		select {
		case <-messages:
			t.Fatal("duplicate report mail")
		default:
		}
	})

	t.Run("zero activity still mails", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		require.NoError(t, runReport(t, inst, p, expiry))
		rows, _ := readReport(t, inst, getReport(t, inst, p, expiry))
		require.Len(t, rows, 1)
		got := <-messages
		_, body := reportMailText(t, got.raw)
		require.Contains(t, body, "No activity recorded.")
	})

	t.Run("disabled transport retries same published file", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
		require.Error(t, runReport(t, inst, p, expiry))
		state := getReport(t, inst, p, expiry)
		require.Equal(t, "file-ready", state.Status)
		require.Equal(t, 1, state.Attempts)
		_, file := readReport(t, inst, state)
		require.NoError(t, runReport(t, inst, p, expiry))
		require.Equal(t, 1, getReport(t, inst, p, expiry).Attempts)
		config.GetConfig().Mail = dialer
		retryReport(t, inst, state)
		require.NoError(t, runReport(t, inst, p, expiry))
		state = getReport(t, inst, p, expiry)
		require.Equal(t, "sent", state.Status)
		_, same := readReport(t, inst, state)
		require.Equal(t, file.Rev(), same.Rev())
		<-messages
	})

	t.Run("opt out and changed deadline cancel pending delivery", func(t *testing.T) {
		for _, change := range []string{"opt-out", "new-expiry", "revoked"} {
			p := newReportLink(t, inst, expiry)
			config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
			require.Error(t, runReport(t, inst, p, expiry))
			state := getReport(t, inst, p, expiry)
			retryReport(t, inst, state)
			switch change {
			case "opt-out":
				*p.ActivityReportEnabled = false
			case "new-expiry":
				p.ExpiresAt = expiry.Add(-time.Hour)
			case "revoked":
				p.Codes = nil
			}
			require.NoError(t, couchdb.UpdateDoc(inst, p))
			config.GetConfig().Mail = dialer
			require.NoError(t, runReport(t, inst, p, expiry))
			require.Equal(t, "cancelled", getReport(t, inst, p, expiry).Status)
		}
		select {
		case <-messages:
			t.Fatal("cancelled report mailed")
		default:
		}
	})

	t.Run("file survives checkpoint crash without duplication", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
		require.Error(t, runReport(t, inst, p, expiry))
		state := getReport(t, inst, p, expiry)
		_, before := readReport(t, inst, state)
		state.Status = "pending"
		retryReport(t, inst, state)
		config.GetConfig().Mail = dialer
		require.NoError(t, runReport(t, inst, p, expiry))
		_, after := readReport(t, inst, getReport(t, inst, p, expiry))
		require.Equal(t, before.Rev(), after.Rev())
		<-messages
	})

	t.Run("no address creates file with no retroactive mail", func(t *testing.T) {
		other := testutils.NewSetup(t, t.Name()).GetTestInstance()
		p := newReportLink(t, other, expiry)
		require.NoError(t, runReport(t, other, p, expiry))
		state := getReport(t, other, p, expiry)
		require.Equal(t, "no-email", state.Status)
		rows, _ := readReport(t, other, state)
		require.Len(t, rows, 1)
		settings, err := other.SettingsDocument()
		require.NoError(t, err)
		settings.M["email"] = "added-later@example.net"
		require.NoError(t, couchdb.UpdateDoc(other, settings))
		require.NoError(t, runReport(t, other, p, expiry))
		select {
		case <-messages:
			t.Fatal("retroactive report mailed")
		default:
		}
	})

	t.Run("email removed during retries permanently suppresses delivery", func(t *testing.T) {
		other := testutils.NewSetup(t, t.Name()).GetTestInstance(&lifecycle.Options{Email: "before@example.net"})
		p := newReportLink(t, other, expiry)
		config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
		require.Error(t, runReport(t, other, p, expiry))
		config.GetConfig().Mail = dialer
		state := getReport(t, other, p, expiry)
		settings, err := other.SettingsDocument()
		require.NoError(t, err)
		delete(settings.M, "email")
		require.NoError(t, couchdb.UpdateDoc(other, settings))
		retryReport(t, other, state)
		require.NoError(t, runReport(t, other, p, expiry))
		require.Equal(t, "no-email", getReport(t, other, p, expiry).Status)
		settings.M["email"] = "after@example.net"
		require.NoError(t, couchdb.UpdateDoc(other, settings))
		require.NoError(t, runReport(t, other, p, expiry))
		require.Equal(t, state.FileID, getReport(t, other, p, expiry).FileID)
		select {
		case <-messages:
			t.Fatal("retroactive retry mailed")
		default:
		}
	})

	t.Run("shared Reports allows generation and delivery", func(t *testing.T) {
		for _, destination := range []string{"public Reports", "public ancestor", "replication reference", "replication rules"} {
			for _, retry := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/retry=%t", destination, retry), func(t *testing.T) {
					other := testutils.NewSetup(t, t.Name()).GetTestInstance(&lifecycle.Options{Email: "shared-reports-owner@example.net", Locale: "en"})
					fs := other.VFS()
					dirID, err := other.ResolveDirID(consts.Apps + "/reports")
					require.NoError(t, err)
					targetID := dirID
					if destination != "public Reports" {
						dir, err := fs.DirByID(dirID)
						require.NoError(t, err)
						parent, err := vfs.NewDirDoc(fs, "Shared parent", "", nil)
						require.NoError(t, err)
						require.NoError(t, fs.CreateDir(parent))
						_, err = vfs.ModifyDirMetadata(fs, dir, &vfs.DocPatch{DirID: &parent.DocID})
						require.NoError(t, err)
						targetID = parent.ID()
					}
					p := newReportLink(t, other, expiry)
					require.NoError(t, couchdb.CreateDoc(other, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), IP: "192.0.2.42", Identity: "shared-report-visitor"}))
					var publishedRev string
					if retry {
						config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
						t.Cleanup(func() { config.GetConfig().Mail = dialer })
						require.Error(t, runReport(t, other, p, expiry))
						state := getReport(t, other, p, expiry)
						require.Equal(t, "file-ready", state.Status)
						_, file := readReport(t, other, state)
						publishedRev = file.Rev()
						retryReport(t, other, state)
						config.GetConfig().Mail = dialer
					}
					// Keep the sharing in place throughout publication and SMTP delivery.
					switch destination {
					case "public Reports", "public ancestor":
						public := &permission.Permission{Type: permission.TypeShareByLink, Codes: map[string]string{"link": "public"}, Permissions: permission.Set{{Type: consts.Files, Values: []string{targetID}}}}
						require.NoError(t, couchdb.CreateDoc(other, public))
					case "replication reference":
						ref := &couchdb.JSONDoc{Type: consts.Shared, M: map[string]interface{}{"_id": consts.Files + "/" + targetID}}
						require.NoError(t, couchdb.CreateNamedDocWithDB(other, ref))
					case "replication rules":
						s := &sharing.Sharing{Rules: []sharing.Rule{{DocType: consts.Files, Selector: "id", Values: []string{targetID}}}}
						require.NoError(t, couchdb.CreateDoc(other, s))
					}
					require.NoError(t, runReport(t, other, p, expiry))
					state := getReport(t, other, p, expiry)
					require.Equal(t, "sent", state.Status)
					rows, file := readReport(t, other, state)
					require.Len(t, rows, 2)
					require.Equal(t, "shared-report-visitor", rows[1][2])
					require.Equal(t, dirID, file.DirID)
					if retry {
						require.Equal(t, publishedRev, file.Rev())
					}
					select {
					case got := <-messages:
						require.Contains(t, got.recipient, "shared-reports-owner@example.net")
						_, body := reportMailText(t, got.raw)
						require.Contains(t, body, file.ID())
						require.Contains(t, body, "shared-report-visitor")
					case <-time.After(time.Second):
						t.Fatal("shared report mail not delivered")
					}
				})
			}
		}
	})

	t.Run("report destination still rejects trashed ancestors", func(t *testing.T) {
		other := testutils.NewSetup(t, t.Name()).GetTestInstance()
		fs := other.VFS()
		id, err := other.ResolveDirID(consts.Apps + "/reports")
		require.NoError(t, err)
		dir, err := fs.DirByID(id)
		require.NoError(t, err)
		parent, err := vfs.NewDirDoc(fs, "Parent", "", nil)
		require.NoError(t, err)
		require.NoError(t, fs.CreateDir(parent))
		dir, err = vfs.ModifyDirMetadata(fs, dir, &vfs.DocPatch{DirID: &parent.DocID})
		require.NoError(t, err)
		require.NoError(t, checkReportDestination(fs, dir))
		_, err = vfs.TrashDir(fs, parent)
		require.NoError(t, err)
		// Use the original snapshot: the fresh parent must still detect trash.
		require.ErrorIs(t, checkReportDestination(fs, dir), vfs.ErrFileInTrash)
	})

	t.Run("quota never publishes partial reports and retries are bounded", func(t *testing.T) {
		other := testutils.NewSetup(t, t.Name()).GetTestInstance()
		other.BytesDiskQuota = 1
		p := newReportLink(t, other, expiry)
		for attempt := 1; attempt <= 8; attempt++ {
			require.Error(t, runReport(t, other, p, expiry))
			state := getReport(t, other, p, expiry)
			require.Equal(t, attempt, state.Attempts)
			require.LessOrEqual(t, time.Until(state.NextAttempt), 6*time.Hour)
			_, err := other.VFS().FileByID(state.FileID)
			require.True(t, os.IsNotExist(err))
			if attempt < 8 {
				retryReport(t, other, state)
			}
		}
		state := getReport(t, other, p, expiry)
		require.Equal(t, "failed", state.Status)
		other.BytesDiskQuota = 0
		require.NoError(t, runReport(t, other, p, expiry))
		require.Equal(t, 8, getReport(t, other, p, expiry).Attempts)
	})

	t.Run("antivirus after publication does not cancel", func(t *testing.T) {
		for _, status := range []string{vfs.AVStatusPending, vfs.AVStatusError, vfs.AVStatusClean} {
			t.Run(status, func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "publication-visitor"}))
				id := publiclink.ReportID(p.ID(), expiry)
				client := config.CouchClient()
				original := client.Transport
				base := original
				if base == nil {
					base = http.DefaultTransport
				}
				changed := false
				client.Transport = &reportTransport{base: base, after: func(req *http.Request) {
					if changed || req.Method != http.MethodPut || !strings.Contains(req.URL.Path, id) || !strings.Contains(req.URL.Path, "public_links-reports") {
						return
					}
					body, err := req.GetBody()
					require.NoError(t, err)
					var checkpoint publiclink.Report
					require.NoError(t, json.NewDecoder(body).Decode(&checkpoint))
					body.Close()
					if checkpoint.Status != "file-ready" {
						return
					}
					changed = true
					file, err := inst.VFS().FileByID("link-report-" + id)
					require.NoError(t, err)
					file.AntivirusScan = &vfs.AntivirusScan{Status: status}
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				}}
				t.Cleanup(func() { client.Transport = original })
				require.NoError(t, runReport(t, inst, p, expiry))
				client.Transport = original
				require.True(t, changed)
				state := getReport(t, inst, p, expiry)
				require.Equal(t, "sent", state.Status)
				rows, _ := readReport(t, inst, state)
				require.Equal(t, "publication-visitor", rows[1][2])
				got := <-messages
				_, body := reportMailText(t, got.raw)
				require.Contains(t, body, "publication-visitor")
				require.NoError(t, runReport(t, inst, p, expiry))
				select {
				case <-messages:
					t.Fatal("duplicate report mail")
				default:
				}
			})
		}
	})

	t.Run("changes after preview are revalidated", func(t *testing.T) {
		for _, change := range []string{"pending", "error", "clean", "infected", "edited", "head-race", "tail-race"} {
			t.Run(change, func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				id := publiclink.ReportID(p.ID(), expiry)
				require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "preview-visitor"}))
				client := config.CouchClient()
				original := client.Transport
				base := original
				if base == nil {
					base = http.DefaultTransport
				}
				updates := 0
				headRaceTriggered := false
				client.Transport = &reportTransport{base: base, before: func(req *http.Request) {
					if change != "head-race" || req.Method != http.MethodGet || req.URL.Query().Get("revs") != "true" || !strings.Contains(req.URL.Path, "link-report-"+id) {
						return
					}
					headRaceTriggered = true
					file, err := inst.VFS().FileByID("link-report-" + id)
					require.NoError(t, err)
					file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusInfected}
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				}, after: func(req *http.Request) {
					if change == "tail-race" && updates >= 2 && !headRaceTriggered && req.Method == http.MethodGet && req.URL.Query().Get("rev") != "" && strings.Contains(req.URL.Path, "link-report-"+id) {
						headRaceTriggered = true
						file, err := inst.VFS().FileByID("link-report-" + id)
						require.NoError(t, err)
						file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusInfected}
						require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
					}
					if req.Method != http.MethodPut || !strings.Contains(req.URL.Path, id) || !strings.Contains(req.URL.Path, "public_links-reports") {
						return
					}
					body, err := req.GetBody()
					require.NoError(t, err)
					var checkpoint publiclink.Report
					require.NoError(t, json.NewDecoder(body).Decode(&checkpoint))
					body.Close()
					if checkpoint.Status != "file-ready" {
						return
					}
					updates++
					if updates != 2 {
						return
					}
					file, err := inst.VFS().FileByID("link-report-" + id)
					require.NoError(t, err)
					if change == "pending" || change == "error" || change == "clean" {
						file.AntivirusScan = &vfs.AntivirusScan{Status: change}
					} else if change == "head-race" || change == "tail-race" {
						file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusPending}
					} else if change == "infected" {
						file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusInfected}
					} else {
						file.Metadata["owner-edited"] = true
					}
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				}}
				t.Cleanup(func() { client.Transport = original })
				err := runReport(t, inst, p, expiry)
				client.Transport = original
				if change == "head-race" || change == "tail-race" {
					require.Error(t, err)
					require.True(t, headRaceTriggered)
				} else {
					require.NoError(t, err)
				}
				require.GreaterOrEqual(t, updates, 2, "must interpose after preview")
				state := getReport(t, inst, p, expiry)
				if change == "pending" || change == "error" || change == "clean" {
					require.Equal(t, "sent", state.Status)
					rows, _ := readReport(t, inst, state)
					require.Equal(t, "preview-visitor", rows[1][2])
					got := <-messages
					_, body := reportMailText(t, got.raw)
					require.Contains(t, body, "preview-visitor")
					require.NoError(t, runReport(t, inst, p, expiry))
					select {
					case <-messages:
						t.Fatal("duplicate report mail")
					default:
					}
				} else if change == "head-race" || change == "tail-race" {
					retryReport(t, inst, state)
					require.NoError(t, runReport(t, inst, p, expiry))
					require.Equal(t, "cancelled", getReport(t, inst, p, expiry).Status)
					select {
					case <-messages:
						t.Fatal("infected report mailed")
					default:
					}
				} else {
					require.Equal(t, "cancelled", state.Status)
					select {
					case <-messages:
						t.Fatal("changed report mailed")
					default:
					}
				}
			})
		}
	})

	t.Run("antivirus revisions do not cancel pending mail", func(t *testing.T) {
		for _, status := range []string{vfs.AVStatusPending, vfs.AVStatusError, vfs.AVStatusClean} {
			t.Run(status, func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "retry-visitor"}))
				config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
				t.Cleanup(func() { config.GetConfig().Mail = dialer })
				require.Error(t, runReport(t, inst, p, expiry))
				state := getReport(t, inst, p, expiry)
				rows, file := readReport(t, inst, state)
				file.AntivirusScan = &vfs.AntivirusScan{Status: status}
				require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				retryReport(t, inst, state)
				config.GetConfig().Mail = dialer
				require.NoError(t, runReport(t, inst, p, expiry))
				state = getReport(t, inst, p, expiry)
				require.Equal(t, "sent", state.Status)
				after, _ := readReport(t, inst, state)
				require.Equal(t, rows, after)
				got := <-messages
				_, body := reportMailText(t, got.raw)
				require.Contains(t, body, "retry-visitor")
				require.NoError(t, runReport(t, inst, p, expiry))
				select {
				case <-messages:
					t.Fatal("duplicate report mail")
				default:
				}
			})
		}
	})

	t.Run("preview rejects transient altered bytes", func(t *testing.T) {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprint(compact), func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				id := publiclink.ReportID(p.ID(), expiry)
				require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "approved-visitor"}))
				client := config.CouchClient()
				original := client.Transport
				base := original
				if base == nil {
					base = http.DefaultTransport
				}
				var originalBytes []byte
				var originalDoc *couchdb.JSONDoc
				changed, restored := false, false
				writeContent := func(content []byte) {
					file, err := inst.VFS().FileByID("link-report-" + id)
					require.NoError(t, err)
					replacement, err := vfs.NewFileDoc(file.DocName, file.DirID, -1, nil, file.Mime, file.Class, time.Now(), false, false, false, nil)
					require.NoError(t, err)
					replacement.Metadata = file.Metadata
					out, err := inst.VFS().CreateFile(replacement, file)
					require.NoError(t, err)
					_, err = out.Write(content)
					require.NoError(t, err)
					require.NoError(t, out.Close())
				}
				client.Transport = &reportTransport{base: base, before: func(req *http.Request) {
					if !changed || restored || req.Method != http.MethodGet || req.URL.Query().Get("rev") == "" || !strings.Contains(req.URL.Path, "link-report-"+id) {
						return
					}
					restored = true
					file, err := inst.VFS().FileByID("link-report-" + id)
					require.NoError(t, err)
					in, err := inst.VFS().OpenFile(file)
					require.NoError(t, err)
					actual, err := io.ReadAll(in)
					require.NoError(t, err)
					in.Close()
					require.Contains(t, string(actual), "attacker-visitor")
					writeContent(originalBytes)
					current := &couchdb.JSONDoc{Type: consts.Files}
					require.NoError(t, couchdb.GetDoc(inst, consts.Files, "link-report-"+id, current))
					originalRev := originalDoc.Rev()
					originalDoc.SetRev(current.Rev())
					require.NoError(t, couchdb.UpdateDoc(inst, originalDoc))
					if compact {
						require.NoError(t, couchdb.Compact(inst, consts.Files))
						require.Eventually(t, func() bool {
							return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, "link-report-"+id, originalRev, &vfs.FileDoc{}))
						}, 15*time.Second, 100*time.Millisecond)
					}
				}, after: func(req *http.Request) {
					if changed || req.Method != http.MethodPut || !strings.Contains(req.URL.Path, id) || !strings.Contains(req.URL.Path, "public_links-reports") {
						return
					}
					body, err := req.GetBody()
					require.NoError(t, err)
					var checkpoint publiclink.Report
					require.NoError(t, json.NewDecoder(body).Decode(&checkpoint))
					body.Close()
					if checkpoint.Status != "file-ready" {
						return
					}
					changed = true
					file, err := inst.VFS().FileByID("link-report-" + id)
					require.NoError(t, err)
					in, err := inst.VFS().OpenFile(file)
					require.NoError(t, err)
					originalBytes, err = io.ReadAll(in)
					require.NoError(t, err)
					in.Close()
					originalDoc = &couchdb.JSONDoc{Type: consts.Files}
					require.NoError(t, couchdb.GetDoc(inst, consts.Files, file.ID(), originalDoc))
					writeContent([]byte(strings.ReplaceAll(string(originalBytes), "approved-visitor", "attacker-visitor")))
				}}
				t.Cleanup(func() { client.Transport = original })
				err := runReport(t, inst, p, expiry)
				client.Transport = original
				require.True(t, changed)
				require.True(t, restored)
				if compact {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					require.Equal(t, "cancelled", getReport(t, inst, p, expiry).Status)
				}
				select {
				case <-messages:
					t.Fatal("unapproved preview was mailed")
				default:
				}
			})
		}
	})

	t.Run("legacy checkpoint without fingerprint fails closed", func(t *testing.T) {
		for _, withAV := range []bool{false, true} {
			t.Run(fmt.Sprint(withAV), func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
				t.Cleanup(func() { config.GetConfig().Mail = dialer })
				require.Error(t, runReport(t, inst, p, expiry))
				state := getReport(t, inst, p, expiry)
				state.FileFingerprint = ""
				if withAV {
					file, err := inst.VFS().FileByID(state.FileID)
					require.NoError(t, err)
					file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean}
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				}
				retryReport(t, inst, state)
				config.GetConfig().Mail = dialer
				require.NoError(t, runReport(t, inst, p, expiry))
				require.Equal(t, "cancelled", getReport(t, inst, p, expiry).Status)
				select {
				case <-messages:
					t.Fatal("legacy report mailed")
				default:
				}
			})
		}
	})

	t.Run("compaction during publication accepts unchanged file", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		id := publiclink.ReportID(p.ID(), expiry)
		require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "publication-compacted"}))
		client := config.CouchClient()
		original := client.Transport
		base := original
		if base == nil {
			base = http.DefaultTransport
		}
		triggered := false
		client.Transport = &reportTransport{base: base, after: func(req *http.Request) {
			if triggered || req.Method != http.MethodPut || !strings.Contains(req.URL.Path, "link-report-"+id) {
				return
			}
			triggered = true
			file := &couchdb.JSONDoc{Type: consts.Files}
			require.NoError(t, couchdb.GetDoc(inst, consts.Files, "link-report-"+id, file))
			publishedRev := file.Rev()
			file.M["antivirus_scan"] = map[string]interface{}{"status": vfs.AVStatusPending}
			require.NoError(t, couchdb.UpdateDoc(inst, file))
			require.NoError(t, couchdb.Compact(inst, consts.Files))
			require.Eventually(t, func() bool {
				return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, file.ID(), publishedRev, &vfs.FileDoc{}))
			}, 15*time.Second, 100*time.Millisecond)
		}}
		t.Cleanup(func() { client.Transport = original })
		require.NoError(t, runReport(t, inst, p, expiry))
		client.Transport = original
		require.True(t, triggered)
		require.Equal(t, "sent", getReport(t, inst, p, expiry).Status)
		got := <-messages
		_, body := reportMailText(t, got.raw)
		require.Contains(t, body, "publication-compacted")
	})

	t.Run("compaction during publication fingerprint retries fresh head", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		id := publiclink.ReportID(p.ID(), expiry)
		require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "late-compaction"}))
		client := config.CouchClient()
		original := client.Transport
		base := original
		if base == nil {
			base = http.DefaultTransport
		}
		triggered := false
		client.Transport = &reportTransport{base: base, before: func(req *http.Request) {
			if triggered || req.Method != http.MethodGet || req.URL.Query().Get("rev") == "" || !strings.Contains(req.URL.Path, "link-report-"+id) {
				return
			}
			triggered = true
			file := &couchdb.JSONDoc{Type: consts.Files}
			require.NoError(t, couchdb.GetDoc(inst, consts.Files, "link-report-"+id, file))
			publishedRev := file.Rev()
			file.M["antivirus_scan"] = map[string]interface{}{"status": vfs.AVStatusPending}
			require.NoError(t, couchdb.UpdateDoc(inst, file))
			require.NoError(t, couchdb.Compact(inst, consts.Files))
			require.Eventually(t, func() bool {
				return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, file.ID(), publishedRev, &vfs.FileDoc{}))
			}, 15*time.Second, 100*time.Millisecond)
		}}
		t.Cleanup(func() { client.Transport = original })
		require.NoError(t, runReport(t, inst, p, expiry))
		client.Transport = original
		require.True(t, triggered)
		require.Equal(t, "sent", getReport(t, inst, p, expiry).Status)
		got := <-messages
		_, body := reportMailText(t, got.raw)
		require.Contains(t, body, "late-compaction")
	})

	t.Run("compacted report revisions use publication fingerprint", func(t *testing.T) {
		for _, change := range []string{"antivirus", "legacy", "metadata", "unknown field", "content", "content with restored metadata", "observed restored edit", "compacted restored edit"} {
			t.Run(change, func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				require.NoError(t, couchdb.CreateDoc(inst, &publiclink.Event{LinkID: p.ID(), Action: publiclink.Opened, OccurredAt: expiry.Add(-time.Second), Identity: "compaction-visitor"}))
				config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
				t.Cleanup(func() { config.GetConfig().Mail = dialer })
				require.Error(t, runReport(t, inst, p, expiry))
				state := getReport(t, inst, p, expiry)
				require.NotEmpty(t, state.FileFingerprint)
				rows, file := readReport(t, inst, state)
				file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusError}
				require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				antivirusRev := file.Rev()
				require.NoError(t, couchdb.Compact(inst, consts.Files))
				require.Eventually(t, func() bool {
					return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, file.ID(), state.FileRev, &vfs.FileDoc{}))
				}, 15*time.Second, 100*time.Millisecond, "publication revision body must actually be compacted")
				switch change {
				case "legacy":
					state.FileFingerprint = ""
				case "metadata":
					file.Metadata["owner-edited"] = true
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
				case "unknown field":
					doc := &couchdb.JSONDoc{Type: consts.Files}
					require.NoError(t, couchdb.GetDoc(inst, consts.Files, file.ID(), doc))
					doc.M["new_unknown_field"] = "modified"
					require.NoError(t, couchdb.UpdateDoc(inst, doc))
				case "content", "content with restored metadata":
					original := &couchdb.JSONDoc{Type: consts.Files}
					require.NoError(t, couchdb.GetDoc(inst, consts.Files, file.ID(), original))
					newFile, err := vfs.NewFileDoc(file.DocName, file.DirID, -1, nil, file.Mime, file.Class, time.Now(), false, false, false, nil)
					require.NoError(t, err)
					newFile.Metadata = file.Metadata
					out, err := inst.VFS().CreateFile(newFile, file)
					require.NoError(t, err)
					_, err = io.WriteString(out, "modified content")
					require.NoError(t, err)
					require.NoError(t, out.Close())
					if change == "content with restored metadata" {
						current := &couchdb.JSONDoc{Type: consts.Files}
						require.NoError(t, couchdb.GetDoc(inst, consts.Files, file.ID(), current))
						original.SetRev(current.Rev())
						require.NoError(t, couchdb.UpdateDoc(inst, original))
						restored := &couchdb.JSONDoc{Type: consts.Files}
						require.NoError(t, couchdb.GetDoc(inst, consts.Files, file.ID(), restored))
						require.Equal(t, original.M, restored.M)
						published, err := inst.VFS().FileByID(file.ID())
						require.NoError(t, err)
						in, err := inst.VFS().OpenFile(published)
						require.NoError(t, err)
						bytes, err := io.ReadAll(in)
						require.NoError(t, err)
						in.Close()
						require.Equal(t, "modified content", string(bytes))
					}
				case "observed restored edit", "compacted restored edit":
					file.Metadata["owner-edited"] = true
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
					editRev := file.Rev()
					delete(file.Metadata, "owner-edited")
					require.NoError(t, couchdb.UpdateDoc(inst.VFS(), file))
					if change == "compacted restored edit" {
						require.NoError(t, couchdb.Compact(inst, consts.Files))
						require.Eventually(t, func() bool {
							return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, state.FileID, editRev, &vfs.FileDoc{}))
						}, 15*time.Second, 100*time.Millisecond, "edited revision must actually be compacted")
					}
				}
				if change == "metadata" || change == "unknown field" || change == "content" || change == "content with restored metadata" {
					require.NoError(t, couchdb.Compact(inst, consts.Files))
					require.Eventually(t, func() bool {
						return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, state.FileID, antivirusRev, &vfs.FileDoc{}))
					}, 15*time.Second, 100*time.Millisecond, "prior revision must be compacted before fingerprint check")
				}
				retryReport(t, inst, state)
				config.GetConfig().Mail = dialer
				require.NoError(t, runReport(t, inst, p, expiry))
				state = getReport(t, inst, p, expiry)
				if change == "antivirus" || change == "compacted restored edit" {
					require.Equal(t, "sent", state.Status)
					after, _ := readReport(t, inst, state)
					require.Equal(t, rows, after)
					got := <-messages
					_, body := reportMailText(t, got.raw)
					require.Contains(t, body, "compaction-visitor")
					require.NoError(t, runReport(t, inst, p, expiry))
				} else {
					require.Equal(t, "cancelled", state.Status)
				}
				select {
				case <-messages:
					t.Fatal("unexpected report mail")
				default:
				}
			})
		}
	})

	t.Run("deliberate file changes cancel pending mail", func(t *testing.T) {
		for _, change := range []string{"edited", "edited then restored", "content", "moved", "trashed", "deleted", "parent trashed"} {
			t.Run(change, func(t *testing.T) {
				p := newReportLink(t, inst, expiry)
				config.GetConfig().Mail = &gomail.DialerOptions{Host: "-"}
				t.Cleanup(func() { config.GetConfig().Mail = dialer })
				require.Error(t, runReport(t, inst, p, expiry))
				state := getReport(t, inst, p, expiry)
				_, file := readReport(t, inst, state)
				switch change {
				case "edited":
					file.Metadata["owner-edited"] = true
					require.NoError(t, couchdb.UpdateDoc(inst, file))
				case "edited then restored":
					file.Metadata["owner-edited"] = true
					require.NoError(t, couchdb.UpdateDoc(inst, file))
					delete(file.Metadata, "owner-edited")
					file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean}
					require.NoError(t, couchdb.UpdateDoc(inst, file))
				case "content":
					newFile, err := vfs.NewFileDoc(file.DocName, file.DirID, -1, nil, file.Mime, file.Class, time.Now(), false, false, false, nil)
					require.NoError(t, err)
					newFile.Metadata = file.Metadata
					out, err := inst.VFS().CreateFile(newFile, file)
					require.NoError(t, err)
					_, err = io.WriteString(out, "altered report content")
					require.NoError(t, err)
					require.NoError(t, out.Close())
				case "moved":
					root := consts.RootDirID
					_, err := vfs.ModifyFileMetadata(inst.VFS(), file, &vfs.DocPatch{DirID: &root})
					require.NoError(t, err)
				case "trashed":
					_, err := vfs.TrashFile(inst.VFS(), file)
					require.NoError(t, err)
				case "deleted":
					require.NoError(t, inst.VFS().DestroyFile(file))
				case "parent trashed":
					dir, err := inst.VFS().DirByID(file.DirID)
					require.NoError(t, err)
					_, err = vfs.TrashDir(inst.VFS(), dir)
					require.NoError(t, err)
				}
				retryReport(t, inst, state)
				config.GetConfig().Mail = dialer
				require.NoError(t, runReport(t, inst, p, expiry))
				require.Equal(t, "cancelled", getReport(t, inst, p, expiry).Status)
				if change == "deleted" {
					_, err := inst.VFS().FileByID(state.FileID)
					require.True(t, os.IsNotExist(err), "deleted report must not be recreated")
				}
				select {
				case <-messages:
					t.Fatal("changed report mailed")
				default:
				}
			})
		}
	})

	t.Run("concurrent duplicate jobs send once", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		var wg sync.WaitGroup
		errors := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				msg, _ := job.NewMessage(publiclink.ReportMessage{LinkID: p.ID(), Expiry: expiry})
				ctx, cancel := job.NewTaskContext("concurrent", job.NewJob(inst, &job.JobRequest{WorkerType: "expired-link-report", Message: msg}), inst)
				defer cancel()
				errors <- expiredLinkReport(ctx)
			}()
		}
		wg.Wait()
		close(errors)
		for err := range errors {
			require.NoError(t, err)
		}
		require.Equal(t, "sent", getReport(t, inst, p, expiry).Status)
		<-messages
		select {
		case <-messages:
			t.Fatal("duplicate concurrent mail")
		default:
		}
	})

	t.Run("cancelled context cannot publish", func(t *testing.T) {
		p := newReportLink(t, inst, expiry)
		msg := publiclink.ReportMessage{LinkID: p.ID(), Expiry: expiry}
		raw, _ := job.NewMessage(msg)
		ctx, cancel := job.NewTaskContext("cancelled", job.NewJob(inst, &job.JobRequest{Message: raw}), inst)
		cancel()
		require.ErrorIs(t, revalidateReport(ctx, msg), context.Canceled)
	})
}
