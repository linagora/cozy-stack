package mails

import (
	"bytes"
	"encoding/base64"
	stdhtml "html"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/mail"
	_ "github.com/cozy/cozy-stack/web/statik"
	"github.com/leonelquinteros/gotext"
	"github.com/stretchr/testify/require"
)

func renderReport(t *testing.T, ctx *job.TaskContext, locale string, data map[string]interface{}) (string, string) {
	t.Helper()
	subject, parts, err := RenderMail(ctx, "expired_link_report", "layout-expired-link-report", locale, "", data)
	require.NoError(t, err)
	require.NotEmpty(t, subject)
	require.Len(t, parts, 2, "real MJML rendering must not silently fall back to text")
	require.Equal(t, "text/plain", parts[0].Type)
	require.Equal(t, "text/html", parts[1].Type)
	return parts[0].Body, parts[1].Body
}

func TestExpiredLinkReportRendering(t *testing.T) {
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	old := config.GetConfig().Konnectors.Cmd
	config.GetConfig().Konnectors.Cmd = node
	t.Cleanup(func() { config.GetConfig().Konnectors.Cmd = old })
	english, err := assets.Open("/locales/en.po", config.DefaultInstanceContext)
	require.NoError(t, err)
	raw, err := io.ReadAll(english)
	require.NoError(t, err)
	reference := gotext.NewPo()
	reference.Parse(raw)
	labels := map[string]struct{ downloaded, noActivity, anonymous string }{
		"en": {"Downloaded", "No activity recorded.", "Anonymous"},
		"fr": {"Téléchargé", "Aucune activité enregistrée.", "Anonyme"}, //nolint:misspell // Keep the exact French translation.
		"es": {"Descargado", "No se ha registrado actividad.", "Anónimo"},
		"de": {"Heruntergeladen", "Keine Aktivität aufgezeichnet.", "Anonym"},
		"it": {"Scaricato", "Nessuna attività registrata.", "Anonimo"},
	}
	for _, locale := range consts.SupportedLocales {
		f, err := assets.Open("/locales/"+locale+".po", config.DefaultInstanceContext)
		require.NoError(t, err)
		po, err := io.ReadAll(f)
		require.NoError(t, err)
		i18n.LoadLocale(locale, "", po)
		t.Run(locale, func(t *testing.T) {
			catalogue := gotext.NewPo()
			catalogue.Parse(po)
			for key := range reference.GetDomain().GetTranslations() {
				if key == "Tree Reports" || strings.HasPrefix(key, "Mail Link Report ") {
					require.True(t, catalogue.IsTranslated(key), "missing translation: %s", key)
				}
			}
			require.Contains(t, labels, locale)
			ctx, cancel := job.NewTaskContext("0", &job.Job{JobID: "render"}, &instance.Instance{Domain: "example.test", Locale: locale})
			defer cancel()
			url := "https://drive.example.test/#/folder/private/file/" + strings.Repeat("long", 35)
			rows := formatReportPreview([][]string{
				{"2026-10-25T00:30:00Z", "192.0.2.3", "Alice <alice@example.test>", "updated"},
				{"2026-10-25T01:30:00Z", "192.0.2.2", " \t ", "opened"},
				{"2026-10-29T13:10:00Z", "192.0.2.1", "<script>alert(1)</script>", "downloaded"},
			}, reportMailLocation("Europe/Paris"))
			expiry := formatReportMailTime(time.Date(2026, 10, 31, 22, 59, 0, 0, time.UTC), reportMailLocation("Europe/Paris"))
			data := map[string]interface{}{"Expiry": expiry, "ReportURL": url, "Events": rows}
			txt, html := renderReport(t, ctx, locale, data)
			for _, name := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
				require.Contains(t, html, `src="cid:`+name+`"`)
			}
			require.NotContains(t, html, "https://blog.cozy.io/")
			require.NotContains(t, html, "Twake Workplace")
			require.NotContains(t, html, "<script>alert(1)</script>")
			require.Contains(t, html, "&lt;script&gt;alert(1)&lt;/script&gt;")
			require.Contains(t, html, "Alice &lt;alice@example.test&gt;")
			require.Contains(t, txt, "Alice <alice@example.test>")
			require.Equal(t, 3, strings.Count(html, `href="`+url+`"`))
			for _, body := range []string{txt, stdhtml.UnescapeString(html)} {
				require.Contains(t, body, expiry)
				require.Contains(t, body, "2026-10-25 02:30 CEST +02:00")
				require.Contains(t, body, "2026-10-25 02:30 CET +01:00")
				require.Contains(t, body, "2026-10-29 14:10 CET +01:00")
				require.Contains(t, body, labels[locale].downloaded)
				require.Contains(t, body, labels[locale].anonymous)
				if locale != "en" {
					require.NotContains(t, body, "Anonymous")
				}
			}
			for _, identities := range [][]string{
				{"", " \t ", "Alice <alice@example.test>"},
				{"'\t ", "'\r ", "'\n "},
			} {
				identityRows := [][]string{}
				for _, identity := range identities {
					identityRows = append(identityRows, []string{"2026-10-25T00:30:00Z", "192.0.2.1", identity, "opened"})
				}
				data["Events"] = formatReportPreview(identityRows, reportMailLocation("Europe/Paris"))
				identityText, identityHTML := renderReport(t, ctx, locale, data)
				anonymousCount := 3
				if identities[2] == "Alice <alice@example.test>" {
					anonymousCount = 2
					require.Contains(t, identityText, identities[2])
					require.Contains(t, identityHTML, "Alice &lt;alice@example.test&gt;")
				}
				for _, body := range []string{identityText, identityHTML} {
					require.Equal(t, anonymousCount, strings.Count(body, labels[locale].anonymous))
					if locale != "en" {
						require.NotContains(t, body, "Anonymous")
					}
				}
			}
			require.Contains(t, html, "<th")
			require.Contains(t, txt, url)
			require.NotContains(t, txt, "downloaded")
			if dir := os.Getenv("REPORT_RENDER_DIR"); dir != "" {
				require.NoError(t, os.MkdirAll(dir, 0700))
				saveReportPreview(t, dir, locale+"-edge", html)
				data["ReportURL"] = "https://drive.example.test/#/folder/private/file/report"
				data["Events"] = formatReportPreview([][]string{
					{"2026-10-28T17:02:00Z", "192.0.2.3", "mira@example.test", "updated"},
					{"2026-10-29T10:25:00Z", "192.0.2.2", "", "opened"},
					{"2026-10-29T13:10:00Z", "192.0.2.1", "", "downloaded"},
				}, reportMailLocation("Europe/Paris"))
				_, preview := renderReport(t, ctx, locale, data)
				saveReportPreview(t, dir, locale, preview)
			}
			data["Events"] = rows[:1]
			_, one := renderReport(t, ctx, locale, data)
			require.Contains(t, one, "192.0.2.1")
			require.NotContains(t, one, "192.0.2.2")
			data["Events"] = [][]string{}
			emptyText, empty := renderReport(t, ctx, locale, data)
			require.Contains(t, empty, labels[locale].noActivity)
			require.Contains(t, emptyText, labels[locale].noActivity)
		})
	}
}

// Only synthetic previews are exported; CID bytes come from the actual bundle.
func saveReportPreview(t *testing.T, dir, name, html string) {
	t.Helper()
	for _, image := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
		file, err := assets.Open("/mails/"+image, config.DefaultInstanceContext)
		require.NoError(t, err)
		content, err := io.ReadAll(file)
		require.NoError(t, err)
		html = strings.ReplaceAll(html, "cid:"+image, "data:image/png;base64,"+base64.StdEncoding.EncodeToString(content))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".html"), []byte(html), 0600))
}

func TestExpiredLinkReportMIME(t *testing.T) {
	if testing.Short() {
		t.Skip("requires a private loopback SMTP listener")
	}
	node, err := exec.LookPath("node")
	require.NoError(t, err)
	old := config.GetConfig().Konnectors.Cmd
	config.GetConfig().Konnectors.Cmd = node
	t.Cleanup(func() { config.GetConfig().Konnectors.Cmd = old })
	po, err := assets.Open("/locales/fr.po", config.DefaultInstanceContext)
	require.NoError(t, err)
	content, err := io.ReadAll(po)
	require.NoError(t, err)
	i18n.LoadLocale("fr", "", content)
	ctx, cancel := job.NewTaskContext("0", &job.Job{JobID: "mime"}, &instance.Instance{Domain: "example.test", Locale: "fr"})
	defer cancel()
	dialer, messages := reportSMTP(t)
	data := map[string]interface{}{
		"Expiry":    formatReportMailTime(time.Date(2026, 7, 15, 23, 30, 0, 0, time.UTC), reportMailLocation("Europe/Paris")),
		"ReportURL": "https://drive.example.test/#/folder/private/file/report",
		"Events":    formatReportPreview([][]string{{"2026-07-15T23:00:00Z", "192.0.2.1", "", "opened"}}, reportMailLocation("Europe/Paris")),
	}
	require.NoError(t, doSendMail(ctx, &mail.Options{
		From: &mail.Address{Email: "noreply@example.test"}, To: []*mail.Address{{Email: "owner@example.test"}},
		Dialer: dialer, RequireDelivery: true, TemplateName: "expired_link_report", Layout: "layout-expired-link-report", Locale: "fr", TemplateValues: data,
	}, "example.test"))
	var got capturedReportMail
	select {
	case got = <-messages:
	case <-time.After(time.Second):
		t.Fatal("SMTP did not accept the report")
	}
	require.Contains(t, got.recipient, "owner@example.test")
	_, plain := reportMailText(t, got.raw)
	_, html := reportMailPart(t, got.raw, "text/html")
	for _, body := range []string{plain, stdhtml.UnescapeString(html)} {
		require.Contains(t, body, "Anonyme")
		require.Contains(t, body, "2026-07-16 01:00 CEST +02:00")
		require.Contains(t, body, "2026-07-16 01:30 CEST +02:00")
	}
	_, parts := reportMailParts(t, got.raw)
	images := 0
	for _, part := range parts {
		if !strings.HasPrefix(part.header.Get("Content-Type"), "image/png") {
			continue
		}
		images++
		name := strings.Trim(part.header.Get("Content-ID"), "<>")
		require.Contains(t, []string{"report-drive.png", "report-link.png", "report-clock.png"}, name)
		require.Contains(t, part.header.Get("Content-Disposition"), "inline")
		require.Contains(t, html, "cid:"+name)
		file, err := assets.Open("/mails/"+name, config.DefaultInstanceContext)
		require.NoError(t, err)
		expected, err := io.ReadAll(file)
		require.NoError(t, err)
		require.Equal(t, expected, part.body, "MIME contains the bundled PNG, not a placeholder")
		image, err := png.Decode(bytes.NewReader(part.body))
		require.NoError(t, err)
		require.Positive(t, image.Bounds().Dx())
		require.Positive(t, image.Bounds().Dy())
	}
	require.Equal(t, 3, images)
}
