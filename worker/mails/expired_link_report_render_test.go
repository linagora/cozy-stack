package mails

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	labels := map[string]struct{ downloaded, noActivity string }{
		"en": {"Downloaded", "No activity recorded."},
		"fr": {"Téléchargé", "Aucune activité enregistrée."},
		"es": {"Descargado", "No se ha registrado actividad."},
		"de": {"Heruntergeladen", "Keine Aktivität aufgezeichnet."},
		"it": {"Scaricato", "Nessuna attività registrata."},
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
			rows := [][]string{{"2026-10-29 14:10 UTC", "192.0.2.1", "<script>alert(1)</script>", "downloaded"}, {"2026-10-29 11:25 UTC", "192.0.2.2", strings.Repeat("long", 30), "opened"}, {"2026-10-28 18:02 UTC", "192.0.2.3", "Alice", "updated"}}
			data := map[string]interface{}{"Expiry": "2026-10-31 23:59 UTC", "ReportURL": url, "Events": rows, "Locale": locale, "InstanceURL": "https://example.test/"}
			html, err := buildHTML("expired_link_report", mail.DefaultLayout, ctx, "", locale, data)
			require.NoError(t, err)
			require.Contains(t, html, `alt="Twake Workplace"`)
			require.Contains(t, html, `href="https://example.test/"`)
			require.Contains(t, html, `https://blog.cozy.io/`+locale+`/`)
			require.Contains(t, html, "&lt;script&gt;alert(1)&lt;/script&gt;")
			require.NotContains(t, html, "<script>alert(1)</script>")
			require.Equal(t, 3, strings.Count(html, `href="`+url+`"`))
			require.Contains(t, html, "2026-10-29 14:10 UTC")
			require.Contains(t, html, "<th")
			txt, err := buildText("expired_link_report", "", locale, data)
			require.NoError(t, err)
			require.Contains(t, txt, url)
			require.NotContains(t, txt, "downloaded")
			require.Contains(t, html, labels[locale].downloaded)
			require.Contains(t, txt, labels[locale].downloaded)
			if dir := os.Getenv("REPORT_RENDER_DIR"); dir != "" {
				require.NoError(t, os.WriteFile(filepath.Join(dir, locale+"-edge.html"), []byte(html), 0600))
				data["ReportURL"] = "https://drive.example.test/#/folder/private/file/report"
				data["Events"] = [][]string{{"2026-10-29 14:10 UTC", "192.168.1.45", "Anonymous", "downloaded"}, {"2026-10-29 11:25 UTC", "82.165.197.1", "Anonymous", "opened"}, {"2026-10-28 18:02 UTC", "104.28.15.88", "mira@example.test", "updated"}}
				preview, err := buildHTML("expired_link_report", mail.DefaultLayout, ctx, "", locale, data)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(dir, locale+".html"), []byte(preview), 0600))
			}
			data["Events"] = rows[:1]
			one, err := buildHTML("expired_link_report", mail.DefaultLayout, ctx, "", locale, data)
			require.NoError(t, err)
			require.Contains(t, one, "192.0.2.1")
			require.NotContains(t, one, "192.0.2.2")
			data["Events"] = [][]string{}
			empty, err := buildHTML("expired_link_report", mail.DefaultLayout, ctx, "", locale, data)
			require.NoError(t, err)
			require.Contains(t, empty, labels[locale].noActivity)
		})
	}
}
