package banner

import (
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain loads the real catalogs, so a message id that no longer exists
// fails here rather than rendering its own name to a user.
func TestMain(m *testing.M) {
	for _, locale := range []string{"en", "fr"} {
		po, err := os.ReadFile("../../assets/locales/" + locale + ".po")
		if err != nil {
			panic(err)
		}
		i18n.LoadLocale(locale, "", po)
	}
	os.Exit(m.Run())
}

var now = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

const gigabyte = 1000 * 1000 * 1000

func TestEvaluateQuota(t *testing.T) {
	t.Run("no banner below the warning threshold", func(t *testing.T) {
		assert.Nil(t, EvaluateQuota(QuotaState{Used: 5 * gigabyte, Quota: 10 * gigabyte}, now))
	})

	t.Run("a warning once the threshold is crossed", func(t *testing.T) {
		b := EvaluateQuota(QuotaState{Used: 9 * gigabyte, Quota: 10 * gigabyte}, now)
		require.NotNil(t, b)
		assert.Equal(t, "quota.almost-full", b.BannerID)
		assert.Equal(t, SeverityWarning, b.Severity)
		assert.True(t, b.Dismissible)
	})

	t.Run("an error once the quota is reached", func(t *testing.T) {
		b := EvaluateQuota(QuotaState{Used: 10 * gigabyte, Quota: 10 * gigabyte}, now)
		require.NotNil(t, b)
		assert.Equal(t, "quota.exceeded", b.BannerID)
		assert.Equal(t, SeverityError, b.Severity)
		assert.False(t, b.Dismissible, "a blocking state is not dismissible")
	})

	t.Run("no banner when the quota is unlimited", func(t *testing.T) {
		assert.Nil(t, EvaluateQuota(QuotaState{Used: 500 * gigabyte, Quota: 0}, now))
	})

	t.Run("a call to action only when the target is known", func(t *testing.T) {
		without := EvaluateQuota(QuotaState{Used: 10 * gigabyte, Quota: 10 * gigabyte}, now)
		require.NotNil(t, without)
		assert.Nil(t, without.CTA, "no link is better than one pointing nowhere")

		with := EvaluateQuota(QuotaState{
			Used: 10 * gigabyte, Quota: 10 * gigabyte,
			SettingsURL: "https://jdoe-settings.example.org/#/subscription",
		}, now)
		require.NotNil(t, with)
		require.NotNil(t, with.CTA)
		assert.Equal(t, "https://jdoe-settings.example.org/#/subscription", with.CTA.URL)
	})

	t.Run("escalating mints a new identifier so a dismissal does not carry over", func(t *testing.T) {
		warning := EvaluateQuota(QuotaState{Used: 9 * gigabyte, Quota: 10 * gigabyte}, now)
		reached := EvaluateQuota(QuotaState{Used: 10 * gigabyte, Quota: 10 * gigabyte}, now)
		require.NotNil(t, warning)
		require.NotNil(t, reached)
		assert.NotEqual(t, warning.BannerID, reached.BannerID)
	})
}

func TestEvaluateQuotaDocumentShape(t *testing.T) {
	state := QuotaState{
		Used: 9 * gigabyte, Quota: 10 * gigabyte, Locale: "fr",
		SettingsURL: "https://jdoe-settings.example.org/#/subscription",
	}

	t.Run("the validity window starts when the occurrence does", func(t *testing.T) {
		b := EvaluateQuota(state, now)
		require.NotNil(t, b)
		require.NotNil(t, b.StartsAt, "startsAt is not one of the fields a client may find missing")
		assert.Equal(t, now, *b.StartsAt)
	})

	t.Run("the text is localized, and lang says which language it is in", func(t *testing.T) {
		b := EvaluateQuota(state, now)
		require.NotNil(t, b)
		assert.Equal(t, "fr", b.Lang)
		assert.NotEqual(t, textQuotaAlmostFull, b.Text, "the message id must not reach the document")

		english := state
		english.Locale = "en"
		assert.NotEqual(t, b.Text, EvaluateQuota(english, now).Text)
	})

	t.Run("an instance with no locale falls back rather than claiming none", func(t *testing.T) {
		noLocale := state
		noLocale.Locale = ""
		b := EvaluateQuota(noLocale, now)
		require.NotNil(t, b)
		assert.Equal(t, "en", b.Lang)
	})

	t.Run("a call to action on any other scheme is dropped", func(t *testing.T) {
		for _, raw := range []string{
			"javascript:alert(1)",
			"http://jdoe-settings.example.org/",
			"/relative",
			"https://",
			"://broken",
		} {
			state := state
			state.SettingsURL = raw
			b := EvaluateQuota(state, now)
			require.NotNil(t, b)
			assert.Nil(t, b.CTA, "%q must not reach the document", raw)
		}
	})
}

func TestEvaluateQuotaFillsEveryContractField(t *testing.T) {
	b := EvaluateQuota(QuotaState{
		Used: 9 * gigabyte, Quota: 10 * gigabyte, Locale: "fr",
		SettingsURL: "https://jdoe-settings.example.org/#/subscription",
	}, now)
	require.NotNil(t, b)

	// The published contract lists cta, dismissedAt and endsAt as the only
	// fields a client may find missing. Everything else is always present.
	assert.Equal(t, BannerIDQuotaAlmostFull, b.BannerID)
	assert.Equal(t, CategoryQuota, b.Category)
	assert.Equal(t, SeverityWarning, b.Severity)
	assert.Equal(t, SurfaceBanner, b.Surface)
	assert.Equal(t, "Supprimez des fichiers ou changez d'offre pour obtenir plus d'espace de stockage.", b.Text)
	assert.Equal(t, "fr", b.Lang)
	assert.True(t, b.Dismissible)
	assert.Equal(t, 50, b.Priority)
	require.NotNil(t, b.StartsAt)
	assert.Equal(t, now, *b.StartsAt)
	assert.Equal(t, TriggerUsageThreshold, b.Source.Trigger)
	assert.Equal(t, now, b.Source.At)
	assert.Nil(t, b.DismissedAt)
}
