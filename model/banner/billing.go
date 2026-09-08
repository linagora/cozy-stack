package banner

import (
	"strconv"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

// BannerIDBillingRestricted identifies the payment states an organization
// cannot recover from on its own.
const BannerIDBillingRestricted = "billing.restricted"

// BannerIDBillingGrace mints the identifier of one step of the grace period.
// It is part of the stored contract: a client keys its dismissal on it.
func BannerIDBillingGrace(attempt int) string {
	return "billing.grace.attempt-" + strconv.Itoa(attempt)
}

// TriggerPaymentFailed is recorded on documents produced by a payment event.
// There is no recovered counterpart: a recovery deletes the document.
const TriggerPaymentFailed = "payment.failed"

// maxGraceAttempt caps the attempt: Stripe keeps retrying past it with
// nothing new to say.
const maxGraceAttempt = len(graceWording)

// The grace banner sits under the restricted dialog and over the quota ones.
const (
	priorityBillingGrace      = 150
	priorityBillingRestricted = 200
)

// The wording, as message ids of the stack locales.
const (
	textBillingRestrictedTitle = "Banners Billing Restricted Title"
	textBillingRestricted      = "Banners Billing Restricted Text"
	textBillingCTALabel        = "Banners Billing CTA Label"
	textBillingSupportLabel    = "Banners Billing Support Label"
)

// graceWording is the escalation, indexed by attempt. The sentences are
// provisional, for product to replace in place.
var graceWording = [...]struct {
	severity string
	msgid    string
}{
	{SeverityInfo, "Banners Billing Grace Info Text"},
	{SeverityWarning, "Banners Billing Grace Warning Text"},
	{SeverityError, "Banners Billing Grace Error Text"},
}

// BillingState is what the billing rules need to decide. It is the payment
// event plus the instance wording context, kept separate from the instance so
// the rules stay testable without one.
type BillingState struct {
	// Status is the subscription status as Stripe reports it, verbatim.
	Status string
	// AttemptCount is the invoice attempt_count, passed through untouched, so
	// it returns to 1 when a new invoice opens. Zero reads as a first attempt.
	AttemptCount int
	B2B          bool
	Locale       string
	// ContextName can override a translation.
	ContextName string
	// ManagerURL is where the call to action points, empty when unknown.
	ManagerURL string
}

// EvaluateBilling returns the banner that applies to a payment state, or nil
// when none does.
func EvaluateBilling(state BillingState, now time.Time) *Banner {
	switch state.Status {
	case "past_due":
		return graceBanner(state, now)
	case "unpaid", "canceled":
		// Only an organization keeps its plan into these statuses, which is
		// what the restricted wording describes. A single subscriber drops to
		// the free tier instead.
		if !state.B2B {
			return nil
		}
		return restrictedBanner(state, now)
	default:
		return nil
	}
}

// The attempt is part of the identifier because Merge carries a dismissal
// forward only while the identifier is unchanged: an escalation has to read as
// a new occurrence, and a re-evaluation of the same attempt has to not.
func graceBanner(state BillingState, now time.Time) *Banner {
	attempt := min(max(state.AttemptCount, 1), maxGraceAttempt)
	wording := graceWording[attempt-1]

	startsAt := now
	banner := &Banner{
		BannerID:    "billing.grace.attempt-" + strconv.Itoa(attempt),
		Category:    CategoryBilling,
		Severity:    wording.severity,
		Surface:     SurfaceBanner,
		Text:        translate(state.Locale, state.ContextName, wording.msgid),
		Lang:        lang(state.Locale),
		Dismissible: true,
		Priority:    priorityBillingGrace,
		StartsAt:    &startsAt,
		Source:      Source{Trigger: TriggerPaymentFailed, At: now},
	}
	if target := ctaTarget(state.ManagerURL); target != "" {
		banner.CTA = &CTA{Label: translate(state.Locale, state.ContextName, textBillingCTALabel), URL: target}
	}
	return banner
}

func restrictedBanner(state BillingState, now time.Time) *Banner {
	startsAt := now
	banner := &Banner{
		BannerID:    BannerIDBillingRestricted,
		Category:    CategoryBilling,
		Severity:    SeverityError,
		Surface:     SurfaceModal,
		Title:       translate(state.Locale, state.ContextName, textBillingRestrictedTitle),
		Text:        translate(state.Locale, state.ContextName, textBillingRestricted),
		Lang:        lang(state.Locale),
		Dismissible: false,
		Priority:    priorityBillingRestricted,
		StartsAt:    &startsAt,
		Source:      Source{Trigger: TriggerPaymentFailed, At: now},
	}
	if target := ctaTarget(state.ManagerURL); target != "" {
		banner.CTA = &CTA{Label: translate(state.Locale, state.ContextName, textBillingCTALabel), URL: target}
		// cozy-client drops a secondary action that has no primary, so it
		// only makes sense alongside one.
		banner.SecondaryCTA = &CTA{
			Label: translate(state.Locale, state.ContextName, textBillingSupportLabel),
			URL:   "https://twake.app/support",
		}
	}
	return banner
}

// RefreshBilling re-evaluates the billing banner of an instance from a payment
// event. eventAt is the moment Stripe recorded the event, not the moment this
// runs, so it both stamps the document and orders it against what is stored.
func RefreshBilling(domain, status string, attemptCount int, eventAt time.Time) error {
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	if !inst.HasBannersEnabled() {
		return nil
	}

	mu := config.Lock().ReadWrite(inst, "banners")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	stale, err := supersededBy(inst, CategoryBilling, eventAt)
	if err != nil || stale {
		return err
	}

	state := BillingState{
		Status:       status,
		AttemptCount: attemptCount,
		// From the instance rather than from how the event was addressed: an
		// instance-level event can land on an organization member, and that
		// member still keeps the plan its organization pays for.
		B2B:         inst.OrgDomain != "",
		Locale:      inst.Locale,
		ContextName: inst.ContextName,
	}
	if premium, err := inst.ManagerURL(instance.ManagerPremiumURL); err == nil {
		state.ManagerURL = premium
	}

	return Materialize(inst, CategoryBilling, EvaluateBilling(state, eventAt), time.Now())
}

// supersededBy reports whether the stored banner was produced by an event at
// least as recent as this one, in which case this one is a redelivery or
// arrived out of order. Bus delivery is at-least-once and unordered, so
// without this a redelivered failure could overwrite a recovery.
//
// ponytail: Source.At is the event that last changed the document, not the
// last one seen, since an unchanged re-evaluation writes nothing. So a failure
// redelivered after a recovery deleted the document recreates it, and a stale
// recovery between two identical failures clears it. Both need a single queue
// reordered or a replay past the Cloudery's own dedupe; record the last
// applied event time per instance if that ever happens.
func supersededBy(db prefixer.Prefixer, category string, eventAt time.Time) (bool, error) {
	stored, err := Stored(db, category)
	if err != nil || stored == nil {
		return false, err
	}
	return !stored.Source.At.Before(eventAt), nil
}
