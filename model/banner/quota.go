package banner

import (
	"net/url"
	"time"

	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/i18n"
)

// The share of the quota at which the user is warned before being blocked. It
// is the same threshold as the quota alert served elsewhere in the product.
const quotaWarningRatio = 0.9

// The identifiers minted by the quota rules. They are part of the stored
// contract: a client keys its dismissal on them.
const (
	BannerIDQuotaExceeded   = "quota.exceeded"
	BannerIDQuotaAlmostFull = "quota.almost-full"
)

// The wording, as message ids of the stack locales. The warning and the call
// to action are reused from the disk quota notification, which already says
// them at the same threshold in every locale the stack ships.
const (
	textQuotaExceeded   = "Banners Quota Exceeded Text"
	textQuotaAlmostFull = "Notifications Disk Quota Close Message"
	textQuotaCTALabel   = "Notifications Disk Quota offers text"
)

// TriggerUsageThreshold is recorded on documents produced by a usage change.
const TriggerUsageThreshold = "usage.threshold.crossed"

// QuotaState is what the quota rules need to decide, kept separate from the
// instance so the rules stay testable without one.
type QuotaState struct {
	Used int64
	// Quota is the number of bytes allowed, zero or less when unlimited.
	Quota  int64
	Locale string
	// ContextName can override a translation.
	ContextName string
	// SettingsURL is where the call to action points, empty when unknown.
	SettingsURL string
}

// EvaluateQuota returns the banner that applies to a quota state, or nil when
// none does.
//
// The identifier carries the threshold rather than the date, so escalating
// from nearly full to full is a new occurrence the user sees again even after
// dismissing the first one, while re-evaluating the same threshold keeps the
// dismissal.
func EvaluateQuota(state QuotaState, now time.Time) *Banner {
	if state.Quota <= 0 || state.Used < 0 {
		return nil
	}

	ratio := float64(state.Used) / float64(state.Quota)
	switch {
	case ratio >= 1:
		return buildQuotaBanner(BannerIDQuotaExceeded, SeverityError, 100, textQuotaExceeded, state, now)
	case ratio >= quotaWarningRatio:
		return buildQuotaBanner(BannerIDQuotaAlmostFull, SeverityWarning, 50, textQuotaAlmostFull, state, now)
	default:
		return nil
	}
}

func buildQuotaBanner(id, severity string, priority int, msgid string, state QuotaState, now time.Time) *Banner {
	startsAt := now
	banner := &Banner{
		BannerID:    id,
		Category:    CategoryQuota,
		Severity:    severity,
		Surface:     SurfaceBanner,
		Text:        translate(state.Locale, state.ContextName, msgid),
		Lang:        lang(state.Locale),
		Dismissible: severity != SeverityError,
		Priority:    priority,
		StartsAt:    &startsAt,
		Source:      Source{Trigger: TriggerUsageThreshold, At: now},
	}
	if target := ctaTarget(state.SettingsURL); target != "" {
		banner.CTA = &CTA{Label: translate(state.Locale, state.ContextName, textQuotaCTALabel), URL: target}
	}
	return banner
}

// ctaTarget keeps anything that is not an absolute https URL out of the
// document. A client renders the call to action as a link, so the scheme is
// the stack's responsibility rather than the client's.
func ctaTarget(raw string) string {
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return raw
}

// translate falls back to English inside i18n for an untranslated locale,
// which is why Lang says the language that was asked for rather than the one
// that came out.
func translate(locale, context, msgid string) string {
	return i18n.Translate(msgid, lang(locale), context)
}

func lang(locale string) string {
	if locale == "" {
		return consts.DefaultLocale
	}
	return locale
}
