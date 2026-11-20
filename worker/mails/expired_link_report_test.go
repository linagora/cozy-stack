package mails

import (
	"bytes"
	"encoding/csv"
	"html"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/feature"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/publiclink"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/model/vfs/vfsafero"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
)

func TestExpiredLinkReportDelivery(t *testing.T) {
	if testing.Short() {
		t.Skip("requires CouchDB and a local SMTP listener")
	}
	testutils.NeedCouchdb(t)
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	conf := config.GetConfig()
	oldCmd, oldMail := conf.Konnectors.Cmd, conf.Mail
	conf.Konnectors.Cmd = node
	t.Cleanup(func() { conf.Konnectors.Cmd, conf.Mail = oldCmd, oldMail })
	po, err := assets.Open("/locales/fr.po", config.DefaultInstanceContext)
	require.NoError(t, err)
	translations, err := io.ReadAll(po)
	require.NoError(t, err)
	i18n.LoadLocale("fr", "", translations)

	const owner = "owner@example.test"
	expiry := time.Date(2025, time.February, 21, 14, 0, 0, 0, time.UTC)
	night, end := publiclink.ReportWindow(expiry)
	inst := &instance.Instance{
		Domain: "report-" + strings.ToLower(utils.RandomString(10)) + ".example.test", Locale: "fr",
		FeatureFlags: map[string]interface{}{publiclink.FlagEnabled: true},
	}
	require.NoError(t, couchdb.CreateDoc(prefixer.GlobalPrefixer, inst))
	t.Cleanup(func() {
		require.NoError(t, couchdb.DeleteDoc(prefixer.GlobalPrefixer, inst))
	})
	t.Cleanup(func() {
		for _, dt := range []string{consts.Files, consts.Settings, consts.Permissions, consts.PublicLinkEvents, consts.PublicLinkReports} {
			if err := couchdb.DeleteDB(inst, dt); err != nil && !couchdb.IsNotFoundError(err) {
				t.Error(err)
			}
		}
	})
	require.NoError(t, inst.MakeVFS())
	require.NoError(t, inst.VFS().InitFs())
	for _, view := range couchdb.Views {
		if view.Doctype == consts.Files || view == couchdb.PublicLinkEventsByTime {
			require.NoError(t, couchdb.DefineView(inst, view))
		}
	}
	settings := &couchdb.JSONDoc{Type: consts.Settings, M: map[string]interface{}{
		"_id": consts.InstanceSettingsID, "email": owner, "public_name": "Owner", "tz": "Europe/Paris",
	}}
	require.NoError(t, couchdb.CreateNamedDocWithDB(inst, settings))

	newLink := func(t *testing.T) *permission.Permission {
		t.Helper()
		enabled := true
		link := &permission.Permission{
			Type: permission.TypeShareByLink, ExpiresAt: expiry.Format(time.RFC3339Nano), ActivityReportEnabled: &enabled,
			Permissions: permission.Set{{Type: consts.Files}}, Codes: map[string]string{"link": "fixture-sharecode"},
			Metadata: &metadata.CozyMetadata{UpdatedByApps: []*metadata.UpdatedByAppEntry{{Instance: "creator-other.example.test"}}},
		}
		require.NoError(t, couchdb.CreateDoc(inst, link))
		for index, identity := range []string{"older-visitor", "visitor-two", "visitor-three", "<visitor-four>", "after-expiry"} {
			event := &publiclink.Event{LinkID: link.ID(), Identity: identity, IP: "192.0.2.8", Action: publiclink.Opened,
				OccurredAt: expiry.Add(time.Duration(index-3) * time.Minute)}
			require.NoError(t, couchdb.CreateDoc(inst, event))
		}
		return link
	}
	fileFor := func(t *testing.T, link *permission.Permission) *vfs.FileDoc {
		t.Helper()
		file, err := inst.VFS().FileByID("link-report-" + publiclink.ReportID(link.ID(), expiry))
		require.NoError(t, err)
		return file
	}
	runWithClock := func(t *testing.T, link *permission.Permission, now func() time.Time) error {
		t.Helper()
		msg, err := job.NewMessage(publiclink.ReportMessage{LinkID: link.ID(), Expiry: expiry})
		require.NoError(t, err)
		j := job.NewJob(inst, &job.JobRequest{WorkerType: publiclink.ReportWorker, Message: msg})
		ctx, cancel := job.NewTaskContext("report-test", j, inst)
		defer cancel()
		ctx, cancelTimeout := ctx.WithTimeout(5 * time.Second)
		defer cancelTimeout()
		return expiredLinkReportWithClock(ctx, now)
	}
	runAt := func(t *testing.T, link *permission.Permission, at time.Time) error {
		t.Helper()
		return runWithClock(t, link, func() time.Time { return at })
	}
	run := func(t *testing.T, link *permission.Permission) error {
		t.Helper()
		return runAt(t, link, night)
	}
	setOwner := func(t *testing.T, email string) {
		t.Helper()
		settings.M["email"] = email
		require.NoError(t, couchdb.UpdateDoc(inst, settings))
	}

	t.Run("owner receives a localized preview of the attested CSV", func(t *testing.T) {
		link := newLink(t)
		options, messages := reportSMTP(t, nil, false)
		conf.Mail = options
		conf.Mail.Host = "-"
		require.EqualError(t, run(t, link), "link report attempt failed")
		file := fileFor(t, link)
		publishedRev := file.Rev()
		file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean}
		require.NoError(t, couchdb.UpdateDoc(inst, file))
		require.NoError(t, couchdb.Compact(inst, consts.Files))
		require.Eventually(t, func() bool {
			return couchdb.IsNotFoundError(couchdb.GetDocRev(inst, consts.Files, file.ID(), publishedRev, &couchdb.JSONDoc{}))
		}, 10*time.Second, 50*time.Millisecond)
		conf.Mail.Host = "127.0.0.1"
		require.NoError(t, run(t, link))
		requireNoReportMail(t, messages)
		require.NoError(t, runAt(t, link, night.AddDate(0, 0, 1)))
		got := receiveReportMail(t, messages)
		require.Equal(t, "RCPT TO:<"+owner+">", got.recipient)
		header, parts := reportMailParts(t, got.raw)
		require.Contains(t, header.Get("To"), owner)
		require.NotEmpty(t, header.Get("Message-Id"))
		require.NotContains(t, got.raw, "creator-other.example.test")
		require.NotContains(t, got.raw, "fixture-sharecode")
		u := inst.SubDomain(consts.DriveSlug)
		u.Fragment = "/folder/" + file.DirID + "/file/" + file.ID()
		bodies := map[string]string{}
		images := map[string][]byte{}
		for _, part := range parts {
			kind, _, _ := strings.Cut(part.header.Get("Content-Type"), ";")
			switch kind {
			case "text/plain", "text/html":
				bodies[kind] = string(part.body)
			case "image/png":
				images[strings.Trim(part.header.Get("Content-ID"), "<>")] = part.body
			default:
				t.Fatalf("unexpected MIME part: %s", kind)
			}
		}
		require.Len(t, bodies, 2)
		for _, body := range bodies {
			require.Contains(t, body, u.String())
			require.Contains(t, html.UnescapeString(body), "2025-02-21 15:00 CET +01:00")
			require.Contains(t, body, "Ouvert")
			require.Contains(t, body, "visitor-four")
			require.Greater(t, strings.Index(body, "visitor-three"), strings.Index(body, "visitor-four"))
			require.Greater(t, strings.Index(body, "visitor-two"), strings.Index(body, "visitor-three"))
			require.NotContains(t, body, "older-visitor")
			require.NotContains(t, body, "after-expiry")
		}
		require.Contains(t, bodies["text/html"], "&lt;visitor-four&gt;")
		require.Len(t, images, 3)
		for _, name := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
			asset, err := assets.Open("/mails/"+name, config.DefaultInstanceContext)
			require.NoError(t, err)
			expected, err := io.ReadAll(asset)
			require.NoError(t, err)
			require.Equal(t, expected, images[name])
			_, err = png.Decode(bytes.NewReader(images[name]))
			require.NoError(t, err)
		}
		current := fileFor(t, link)
		require.Equal(t, file.Rev(), current.Rev())
		in, err := inst.VFS().OpenFile(current)
		require.NoError(t, err)
		rows, err := csv.NewReader(in).ReadAll()
		require.NoError(t, in.Close())
		require.NoError(t, err)
		require.Len(t, rows, 5)
		require.Equal(t, "older-visitor", rows[1][2])
		require.Equal(t, "<visitor-four>", rows[4][2])
		require.NoError(t, runAt(t, link, night.AddDate(0, 0, 2)))
		requireNoReportMail(t, messages)
	})

	t.Run("no retroactive email when the owner had no address", func(t *testing.T) {
		link := newLink(t)
		options, messages := reportSMTP(t, nil, false)
		conf.Mail = options
		setOwner(t, "")
		require.NoError(t, run(t, link))
		file := fileFor(t, link)
		setOwner(t, owner)
		require.NoError(t, run(t, link))
		require.Equal(t, file.Rev(), fileFor(t, link).Rev())
		requireNoReportMail(t, messages)
	})

	t.Run("rechecks changes while opening SMTP", func(t *testing.T) {
		for _, change := range []string{
			"clean", "revoked", "expiry", "flag", "manager-disabled", "manager-unavailable", "recipient", "infected", "bytes-only",
			"clean-and-changed-metadata", "clean-and-changed-bytes", "clean-after-reverted-metadata", "clean-and-revoked",
		} {
			t.Run(change, func(t *testing.T) {
				link := newLink(t)
				if change == "recipient" {
					t.Cleanup(func() { setOwner(t, owner) })
				}
				var managerChanged atomic.Bool
				var managerInstance *instance.Instance
				if change == "manager-disabled" || change == "manager-unavailable" {
					managerInstance = useReportFlagManager(t, inst, change, &managerChanged)
				}
				if change == "flag" {
					t.Cleanup(func() {
						var current instance.Instance
						require.NoError(t, couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, inst.ID(), &current))
						current.FeatureFlags[publiclink.FlagEnabled] = true
						require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, &current))
						inst.SetRev(current.Rev())
					})
				}
				mutate := func() error {
					switch change {
					case "revoked":
						link.ActivityReportEnabled = nil
						return couchdb.UpdateDoc(inst, link)
					case "expiry":
						link.ExpiresAt = expiry.Add(time.Nanosecond).Format(time.RFC3339Nano)
						return couchdb.UpdateDoc(inst, link)
					case "flag":
						var current instance.Instance
						if err := couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, inst.ID(), &current); err != nil {
							return err
						}
						current.FeatureFlags[publiclink.FlagEnabled] = false
						return couchdb.UpdateDoc(prefixer.GlobalPrefixer, &current)
					case "manager-disabled", "manager-unavailable":
						managerChanged.Store(true)
						return nil
					case "recipient":
						settings.M["email"] = "changed@example.test"
						return couchdb.UpdateDoc(inst, settings)
					}
					fs := inst.VFS()
					file, err := fs.FileByID("link-report-" + publiclink.ReportID(link.ID(), expiry))
					if err != nil {
						return err
					}
					if change == "infected" {
						file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusInfected}
						return couchdb.UpdateDoc(inst, file)
					}
					if change == "clean-after-reverted-metadata" {
						tags := file.Tags
						file.Tags = []string{"changed"}
						if err := couchdb.UpdateDoc(inst, file); err != nil {
							return err
						}
						file.Tags = tags
					}
					if change != "bytes-only" {
						now := time.Now()
						file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusClean, ScannedAt: &now}
						if change == "clean-and-changed-metadata" {
							file.Tags = []string{"changed"}
						}
						if err := couchdb.UpdateDoc(inst, file); err != nil {
							return err
						}
					}
					switch change {
					case "clean-and-revoked":
						link.ActivityReportEnabled = nil
						return couchdb.UpdateDoc(inst, link)
					case "bytes-only", "clean-and-changed-bytes":
						path, err := fs.FilePath(file)
						if err != nil {
							return err
						}
						// Corrupt the stored bytes without another index revision.
						backend := vfsafero.GetMemFS(inst.DomainName())
						original, err := afero.ReadFile(backend, path)
						if err != nil {
							return err
						}
						original[0] ^= 1
						return afero.WriteFile(backend, path, original, 0600)
					}
					return nil
				}
				changed := make(chan error, 1)
				mailCommands := make(chan int, 2)
				options, messages := reportSMTPWithFaults(t, reportSMTPFaults{
					onHello: func(int) { changed <- mutate() },
					dropMail: func(number int) bool {
						mailCommands <- number
						return false
					},
				})
				conf.Mail = options
				err := run(t, link)
				select {
				case err := <-changed:
					require.NoError(t, err)
				default:
					t.Fatal("SMTP handshake did not reach the mutation")
				}
				if change == "clean" {
					require.NoError(t, err)
					got := receiveReportMail(t, messages)
					require.Equal(t, "RCPT TO:<"+owner+">", got.recipient)
					_, parts := reportMailParts(t, got.raw)
					previewCount := 0
					for _, part := range parts {
						if strings.HasPrefix(part.header.Get("Content-Type"), "text/") {
							require.Contains(t, string(part.body), "visitor-four")
							previewCount++
						}
					}
					require.Equal(t, 2, previewCount)
					require.NoError(t, run(t, link))
					require.Len(t, mailCommands, 1)
				} else {
					require.EqualError(t, err, "link report attempt failed")
					require.Empty(t, mailCommands, "must not submit the envelope")
				}
				requireNoReportMail(t, messages)
				if managerInstance != nil {
					require.True(t, feature.Enabled(managerInstance, publiclink.FlagEnabled), "the ordinary flag cache was not changed")
					var state publiclink.Delivery
					require.NoError(t, couchdb.GetDoc(inst, consts.PublicLinkReports, publiclink.DeliveryID(publiclink.ReportID(link.ID(), expiry)), &state))
					require.Equal(t, publiclink.DeliveryPending, state.Status)
				}
			})
		}
	})

	t.Run("day jobs create neither a checkpoint nor a report", func(t *testing.T) {
		link := newLink(t)
		options, messages := reportSMTP(t, nil, false)
		conf.Mail = options
		for _, daytime := range []time.Time{night.Add(-time.Nanosecond), end} {
			require.NoError(t, runAt(t, link, daytime))
		}
		key := publiclink.ReportID(link.ID(), expiry)
		err := couchdb.GetDoc(inst, consts.PublicLinkReports, publiclink.DeliveryID(key), &publiclink.Delivery{})
		require.True(t, couchdb.IsNotFoundError(err), "%v", err)
		_, err = inst.VFS().FileByID("link-report-" + key)
		require.ErrorIs(t, err, os.ErrNotExist)
		requireNoReportMail(t, messages)
	})

	t.Run("does not interpret an unknown delivery checkpoint as pending", func(t *testing.T) {
		link := newLink(t)
		key := publiclink.ReportID(link.ID(), expiry)
		state := &couchdb.JSONDoc{Type: consts.PublicLinkReports, M: map[string]interface{}{"_id": publiclink.DeliveryID(key)}}
		require.NoError(t, couchdb.CreateNamedDocWithDB(inst, state))
		options, messages := reportSMTP(t, nil, false)
		conf.Mail = options
		require.NoError(t, run(t, link))
		_, err := inst.VFS().FileByID("link-report-" + key)
		require.ErrorIs(t, err, os.ErrNotExist)
		requireNoReportMail(t, messages)
	})

	t.Run("a report admitted at night can finish after the window closes", func(t *testing.T) {
		link := newLink(t)
		var closed atomic.Bool
		options, messages := reportSMTP(t, func() { closed.Store(true) }, false)
		conf.Mail = options
		require.NoError(t, runWithClock(t, link, func() time.Time {
			if closed.Load() {
				return end
			}
			return end.Add(-time.Nanosecond)
		}))
		require.True(t, closed.Load())
		require.Equal(t, "RCPT TO:<"+owner+">", receiveReportMail(t, messages).recipient)
		require.NoError(t, runAt(t, link, night.AddDate(0, 0, 1)))
		requireNoReportMail(t, messages)
	})

	t.Run("requires opt-in before publishing the CSV", func(t *testing.T) {
		link := newLink(t)
		link.ActivityReportEnabled = nil
		require.NoError(t, couchdb.UpdateDoc(inst, link))
		options, messages := reportSMTP(t, nil, false)
		conf.Mail = options
		require.EqualError(t, run(t, link), "link report attempt failed")
		_, err := inst.VFS().FileByID("link-report-" + publiclink.ReportID(link.ID(), expiry))
		require.ErrorIs(t, err, os.ErrNotExist)
		requireNoReportMail(t, messages)
	})

	t.Run("concurrent jobs send one message", func(t *testing.T) {
		link := newLink(t)
		options, messages := reportSMTP(t, nil, false)
		conf.Mail = options
		start := make(chan struct{})
		results := make(chan error, 2)
		for range cap(results) {
			go func() {
				<-start
				results <- run(t, link)
			}()
		}
		close(start)
		for range cap(results) {
			require.NoError(t, <-results)
		}
		require.Equal(t, "RCPT TO:<"+owner+">", receiveReportMail(t, messages).recipient)
		requireNoReportMail(t, messages)
	})

	t.Run("does not reconnect after the first MAIL FROM loses its reply", func(t *testing.T) {
		link := newLink(t)
		greetings := make(chan int, 4)
		mailCommands := make(chan int, 4)
		revoked := make(chan error, 1)
		options, messages := reportSMTPWithFaults(t, reportSMTPFaults{
			onHello: func(number int) {
				greetings <- number
				if number == 2 {
					// Only the old sender reaches this second handshake. Revoke
					// before sending its EHLO reply to expose the stale check.
					changed := link.Clone().(*permission.Permission)
					changed.ActivityReportEnabled = nil
					revoked <- couchdb.UpdateDoc(inst, changed)
				}
			},
			dropMail: func(number int) bool {
				mailCommands <- number
				return number == 1
			},
		})
		conf.Mail = options
		err := run(t, link)
		select {
		case result := <-revoked:
			require.NoError(t, result)
		default:
		}
		requireNoReportMail(t, messages)
		require.EqualError(t, err, "link report attempt failed")
		require.NoError(t, run(t, link))
		requireNoReportMail(t, messages)
		require.Len(t, greetings, 1)
		require.Len(t, mailCommands, 1)
		require.Equal(t, 1, <-mailCommands)
	})

	t.Run("closes the SMTP transport after DATA", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			rejected   bool
			ignoreQuit bool
		}{
			{name: "accepted"},
			{name: "accepted even if QUIT is ignored", ignoreQuit: true},
			{name: "rejected", rejected: true, ignoreQuit: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				link := newLink(t)
				eof := make(chan struct{}, 1)
				quits := make(chan struct{}, 2)
				faults := reportSMTPFaults{
					onEOF:      func() { eof <- struct{}{} },
					onQuit:     func() { quits <- struct{}{} },
					ignoreQuit: tc.ignoreQuit,
				}
				if tc.rejected {
					faults.dataReply = "451 temporary failure"
				}
				options, messages := reportSMTPWithFaults(t, faults)
				conf.Mail = options
				err := run(t, link)
				require.Equal(t, "RCPT TO:<"+owner+">", receiveReportMail(t, messages).recipient)
				select {
				case <-eof:
				case <-time.After(time.Second):
					t.Fatal("relay did not observe client closing the SMTP connection")
				}
				if tc.rejected {
					require.EqualError(t, err, "link report attempt failed")
					require.Empty(t, quits, "do not wait for QUIT after a failed submission")
				} else {
					require.NoError(t, err)
					require.Len(t, quits, 1)
				}
				require.NoError(t, run(t, link))
				requireNoReportMail(t, messages)
			})
		}
	})

	t.Run("does not replay a submission with an unknown outcome", func(t *testing.T) {
		link := newLink(t)
		options, messages := reportSMTP(t, nil, true)
		conf.Mail = options
		require.EqualError(t, run(t, link), "link report attempt failed")
		require.Equal(t, "RCPT TO:<"+owner+">", receiveReportMail(t, messages).recipient)
		require.NoError(t, runAt(t, link, night.AddDate(0, 0, 1)))
		requireNoReportMail(t, messages)
	})
}

func receiveReportMail(t *testing.T, messages <-chan capturedReportMail) capturedReportMail {
	t.Helper()
	select {
	case got := <-messages:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("missing SMTP message")
		return capturedReportMail{}
	}
}

func useReportFlagManager(t *testing.T, inst *instance.Instance, change string, changed *atomic.Bool) *instance.Instance {
	t.Helper()
	manager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/features" || r.URL.Query().Get("sets") != change {
			t.Errorf("unexpected Manager request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if changed.Load() {
			if change == "manager-unavailable" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, `{"flags":{"drive.public_link_events.enabled":false}}`)
			return
		}
		_, _ = io.WriteString(w, `{"flags":{"drive.public_link_events.enabled":true}}`)
	}))
	t.Cleanup(manager.Close)
	conf := config.GetConfig()
	oldClouderies, oldContexts := conf.Clouderies, conf.Contexts
	conf.Clouderies = map[string]config.ClouderyConfig{
		change: {API: config.ClouderyAPI{URL: manager.URL, Token: "fixture"}},
	}
	// Even with a true fallback, an unreachable higher-priority Manager must
	// not authorize a new SMTP envelope.
	conf.Contexts = map[string]interface{}{
		change: map[string]interface{}{"features": map[string]interface{}{publiclink.FlagEnabled: true}},
	}
	t.Cleanup(func() { conf.Clouderies, conf.Contexts = oldClouderies, oldContexts })

	var current instance.Instance
	require.NoError(t, couchdb.GetDoc(prefixer.GlobalPrefixer, consts.Instances, inst.ID(), &current))
	original := current
	current.FeatureFlags = nil
	current.ContextName = change
	current.FeatureSets = []string{change}
	require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, &current))
	t.Cleanup(func() {
		original.SetRev(current.Rev())
		require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, &original))
		inst.SetRev(original.Rev())
	})
	require.True(t, feature.Enabled(&current, publiclink.FlagEnabled), "prime the Manager cache")
	return &current
}

func requireNoReportMail(t *testing.T, messages <-chan capturedReportMail) {
	t.Helper()
	// The worker call and SMTP exchange have already returned.
	select {
	case <-messages:
		t.Fatal("unexpected SMTP message")
	default:
	}
}
