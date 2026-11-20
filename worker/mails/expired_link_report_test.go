package mails

import (
	"bytes"
	"encoding/csv"
	"html"
	"image/png"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
	inst := &instance.Instance{Domain: "report-" + strings.ToLower(utils.RandomString(10)) + ".example.test", Locale: "fr"}
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
	run := func(t *testing.T, link *permission.Permission) error {
		t.Helper()
		msg, err := job.NewMessage(publiclink.ReportMessage{LinkID: link.ID(), Expiry: expiry})
		require.NoError(t, err)
		j := job.NewJob(inst, &job.JobRequest{WorkerType: expiredReportWorker, Message: msg})
		ctx, cancel := job.NewTaskContext("report-test", j, inst)
		defer cancel()
		ctx, cancelTimeout := ctx.WithTimeout(5 * time.Second)
		defer cancelTimeout()
		return expiredLinkReport(ctx)
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
		require.NoError(t, run(t, link))
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

	t.Run("refuses changes while opening SMTP", func(t *testing.T) {
		for _, change := range []string{"revoked", "recipient", "infected", "bytes-only"} {
			t.Run(change, func(t *testing.T) {
				link := newLink(t)
				if change == "recipient" {
					t.Cleanup(func() { setOwner(t, owner) })
				}
				changed := make(chan error, 1)
				options, messages := reportSMTP(t, func() {
					var err error
					if change == "revoked" {
						link.ActivityReportEnabled = nil
						err = couchdb.UpdateDoc(inst, link)
					} else if change == "recipient" {
						settings.M["email"] = "changed@example.test"
						err = couchdb.UpdateDoc(inst, settings)
					} else {
						fs := inst.VFS()
						var file *vfs.FileDoc
						file, err = fs.FileByID("link-report-" + publiclink.ReportID(link.ID(), expiry))
						if err == nil && change == "infected" {
							file.AntivirusScan = &vfs.AntivirusScan{Status: vfs.AVStatusInfected}
							err = couchdb.UpdateDoc(inst, file)
						} else if err == nil {
							var path string
							path, err = fs.FilePath(file)
							if err == nil {
								// Corrupt actual stored bytes without an index revision change.
								backend := vfsafero.GetMemFS(inst.DomainName())
								var original []byte
								original, err = afero.ReadFile(backend, path)
								if err == nil {
									original[0] ^= 1
									err = afero.WriteFile(backend, path, original, 0600)
								}
							}
						}
					}
					changed <- err
				}, false)
				conf.Mail = options
				require.EqualError(t, run(t, link), "link report attempt failed")
				select {
				case err := <-changed:
					require.NoError(t, err)
				default:
					t.Fatal("SMTP handshake did not reach the mutation")
				}
				requireNoReportMail(t, messages)
			})
		}
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
		require.NoError(t, run(t, link))
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

func requireNoReportMail(t *testing.T, messages <-chan capturedReportMail) {
	t.Helper()
	// The worker call and SMTP exchange have already returned.
	select {
	case <-messages:
		t.Fatal("unexpected SMTP message")
	default:
	}
}
