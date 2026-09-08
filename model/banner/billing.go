package banner

import (
	"time"

	"github.com/cozy/cozy-stack/model/instance"
)

// BannerIDBillingRestricted identifies the one payment state that has an
// approved design.
const BannerIDBillingRestricted = "billing.restricted"

// TriggerPaymentFailed is recorded on documents produced by a payment event.
// There is no recovered counterpart: a recovery deletes the document.
const TriggerPaymentFailed = "payment.failed"

// The wording, as message ids of the stack locales.
const (
	textBillingRestrictedTitle = "Banners Billing Restricted Title"
	textBillingRestricted      = "Banners Billing Restricted Text"
	textBillingCTALabel        = "Banners Billing CTA Label"
	textBillingSupportLabel    = "Banners Billing Support Label"
)

// BillingState is what the billing rules need to decide. It is the payment
// event plus the instance wording context, kept separate from the instance so
// the rules stay testable without one.
type BillingState struct {
	// Status is the subscription status as Stripe reports it, verbatim.
	Status string
	Locale string
	// ContextName can override a translation.
	ContextName string
	// ManagerURL is where the call to action points, empty when unknown.
	ManagerURL string
}

// EvaluateBilling returns the banner that applies to a payment state, or nil
// when none does.
func EvaluateBilling(state BillingState, now time.Time) *Banner {
	// Only a subscription Stripe has given up on produces a banner. While it is
	// retrying the status is past_due, the Cloudery keeps the plan, and every
	// approved wording describes an already restricted workspace, so there is
	// nothing to say to a user whose access is intact.
	if state.Status != "unpaid" && state.Status != "canceled" {
		return nil
	}

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
		Priority:    200,
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
// event.
func RefreshBilling(domain, status string, eventAt time.Time) error {
	return refresh(domain, CategoryBilling, eventAt, func(inst *instance.Instance, managerURL string) *Banner {
		return EvaluateBilling(BillingState{
			Status:      status,
			Locale:      inst.Locale,
			ContextName: inst.ContextName,
			ManagerURL:  managerURL,
		}, eventAt)
	})
}
