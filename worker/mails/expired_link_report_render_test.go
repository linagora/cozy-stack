package mails

import (
	"bytes"
	"image/png"
	"io"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/i18n"
	_ "github.com/cozy/cozy-stack/web/statik"
	"github.com/leonelquinteros/gotext"
	"github.com/stretchr/testify/require"
	nethtml "golang.org/x/net/html"
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
	labels := map[string]struct{ downloaded, opened, updated, empty, anonymous string }{
		"en": {"Downloaded", "Opened", "Link edited", "No activity recorded.", "Anonymous"},
		"fr": {"Téléchargé", "Ouvert", "Lien modifié", "Aucune activité enregistrée.", "Anonyme"}, //nolint:misspell // Keep the exact French translation.
		"es": {"Descargado", "Abierto", "Enlace editado", "No se ha registrado actividad.", "Anónimo"},
		"de": {"Heruntergeladen", "Geöffnet", "Link bearbeitet", "Keine Aktivität aufgezeichnet.", "Anonym"},
		"it": {"Scaricato", "Aperto", "Link modificato", "Nessuna attività registrata.", "Anonimo"}, //nolint:misspell // Keep the exact Italian translation.
	}
	removedScope := map[string]string{
		"en": "The CSV contains all retained link events up to expiration, including activity before reports were enabled.",
		"fr": "Le CSV contient tous les événements conservés jusqu’à l’expiration, y compris avant l’activation du rapport.",
		"es": "El CSV contiene todos los eventos conservados del enlace hasta su caducidad, incluida la actividad anterior a la activación de los informes.",
		"de": "Die CSV-Datei enthält alle gespeicherten Link-Ereignisse bis zum Ablauf, einschließlich der Aktivitäten vor der Aktivierung der Berichte.",
		"it": "Il CSV contiene tutti gli eventi conservati del link fino alla scadenza, comprese le attività precedenti all'attivazione dei report.", //nolint:misspell // Preserve the removed Italian translation.
	}
	removedAuthentication := map[string]string{
		"en": "Sign in to the hosting Twake instance to open the report.",
		"fr": "Connectez-vous à l’instance Twake hébergeant le lien pour ouvrir le rapport.",
		"es": "Inicie sesión en la instancia de Twake que aloja el enlace para abrir el informe.",
		"de": "Melden Sie sich bei der Twake-Instanz an, auf der der Link gehostet wird, um den Bericht zu öffnen.",
		"it": "Accedi all'istanza Twake che ospita il link per aprire il report.",
	}
	for _, locale := range consts.SupportedLocales {
		t.Run(locale, func(t *testing.T) {
			file, err := assets.Open("/locales/"+locale+".po", config.DefaultInstanceContext)
			require.NoError(t, err)
			po, err := io.ReadAll(file)
			require.NoError(t, err)
			i18n.LoadLocale(locale, "", po)
			catalogue := gotext.NewPo()
			catalogue.Parse(po)
			keys := 0
			for key := range reference.GetDomain().GetTranslations() {
				if strings.HasPrefix(key, "Mail Link Report ") {
					keys++
					require.True(t, catalogue.IsTranslated(key), "missing translation: %s", key)
				}
			}
			require.Greater(t, keys, 0)
			label, ok := labels[locale]
			require.True(t, ok)
			scope, ok := removedScope[locale]
			require.True(t, ok)
			authentication, ok := removedAuthentication[locale]
			require.True(t, ok)
			ctx, cancel := job.NewTaskContext("0", &job.Job{JobID: "render"}, &instance.Instance{Domain: "example.test", Locale: locale})
			defer cancel()
			reportURL := "https://drive.example.test/#/folder/private/file/" + strings.Repeat("long", 35) + `?a=1&b=" onmouseover="alert(1)`
			rows := [][]string{
				{"2026-10-29 14:10 UTC", "192.0.2.1<script>", "<script>alert(1)</script>", "downloaded"},
				{"2026-10-25 01:30 UTC", "192.0.2.2", "", "opened"},
				{"2026-10-25 00:30 UTC", "192.0.2.3", "Alice <alice@example.test>", "updated"},
			}
			templater := MailTemplater{"expired_link_report": subjectEntry{"Mail Link Report Subject", nil}}
			for _, count := range []int{0, 1, 3} {
				data := map[string]interface{}{"Expiry": "2026-10-31 23:59 UTC", "ReportURL": reportURL, "Events": rows[:count]}
				subject, parts, err := templater.Execute(ctx, "expired_link_report", "layout-expired-link-report", locale, "", data)
				require.NoError(t, err)
				require.Equal(t, catalogue.Get("Mail Link Report Subject"), subject)
				require.Len(t, parts, 2, "MJML must not silently fall back to text")
				require.Equal(t, "text/plain", parts[0].Type)
				require.Equal(t, "text/html", parts[1].Type)
				plain, html := parts[0].Body, parts[1].Body
				require.Contains(t, plain, reportURL)
				for _, body := range []string{plain, html} {
					require.NotContains(t, nethtml.UnescapeString(body), scope)
					require.NotContains(t, nethtml.UnescapeString(body), authentication)
					require.NotContains(t, body, "Mail Link Report Scope")
					require.NotContains(t, body, "Mail Link Report Authentication")
					require.Contains(t, body, catalogue.Get("Mail Link Report Automatic"))
					require.Contains(t, body, "2026-10-31 23:59 UTC")
					if count == 0 {
						require.Contains(t, body, label.empty)
					} else {
						require.NotContains(t, body, label.empty)
						require.Contains(t, body, label.downloaded)
					}
					for i, row := range rows {
						if i < count {
							require.Contains(t, body, row[0])
						} else {
							require.NotContains(t, body, row[0])
						}
					}
				}
				tokens := nethtml.NewTokenizer(strings.NewReader(html))
				links := 0
				for token := tokens.Next(); token != nethtml.ErrorToken; token = tokens.Next() {
					if token != nethtml.StartTagToken && token != nethtml.SelfClosingTagToken {
						continue
					}
					element := tokens.Token()
					for _, attr := range element.Attr {
						require.NotEqual(t, "onmouseover", attr.Key)
						if element.Data == "a" && attr.Key == "href" {
							decoded, err := url.PathUnescape(attr.Val)
							require.NoError(t, err)
							require.Equal(t, reportURL, decoded)
							links++
						}
					}
				}
				require.ErrorIs(t, tokens.Err(), io.EOF)
				require.Equal(t, 3, links)
				for _, name := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
					require.Contains(t, html, `src="cid:`+name+`"`)
				}
				if count > 0 {
					require.Contains(t, plain, "192.0.2.1<script>")
					require.Contains(t, html, "192.0.2.1&lt;script&gt;")
					require.Contains(t, html, "&lt;script&gt;alert(1)&lt;/script&gt;")
					require.NotContains(t, html, "<script>alert(1)</script>")
				}
				if count == 1 {
					require.NotContains(t, html, "192.0.2.2")
				}
				if count == 3 {
					for _, body := range []string{plain, html} {
						require.Contains(t, body, label.opened)
						require.Contains(t, body, label.updated)
					}
					require.Contains(t, plain, label.anonymous)
					require.Contains(t, html, label.anonymous)
					require.Contains(t, html, "Alice &lt;alice@example.test&gt;")
				}
			}
		})
	}
}

func TestExpiredLinkReportAssetsMatchSources(t *testing.T) {
	paths := []string{"mails/expired_link_report.mjml", "mails/expired_link_report.text"}
	for _, locale := range consts.SupportedLocales {
		paths = append(paths, "locales/"+locale+".po")
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			file, err := assets.Open("/"+path, config.DefaultInstanceContext)
			require.NoError(t, err)
			data, err := io.ReadAll(file)
			require.NoError(t, err)
			source, err := os.ReadFile("../../assets/" + path)
			require.NoError(t, err)
			require.Equal(t, source, data)
		})
	}
}

func TestExpiredLinkReportImages(t *testing.T) {
	for _, name := range []string{"report-drive.png", "report-link.png", "report-clock.png"} {
		t.Run(name, func(t *testing.T) {
			file, err := assets.Open("/mails/"+name, config.DefaultInstanceContext)
			require.NoError(t, err)
			data, err := io.ReadAll(file)
			require.NoError(t, err)
			image, err := png.Decode(bytes.NewReader(data))
			require.NoError(t, err)
			require.False(t, image.Bounds().Empty())
			source, err := os.ReadFile("../../assets/mails/" + name)
			require.NoError(t, err)
			require.Equal(t, source, data)
		})
	}
}
