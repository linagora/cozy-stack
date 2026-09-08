package banner

import (
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/pkg/i18n"
)

// BannerIDTrialEnding identifies the one occurrence a trial has. The id names
// the condition rather than the date, so re-materializing an unchanged trial
// keeps a dismissal.
const BannerIDTrialEnding = "trial.ending"

// TriggerTrialChanged is recorded on documents produced by a trial event.
const TriggerTrialChanged = "trial.changed"

// The wording, as a message id of the stack locales. It takes the end date.
const textTrialEnding = "Banners Trial Ending Text"

// trialDateLayout is the end date as the wording reads it. Day before month,
// which is the order en, fr, ru and vi all write.
//
// ponytail: monday v1.0.2 has no vi locale, so Vietnamese wording gets an
// English month name. Nothing to map it to short of a date formatter of our
// own, and the order is right, so it stays as it is.
const trialDateLayout = "2 January 2006"

// TrialState is what the trial rule needs to decide, kept separate from the
// instance so the rule stays testable without one.
type TrialState struct {
	// Status is the subscription status as Stripe reports it, verbatim.
	Status string
	// EndsAt is when the trial ends, known from the day it starts.
	EndsAt time.Time
	Locale string
	// ContextName can override a translation.
	ContextName string
	// ManagerURL is where the call to action points, empty when unknown.
	ManagerURL string
}

// EvaluateTrial returns the banner that applies to a trial, or nil when none
// does. Every status but trialing is a trial that is over, whether it
// converted, ran out or failed its first charge, and nil is what deletes the
// document.
func EvaluateTrial(state TrialState, now time.Time) *Banner {
	if state.Status != "trialing" {
		return nil
	}
	// A window ending at the zero time is dropped by every client while still
	// occupying the slot, and one that has already closed says nothing true.
	// The second case is what a redelivered trialing event looks like after the
	// trial is over, which supersededBy cannot catch once the document it
	// ordered against has been deleted.
	if state.EndsAt.IsZero() || !state.EndsAt.After(now) {
		return nil
	}

	startsAt := now
	endsAt := state.EndsAt
	banner := &Banner{
		BannerID:    BannerIDTrialEnding,
		Category:    CategoryTrial,
		Severity:    SeverityInfo,
		Surface:     SurfaceBanner,
		Text:        translate(state.Locale, state.ContextName, textTrialEnding, i18n.LocalizeTime(state.EndsAt, lang(state.Locale), trialDateLayout)),
		Lang:        lang(state.Locale),
		Dismissible: true,
		Priority:    25,
		StartsAt:    &startsAt,
		// Load bearing: the stack has no scheduled evaluation, so the client
		// evaluating this window against its own clock is what makes the
		// banner go away on the day the trial ends, message or no message.
		EndsAt: &endsAt,
		Source: Source{Trigger: TriggerTrialChanged, At: now},
	}
	if target := ctaTarget(state.ManagerURL); target != "" {
		banner.CTA = &CTA{Label: translate(state.Locale, state.ContextName, textBillingCTALabel), URL: target}
	}
	return banner
}

// RefreshTrial re-evaluates the trial banner of an instance from a trial event.
func RefreshTrial(domain, status string, endsAt, eventAt time.Time) error {
	return refresh(domain, CategoryTrial, eventAt, func(inst *instance.Instance, managerURL string) *Banner {
		return EvaluateTrial(TrialState{
			Status:      status,
			EndsAt:      endsAt,
			Locale:      inst.Locale,
			ContextName: inst.ContextName,
			ManagerURL:  managerURL,
		}, eventAt)
	})
}
