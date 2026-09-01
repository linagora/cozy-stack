package banner

import (
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/metadata"
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

func TestMerge(t *testing.T) {
	dismissedAt := now.Add(-24 * time.Hour)

	t.Run("keeps a dismissal when the occurrence is the same", func(t *testing.T) {
		stored := &Banner{DocID: "abc", DocRev: "1-aaa", BannerID: "quota.almost-full", DismissedAt: &dismissedAt}
		fresh := &Banner{BannerID: "quota.almost-full", Text: "updated wording"}

		merged := Merge(fresh, stored)

		assert.Equal(t, "abc", merged.DocID)
		assert.Equal(t, "1-aaa", merged.DocRev)
		require.NotNil(t, merged.DismissedAt)
		assert.Equal(t, dismissedAt, *merged.DismissedAt)
	})

	t.Run("clears a dismissal when the occurrence changed", func(t *testing.T) {
		stored := &Banner{DocID: "abc", BannerID: "quota.almost-full", DismissedAt: &dismissedAt}
		fresh := &Banner{BannerID: "quota.exceeded"}

		merged := Merge(fresh, stored)

		assert.Nil(t, merged.DismissedAt, "a new occurrence has to be seen again")
	})

	t.Run("leaves a first materialization untouched", func(t *testing.T) {
		fresh := &Banner{BannerID: "quota.exceeded"}
		merged := Merge(fresh, nil)
		assert.Empty(t, merged.DocID)
		assert.Nil(t, merged.DismissedAt)
	})
}

func TestChanged(t *testing.T) {
	base := &Banner{BannerID: "quota.exceeded", Severity: SeverityError, Text: "full"}

	t.Run("an identical evaluation does not rewrite the document", func(t *testing.T) {
		same := *base
		assert.False(t, changed(&same, base))
	})

	t.Run("new wording rewrites it", func(t *testing.T) {
		other := *base
		other.Text = "still full"
		assert.True(t, changed(&other, base))
	})

	t.Run("a call to action appearing or disappearing rewrites it", func(t *testing.T) {
		withCTA := *base
		withCTA.CTA = &CTA{Label: "Upgrade", URL: "https://example.org"}
		assert.True(t, changed(&withCTA, base))
		assert.True(t, changed(base, &withCTA))
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

func TestMergeCarriesTheWindowForward(t *testing.T) {
	began := now.Add(-72 * time.Hour)

	t.Run("the same occurrence keeps the moment it started", func(t *testing.T) {
		stored := &Banner{DocID: "abc", BannerID: BannerIDQuotaAlmostFull, StartsAt: &began}
		fresh := &Banner{BannerID: BannerIDQuotaAlmostFull, StartsAt: &now}

		merged := Merge(fresh, stored)

		require.NotNil(t, merged.StartsAt)
		assert.Equal(t, began, *merged.StartsAt)
	})

	t.Run("a new occurrence starts now", func(t *testing.T) {
		stored := &Banner{DocID: "abc", BannerID: BannerIDQuotaAlmostFull, StartsAt: &began}
		fresh := &Banner{BannerID: BannerIDQuotaExceeded, StartsAt: &now}

		merged := Merge(fresh, stored)

		require.NotNil(t, merged.StartsAt)
		assert.Equal(t, now, *merged.StartsAt)
	})

	t.Run("merging does not write through to the evaluated banner", func(t *testing.T) {
		fresh := &Banner{BannerID: BannerIDQuotaExceeded, CTA: &CTA{Label: "Upgrade"}}
		merged := Merge(fresh, nil)
		stamp(merged, now)

		assert.Nil(t, fresh.Metadata, "stamping the merged copy must not reach the caller's banner")
		assert.NotSame(t, fresh.CTA, merged.CTA)
	})
}

func TestChangedCoversEveryProducedField(t *testing.T) {
	later := now.Add(24 * time.Hour)
	base := &Banner{
		BannerID: BannerIDQuotaExceeded, Category: CategoryQuota, Severity: SeverityError,
		Surface: SurfaceBanner, Text: "full", Lang: "en", Priority: 100, StartsAt: &now,
	}

	t.Run("a moved validity window rewrites it", func(t *testing.T) {
		other := *base
		other.EndsAt = &later
		assert.True(t, changed(&other, base))
		assert.True(t, changed(base, &other))
	})

	t.Run("a different category rewrites it", func(t *testing.T) {
		other := *base
		other.Category = CategorySystem
		assert.True(t, changed(&other, base))
	})

	t.Run("an equal window does not", func(t *testing.T) {
		sameInstant := now.In(time.FixedZone("CEST", 2*60*60))
		other := *base
		other.StartsAt = &sameInstant
		assert.False(t, changed(&other, base), "the same instant in another zone is not a change")
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

func TestStampRepairsAnAdoptedEnvelope(t *testing.T) {
	t.Run("an application authored envelope is corrected, not trusted", func(t *testing.T) {
		b := &Banner{BannerID: BannerIDQuotaExceeded, Metadata: &metadata.CozyMetadata{
			CreatedByApp: "drive", DocTypeVersion: "7",
		}}

		stamp(b, now)

		assert.Equal(t, stackAuthor, b.Metadata.CreatedByApp)
		assert.Equal(t, DocTypeVersion, b.Metadata.DocTypeVersion)
		assert.Equal(t, metadata.MetadataVersion, b.Metadata.MetadataVersion,
			"the contract has metadataVersion on every banner")
		assert.Equal(t, now, b.Metadata.CreatedAt, "a zero createdAt is not publishable")
	})

	t.Run("an envelope the stack has to repair is rewritten even when the wording matches", func(t *testing.T) {
		stored := &Banner{
			DocID: "abc", BannerID: BannerIDQuotaExceeded, Text: "full",
			Metadata: &metadata.CozyMetadata{CreatedByApp: "drive", MetadataVersion: 1, DocTypeVersion: DocTypeVersion},
		}
		merged := Merge(&Banner{BannerID: BannerIDQuotaExceeded, Text: "full"}, stored)
		stamp(merged, now)

		assert.True(t, changed(merged, stored),
			"a document clients drop as untrusted must not be left alone")
	})
}

func TestAModalAlwaysHasAWayOut(t *testing.T) {
	t.Run("a blocking dialog with no action becomes closable", func(t *testing.T) {
		b := &Banner{Surface: SurfaceModal, Dismissible: false}
		ensureEscapable(b)
		assert.True(t, b.Dismissible, "a reload is not a way out, it brings the same banner back")
	})

	t.Run("a blocking dialog with an action is left alone", func(t *testing.T) {
		b := &Banner{Surface: SurfaceModal, Dismissible: false, CTA: &CTA{Label: "Pay", URL: "https://x.example"}}
		ensureEscapable(b)
		assert.False(t, b.Dismissible, "the call to action is the way out")
	})

	t.Run("an inline banner is never forced open", func(t *testing.T) {
		b := &Banner{Surface: SurfaceBanner, Dismissible: false}
		ensureEscapable(b)
		assert.False(t, b.Dismissible, "a banner does not cover the application")
	})
}

func TestEvaluateBilling(t *testing.T) {
	state := func(status string) BillingState {
		return BillingState{Status: status, Locale: "en"}
	}

	t.Run("no banner while the subscription is paying", func(t *testing.T) {
		assert.Nil(t, EvaluateBilling(state("active"), now))
		assert.Nil(t, EvaluateBilling(state("trialing"), now))
	})

	t.Run("no banner while Stripe is still retrying", func(t *testing.T) {
		assert.Nil(t, EvaluateBilling(state("past_due"), now),
			"past_due keeps the plan, and no approved wording exists for that state")
	})

	t.Run("a subscription Stripe gave up on blocks", func(t *testing.T) {
		for _, status := range []string{"unpaid", "canceled"} {
			b := EvaluateBilling(state(status), now)
			require.NotNil(t, b, status)
			assert.Equal(t, BannerIDBillingRestricted, b.BannerID, status)
			assert.Equal(t, SeverityError, b.Severity, status)
			assert.Equal(t, SurfaceModal, b.Surface, status)
			assert.False(t, b.Dismissible, status)
		}
	})

	t.Run("the wording is localized like every other banner", func(t *testing.T) {
		b := EvaluateBilling(BillingState{Status: "unpaid", Locale: "fr"}, now)
		require.NotNil(t, b)
		assert.Equal(t, "fr", b.Lang)
		assert.NotEqual(t, textBillingRestricted, b.Text, "the message id must not reach the document")
	})

	t.Run("a blocking dialog with no call to action is made closable", func(t *testing.T) {
		b := EvaluateBilling(state("unpaid"), now)
		require.NotNil(t, b)
		require.Nil(t, b.CTA, "no manager URL is configured in this state")
		ensureEscapable(b)
		assert.True(t, b.Dismissible, "otherwise the user cannot reach the application at all")
	})
}
