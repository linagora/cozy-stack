package banner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const decidedAt = 1788944400

// fixture decodes one of the shared wire fixtures, which are what the backend
// publisher is developed against. A field this package stops reading, or a
// field it starts requiring, breaks here rather than in production.
func fixture(t *testing.T, name string) Command {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	require.NoError(t, err)
	var cmd Command
	require.NoError(t, json.Unmarshal(raw, &cmd))
	return cmd
}

// valid is a command every field of which passes, so a case can break exactly
// one thing and name what it broke.
func valid(t *testing.T) Command {
	t.Helper()
	return fixture(t, "materialize")
}

func TestFixturesAreTheContract(t *testing.T) {
	t.Run("a materialize carries its decision and every locale of it", func(t *testing.T) {
		cmd := fixture(t, "materialize")

		assert.Equal(t, "alice.twake.app", cmd.WorkplaceFqdn)
		assert.Empty(t, cmd.Tenant)
		assert.Equal(t, "banner-command-42", cmd.EventID)
		assert.Equal(t, int64(42), cmd.Revision)
		assert.Equal(t, int64(decidedAt), cmd.Timestamp)
		assert.Equal(t, CategoryBilling, cmd.Category)
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", cmd.BannerID)
		assert.Equal(t, SeverityWarning, cmd.Severity)
		assert.Equal(t, SurfaceBanner, cmd.Surface)
		assert.Equal(t, 150, cmd.Priority)
		assert.True(t, cmd.Dismissible)
		assert.Equal(t, "Échec du paiement", cmd.Title["fr"])
		require.NotNil(t, cmd.CTA)
		assert.Equal(t, "Mettre à jour le moyen de paiement", cmd.CTA.Label["fr"])
		require.NotNil(t, cmd.SecondaryCTA)
		require.NotNil(t, cmd.StartsAt)
		require.NotNil(t, cmd.EndsAt)
		assert.NoError(t, cmd.validate())
	})

	t.Run("a clear carries no wording", func(t *testing.T) {
		cmd := fixture(t, "clear")
		cmd.Clear = true

		assert.Empty(t, cmd.BannerID)
		assert.Empty(t, cmd.Text)
		assert.Equal(t, int64(43), cmd.Revision)
		assert.NoError(t, cmd.validate())
	})

	t.Run("an organization is addressed by its tenant ID", func(t *testing.T) {
		cmd := fixture(t, "organization")

		assert.Equal(t, "acme_org:123", cmd.Tenant)
		assert.Empty(t, cmd.WorkplaceFqdn)
		assert.Equal(t, SurfaceModal, cmd.Surface)
		assert.NoError(t, cmd.validate())
	})

	t.Run("the payload cannot smuggle document fields", func(t *testing.T) {
		raw := []byte(`{
			"category": "billing", "workplaceFqdn": "alice.twake.app",
			"revision": 1, "timestamp": 1788944400,
			"bannerId": "billing.restricted", "severity": "error", "surface": "banner",
			"text": {"en": "restricted"},
			"_id": "banner-billing", "_rev": "9-forged", "clear": true,
			"dismissedAt": "2026-01-01T00:00:00Z",
			"cozyMetadata": {"createdByApp": "drive"}
		}`)
		var cmd Command
		require.NoError(t, json.Unmarshal(raw, &cmd))

		assert.False(t, cmd.Clear, "only the routing key decides a clear")
		b := cmd.banner("en")
		require.NotNil(t, b)
		assert.Empty(t, b.DocID)
		assert.Empty(t, b.DocRev)
		assert.Nil(t, b.DismissedAt)
		assert.Nil(t, b.Metadata)
	})
}

// TestValidateRejections has one case per rejection: a missing case is a
// hole, not a gap in coverage.
func TestValidateRejections(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*Command)
		want   string
	}{
		{"no category", func(c *Command) { c.Category = "" }, "not a valid category"},
		{"category with an upper case letter", func(c *Command) { c.Category = "Billing" }, "not a valid category"},
		{"category starting with a digit", func(c *Command) { c.Category = "2fa" }, "not a valid category"},
		{"category too long", func(c *Command) { c.Category = strings.Repeat("a", 33) }, "not a valid category"},
		{"the quota category", func(c *Command) { c.Category = CategoryQuota }, "reserved for the stack's own rules"},
		{"no target", func(c *Command) { c.WorkplaceFqdn = "" }, "exactly one of tenant and workplaceFqdn"},
		{"both targets", func(c *Command) { c.Tenant = "acme.example" }, "exactly one of tenant and workplaceFqdn"},
		{"tenant too long", func(c *Command) { c.WorkplaceFqdn = ""; c.Tenant = strings.Repeat("a", maxTenantLen+1) }, "tenant must be at most"},
		{"blank tenant", func(c *Command) { c.WorkplaceFqdn = ""; c.Tenant = " " }, "no surrounding whitespace"},
		{"tenant with surrounding whitespace", func(c *Command) { c.WorkplaceFqdn = ""; c.Tenant = " acme" }, "no surrounding whitespace"},
		{"a target with a path", func(c *Command) { c.WorkplaceFqdn = "alice.twake.app/../bob" }, "is not a valid target"},
		{"a target with a scheme", func(c *Command) { c.WorkplaceFqdn = "https://alice.twake.app" }, "is not a valid target"},
		{"a target too long", func(c *Command) { c.WorkplaceFqdn = strings.Repeat("a", 256) }, "is not a valid target"},
		{"no revision", func(c *Command) { c.Revision = 0 }, "a positive revision is required"},
		{"a negative revision", func(c *Command) { c.Revision = -1 }, "a positive revision is required"},
		{"no timestamp", func(c *Command) { c.Timestamp = 0 }, "timestamp is required"},
		{"timestamp in milliseconds", func(c *Command) { c.Timestamp *= 1000 }, "timestamp must be epoch seconds"},
		{"event id too long", func(c *Command) { c.EventID = strings.Repeat("e", maxEventIDLen+1) }, "eventId is longer than"},
		{"no bannerId", func(c *Command) { c.BannerID = "" }, "not a valid identifier"},
		{"bannerId with a slash", func(c *Command) { c.BannerID = "billing/grace" }, "not a valid identifier"},
		{"bannerId too long", func(c *Command) { c.BannerID = strings.Repeat("a", 65) }, "not a valid identifier"},
		{"unknown severity", func(c *Command) { c.Severity = "critical" }, "severity"},
		{"no severity", func(c *Command) { c.Severity = "" }, "severity"},
		{"unknown surface", func(c *Command) { c.Surface = "toast" }, "surface"},
		{"no surface", func(c *Command) { c.Surface = "" }, "surface"},
		{"a negative priority", func(c *Command) { c.Priority = -1 }, "priority"},
		{"a priority above the range", func(c *Command) { c.Priority = maxPriority + 1 }, "priority"},
		{"a window that ends before it starts", func(c *Command) { c.StartsAt, c.EndsAt = c.EndsAt, c.StartsAt }, "startsAt is not before endsAt"},
		{"a window with no length", func(c *Command) { c.EndsAt = c.StartsAt }, "startsAt is not before endsAt"},
		{"window outside RFC3339", func(c *Command) { at := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC); c.EndsAt = &at }, "window must be within"},
		{"no text", func(c *Command) { c.Text = nil }, "required in the en fallback locale"},
		{"text without the fallback locale", func(c *Command) { delete(c.Text, "en") }, "required in the en fallback locale"},
		{"a title without the fallback locale", func(c *Command) { delete(c.Title, "en") }, "required in the en fallback locale"},
		{"a cta label without the fallback locale", func(c *Command) { delete(c.CTA.Label, "en") }, "required in the en fallback locale"},
		{"text too long", func(c *Command) { c.Text["en"] = strings.Repeat("a", maxTextLen+1) }, "text is longer than"},
		{"locale key too long", func(c *Command) { c.Text[strings.Repeat("a", maxLocaleLen+1)] = "text" }, "locale key must be"},
		{"empty locale key", func(c *Command) { c.Text[""] = "text" }, "locale key must be"},
		{"more locales than a document could ever read", func(c *Command) {
			for i := 0; i <= maxLocales; i++ {
				c.Text[fmt.Sprintf("l%d", i)] = "filler"
			}
		}, "carries more than"},
		{"text too long in another locale", func(c *Command) { c.Text["fr"] = strings.Repeat("a", maxTextLen+1) }, "text is longer than"},
		{"title too long", func(c *Command) { c.Title["en"] = strings.Repeat("a", maxTitleLen+1) }, "title is longer than"},
		{"cta over http", func(c *Command) { c.CTA.URL = "http://manager.example.org" }, "cta.url is not an absolute https URL"},
		{"cta with a javascript scheme", func(c *Command) { c.CTA.URL = "javascript:alert(1)" }, "cta.url is not an absolute https URL"},
		{"cta with a relative url", func(c *Command) { c.CTA.URL = "/billing" }, "cta.url is not an absolute https URL"},
		{"cta with no url", func(c *Command) { c.CTA.URL = "" }, "cta.url is not an absolute https URL"},
		{"cta url too long", func(c *Command) { c.CTA.URL = "https://manager.example.org/" + strings.Repeat("a", maxURLLen) }, "cta.url is longer than"},
		{"cta label too long", func(c *Command) { c.CTA.Label["en"] = strings.Repeat("a", maxLabelLen+1) }, "cta.label is longer than"},
		{"secondary cta over http", func(c *Command) { c.SecondaryCTA.URL = "http://twake.app/support" }, "secondaryCta.url is not an absolute https URL"},
		{"a secondary action with no primary", func(c *Command) { c.CTA = nil }, "secondaryCta needs a cta"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := valid(t)
			tc.break_(&cmd)
			err := cmd.validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidCommand, "the failure has to be classified as unfixable")
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("an end stated without a start is not judged here", func(t *testing.T) {
		// The start comes from the stored occurrence, which validation cannot
		// see, so the pair is checked once the merge has resolved it.
		for _, name := range []string{"an end before the decision", "an end at the decision"} {
			cmd := valid(t)
			at := time.Unix(cmd.Timestamp, 0).UTC()
			if name == "an end before the decision" {
				at = at.Add(-24 * time.Hour)
			}
			cmd.StartsAt, cmd.EndsAt = nil, &at
			assert.NoError(t, cmd.validate(), name)
		}
	})

	t.Run("a complete command is accepted", func(t *testing.T) {
		assert.NoError(t, valid(t).validate())
	})

	t.Run("a clear is still addressed and ordered", func(t *testing.T) {
		clear := func() Command {
			cmd := fixture(t, "clear")
			cmd.Clear = true
			return cmd
		}
		for _, tc := range []struct {
			name   string
			break_ func(*Command)
			want   string
		}{
			{"no revision", func(c *Command) { c.Revision = 0 }, "a positive revision is required"},
			{"no timestamp", func(c *Command) { c.Timestamp = 0 }, "timestamp is required"},
			{"timestamp in milliseconds", func(c *Command) { c.Timestamp *= 1000 }, "timestamp must be epoch seconds"},
			{"oversized event id", func(c *Command) { c.EventID = strings.Repeat("e", maxEventIDLen+1) }, "eventId is longer than"},
			{"oversized wording", func(c *Command) { c.Text = Localized{"en": strings.Repeat("x", 2<<20)} }, "clear must not carry presentation"},
			{"ordinary wording", func(c *Command) { c.Text = Localized{"en": "ignored?"} }, "clear must not carry presentation"},
			{"no target", func(c *Command) { c.WorkplaceFqdn = "" }, "exactly one of tenant and workplaceFqdn"},
			{"the quota category", func(c *Command) { c.Category = CategoryQuota }, "reserved"},
		} {
			cmd := clear()
			tc.break_(&cmd)
			assert.ErrorContains(t, cmd.validate(), tc.want, tc.name)
		}
	})
}

func TestCommandDocumentShape(t *testing.T) {
	at := time.Unix(decidedAt, 0).UTC()

	t.Run("every field of the contract", func(t *testing.T) {
		b := valid(t).banner("en")
		require.NotNil(t, b)
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", b.BannerID)
		assert.Equal(t, CategoryBilling, b.Category)
		assert.Equal(t, SeverityWarning, b.Severity)
		assert.Equal(t, SurfaceBanner, b.Surface)
		assert.True(t, b.Dismissible)
		assert.Equal(t, 150, b.Priority)
		assert.Equal(t, "Payment failed", b.Title)
		require.NotNil(t, b.SecondaryCTA)
		assert.Equal(t, "https://twake.app/support", b.SecondaryCTA.URL)
		assert.Equal(t, TriggerCommand, b.Source.Trigger)
		assert.Equal(t, at, b.Source.At, "the decision time is provenance on the document")
		require.NotNil(t, b.StartsAt)
		require.NotNil(t, b.EndsAt)
	})

	t.Run("a window the backend left out is left for Materialize to fill", func(t *testing.T) {
		cmd := valid(t)
		cmd.StartsAt, cmd.EndsAt = nil, nil
		b := cmd.banner("en")
		require.NotNil(t, b)
		assert.Nil(t, b.StartsAt, "an unstated window keeps the occurrence's own start")
		assert.Nil(t, b.EndsAt)
		assert.Equal(t, at, b.Source.At, "and Materialize starts a first one at the decision")
	})

	t.Run("a clear produces no document", func(t *testing.T) {
		cmd := valid(t)
		cmd.Clear = true
		assert.Nil(t, cmd.banner("en"))
	})
}

func TestLocaleIsPickedForTheWholeBanner(t *testing.T) {
	t.Run("the instance locale when the backend sent all of it", func(t *testing.T) {
		b := valid(t).banner("fr")
		require.NotNil(t, b)
		assert.Equal(t, "Échec du paiement", b.Title)
		assert.Contains(t, b.Text, "Nous n'avons pas pu")
		assert.Equal(t, "Mettre à jour le moyen de paiement", b.CTA.Label)
		assert.Equal(t, "fr", b.Lang)
	})

	t.Run("the fallback locale when the backend sent none of it", func(t *testing.T) {
		b := valid(t).banner("de")
		require.NotNil(t, b)
		assert.Equal(t, "Payment failed", b.Title)
		assert.Equal(t, "en", b.Lang, "lang names the language the user actually reads")
	})

	t.Run("an instance with no locale reads the fallback", func(t *testing.T) {
		b := valid(t).banner("")
		require.NotNil(t, b)
		assert.Equal(t, "en", b.Lang)
	})

	t.Run("a language the stack has no catalog for is still the backend's to send", func(t *testing.T) {
		require.NotContains(t, consts.SupportedLocales, "ru")
		cmd := valid(t)
		cmd.Text["ru"] = "Мы не смогли списать средства с вашей карты."
		cmd.Title["ru"] = "Платёж не прошёл"
		cmd.CTA.Label["ru"] = "Обновить способ оплаты"
		cmd.SecondaryCTA.Label["ru"] = "Связаться со службой поддержки"

		b := cmd.banner("ru")
		require.NotNil(t, b)
		assert.Equal(t, "ru", b.Lang, "the stack renders none of this, so its catalogs have no say")
		assert.Equal(t, "Платёж не прошёл", b.Title)
	})

	t.Run("a half translated locale is not used at all", func(t *testing.T) {
		for _, missing := range []func(*Command){
			func(c *Command) { delete(c.Text, "fr") },
			func(c *Command) { delete(c.Title, "fr") },
			func(c *Command) { delete(c.CTA.Label, "fr") },
			func(c *Command) { delete(c.SecondaryCTA.Label, "fr") },
		} {
			cmd := valid(t)
			missing(&cmd)
			b := cmd.banner("fr")
			require.NotNil(t, b)
			assert.Equal(t, "en", b.Lang, "a sentence and its button must be in one language")
			assert.Equal(t, "Payment failed", b.Title)
		}
	})
}

// needCouchDB is testutils.NeedCouchdb, inlined to avoid a circular import.
func needCouchDB(t *testing.T) {
	t.Helper()
	if _, err := couchdb.CheckStatus(context.Background()); err != nil {
		t.Fatal("This test need couchdb to run.")
	}
}

const (
	commandContext  = "banner-command-test"
	refusedContext  = "banner-command-test-refused"
	noBannerContext = "banner-command-test-off"
)

// useCommandContexts registers the test contexts used by the command tests.
func useCommandContexts(t *testing.T) {
	t.Helper()
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		commandContext: map[string]interface{}{
			"enable_banners":            true,
			"banner_command_categories": []interface{}{CategoryBilling, CategoryTrial},
		},
		refusedContext: map[string]interface{}{
			"enable_banners": true,
		},
		noBannerContext: map[string]interface{}{},
	}
	t.Cleanup(func() { conf.Contexts = previous })
}

func newInstance(t *testing.T, contextName, locale, orgID string) *instance.Instance {
	t.Helper()
	domain := fmt.Sprintf("banner-cmd-%d.example", time.Now().UnixNano())
	inst, err := lifecycle.Create(&lifecycle.Options{
		Domain:      domain,
		Email:       "alice@example.org",
		Locale:      locale,
		ContextName: contextName,
		OrgID:       orgID,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(domain) })
	return inst
}

// materialize returns the shared fixture aimed at one instance. Every command
// keeps the fixture's timestamp, so only the revision orders them.
func materialize(t *testing.T, inst *instance.Instance, revision int64) Command {
	t.Helper()
	cmd := fixture(t, "materialize")
	cmd.WorkplaceFqdn = inst.Domain
	cmd.Revision = revision
	return cmd
}

func clearCommand(t *testing.T, inst *instance.Instance, revision int64) Command {
	t.Helper()
	cmd := fixture(t, "clear")
	cmd.WorkplaceFqdn = inst.Domain
	cmd.Revision = revision
	cmd.Clear = true
	return cmd
}

func storedBanner(t *testing.T, inst *instance.Instance) *Banner {
	t.Helper()
	stored, err := Stored(inst, CategoryBilling)
	require.NoError(t, err)
	return stored
}

func storedState(t *testing.T, inst *instance.Instance) *commandState {
	t.Helper()
	stored, err := storedCommand(inst, CategoryBilling)
	require.NoError(t, err)
	require.NotNil(t, stored)
	return stored
}

// dismiss records a dismissal the way an application does, by writing the
// public document.
func dismiss(t *testing.T, inst *instance.Instance) {
	t.Helper()
	stored := storedBanner(t, inst)
	require.NotNil(t, stored)
	at := time.Now().UTC().Truncate(time.Second)
	stored.DismissedAt = &at
	require.NoError(t, couchdb.UpdateDoc(inst, stored))
}

func TestApplyCommand(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)
	useCommandContexts(t)

	t.Run("a materialize creates the document a client reads", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")

		require.NoError(t, ApplyCommand(materialize(t, inst, 42)))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, "banner-billing", stored.DocID, "one document per category")
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", stored.BannerID)
		assert.Equal(t, stackAuthor, stored.Metadata.CreatedByApp, "clients gate trust on this")
		assert.Equal(t, DocTypeVersion, stored.Metadata.DocTypeVersion)
		assert.Equal(t, TriggerCommand, stored.Source.Trigger)
	})

	t.Run("an unchanged newer decision advances the ordering without rewriting", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 10)))
		created := storedBanner(t, inst)
		require.NotNil(t, created)

		require.NoError(t, ApplyCommand(materialize(t, inst, 11)))
		again := storedBanner(t, inst)
		require.NotNil(t, again)
		assert.Equal(t, created.DocRev, again.DocRev, "an unchanged banner must not wake the realtime clients")

		// The stale clear is what the old timestamp guard let through: the
		// document it would compare against never moved.
		require.NoError(t, ApplyCommand(clearCommand(t, inst, 10)))
		assert.NotNil(t, storedBanner(t, inst), "a clear older than the last decision changes nothing")
	})

	t.Run("a clear removes the document and outlives a stale materialize", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 20)))
		require.NotNil(t, storedBanner(t, inst))

		require.NoError(t, ApplyCommand(clearCommand(t, inst, 21)))
		assert.Nil(t, storedBanner(t, inst))

		require.NoError(t, ApplyCommand(materialize(t, inst, 20)))
		assert.Nil(t, storedBanner(t, inst), "the cleared category keeps its revision with no document to hold it")
	})

	t.Run("a redelivery of the same revision changes nothing", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 30)))
		first := storedBanner(t, inst)
		require.NotNil(t, first)

		require.NoError(t, ApplyCommand(materialize(t, inst, 30)))
		again := storedBanner(t, inst)
		require.NotNil(t, again)
		assert.Equal(t, first.DocRev, again.DocRev)
	})

	// A revision reused with different wording is a backend bug the stack
	// cannot repair, so it is ignored like any other non-newer revision rather
	// than given a rejection path of its own.
	t.Run("a revision reused for another payload is ignored", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 40)))

		other := materialize(t, inst, 40)
		other.BannerID = "billing.restricted"
		require.NoError(t, ApplyCommand(other))
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", storedBanner(t, inst).BannerID)
	})

	t.Run("the same occurrence keeps a dismissal the user recorded", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 50)))

		dismissed := storedBanner(t, inst)
		require.NotNil(t, dismissed)
		at := time.Now().UTC().Truncate(time.Second)
		dismissed.DismissedAt = &at
		require.NoError(t, couchdb.UpdateDoc(inst, dismissed))

		// A redelivery of the same revision, then a newer command with new
		// wording for the same occurrence.
		require.NoError(t, ApplyCommand(materialize(t, inst, 50)))
		require.NotNil(t, storedBanner(t, inst).DismissedAt, "a retry must not resurrect a closed banner")

		reworded := materialize(t, inst, 51)
		reworded.Text["en"] = "We could not charge your card. This is the last attempt."
		require.NoError(t, ApplyCommand(reworded))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Contains(t, stored.Text, "last attempt")
		require.NotNil(t, stored.DismissedAt, "same occurrence, same dismissal")

		// A new occurrence is a message the user has not seen.
		escalated := materialize(t, inst, 52)
		escalated.BannerID = "billing.grace.cycle-a.attempt-3"
		require.NoError(t, ApplyCommand(escalated))
		assert.Nil(t, storedBanner(t, inst).DismissedAt)
	})

	t.Run("the instance locale decides the wording", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 60)))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, "fr", stored.Lang)
		assert.Equal(t, "Échec du paiement", stored.Title)
	})

	t.Run("a refresh re-localizes a banner but does not restore a deleted one", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		scheduled := materialize(t, inst, 76)
		starts := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
		ends := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
		scheduled.StartsAt, scheduled.EndsAt = &starts, &ends
		require.NoError(t, ApplyCommand(scheduled))

		// A later decision on the same occurrence that states no window: the
		// scheduled start now lives only on the public document.
		reworded := materialize(t, inst, 77)
		reworded.StartsAt, reworded.EndsAt = nil, &ends
		require.NoError(t, ApplyCommand(reworded))
		require.Equal(t, starts, storedBanner(t, inst).StartsAt.UTC())

		// An application deletes it, as its write access lets it.
		require.NoError(t, couchdb.DeleteDoc(inst, storedBanner(t, inst)))
		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "en"}))

		assert.Nil(t, storedBanner(t, inst),
			"recreating it would move a March 2027 banner to the decision time")
	})

	t.Run("a language change re-picks a locale the backend already sent", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 61)))
		require.Equal(t, "fr", storedBanner(t, inst).Lang)

		// The stack keeps every locale the backend sent, so it can pick again
		// without the backend publishing anything.
		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "en"}))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, "en", stored.Lang)
		assert.Equal(t, "Payment failed", stored.Title)
		assert.Equal(t, int64(61), storedState(t, inst).Revision, "re-localizing is not a decision")
	})

	t.Run("a language change falls back for a locale the backend did not send", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 62)))

		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "de"}))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, consts.DefaultLocale, stored.Lang)
	})

	t.Run("a language change keeps a dismissal and a cleared category", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 63)))
		dismiss(t, inst)

		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "en"}))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, "en", stored.Lang)
		require.NotNil(t, stored.DismissedAt, "a new language is not a new occurrence")

		require.NoError(t, ApplyCommand(clearCommand(t, inst, 64)))
		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "fr"}))
		assert.Nil(t, storedBanner(t, inst), "a cleared category stays cleared")
	})

	t.Run("a moved window is applied at both ends", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 70)))

		// The same occurrence, moved wholesale into the next year. Applying
		// only the new end would leave a window the backend never asked for.
		moved := materialize(t, inst, 71)
		starts := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
		ends := time.Date(2027, 3, 10, 0, 0, 0, 0, time.UTC)
		moved.StartsAt, moved.EndsAt = &starts, &ends
		require.NoError(t, ApplyCommand(moved))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		require.NotNil(t, stored.StartsAt)
		require.NotNil(t, stored.EndsAt)
		assert.Equal(t, starts, stored.StartsAt.UTC())
		assert.Equal(t, ends, stored.EndsAt.UTC())
	})

	t.Run("a command that states no window keeps the occurrence's start", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 72)))
		began := storedBanner(t, inst).StartsAt
		require.NotNil(t, began)

		reworded := materialize(t, inst, 73)
		reworded.StartsAt, reworded.EndsAt = nil, nil
		reworded.Text["en"] = "We could not charge your card. This is the last attempt."
		require.NoError(t, ApplyCommand(reworded))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		require.NotNil(t, stored.StartsAt)
		assert.Equal(t, began.UTC(), stored.StartsAt.UTC(),
			"rewording an occurrence must not restart it")
	})

	t.Run("an end moved on its own is accepted, even into the past", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, ApplyCommand(materialize(t, inst, 74)))
		began := storedBanner(t, inst).StartsAt
		require.NotNil(t, began)

		// The backend closes the window without restating the start. Judging
		// this at intake against the decision time would refuse it.
		ended := materialize(t, inst, 75)
		ended.Timestamp = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC).Unix()
		ends := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		ended.StartsAt, ended.EndsAt = nil, &ends
		require.NoError(t, ApplyCommand(ended))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		require.NotNil(t, stored.EndsAt)
		assert.Equal(t, ends, stored.EndsAt.UTC())
		require.NotNil(t, stored.StartsAt)
		assert.Equal(t, began.UTC(), stored.StartsAt.UTC(), "the occurrence keeps its own start")
	})

	t.Run("a category the context does not accept is skipped", func(t *testing.T) {
		inst := newInstance(t, refusedContext, "en", "")

		require.NoError(t, ApplyCommand(materialize(t, inst, 80)))
		assert.Nil(t, storedBanner(t, inst))
		stored, err := storedCommand(inst, CategoryBilling)
		require.NoError(t, err)
		assert.Nil(t, stored, "skipping must not advance the revision")
	})

	t.Run("an instance that displays no banner is a no-op", func(t *testing.T) {
		inst := newInstance(t, noBannerContext, "en", "")

		require.NoError(t, ApplyCommand(materialize(t, inst, 90)))
		assert.Nil(t, storedBanner(t, inst))
	})

	t.Run("an unknown workplace is retried, not rejected", func(t *testing.T) {
		cmd := valid(t)
		cmd.WorkplaceFqdn = fmt.Sprintf("missing-%d.example", time.Now().UnixNano())

		err := ApplyCommand(cmd)
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrInvalidCommand,
			"a workplace still being provisioned is indistinguishable from a deleted one")
	})

	t.Run("an invalid command never reaches an instance", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		cmd := materialize(t, inst, 100)
		cmd.Severity = "critical"

		assert.ErrorIs(t, ApplyCommand(cmd), ErrInvalidCommand)
		assert.Nil(t, storedBanner(t, inst))
	})

	t.Run("a blocking modal with no way out is made closable", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		cmd := materialize(t, inst, 105)
		cmd.Surface = SurfaceModal
		cmd.Dismissible = false
		cmd.CTA, cmd.SecondaryCTA = nil, nil
		require.NoError(t, ApplyCommand(cmd))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.True(t, stored.Dismissible, "a reload is not a way out, it brings the same banner back")
	})

	t.Run("a commanded banner and a quota banner coexist", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")

		quota := EvaluateQuota(QuotaState{Used: 10 * gigabyte, Quota: 10 * gigabyte}, now)
		require.NoError(t, Materialize(inst, CategoryQuota, quota, now))
		require.NoError(t, ApplyCommand(materialize(t, inst, 110)))

		fromRules, err := Stored(inst, CategoryQuota)
		require.NoError(t, err)
		require.NotNil(t, fromRules)
		assert.Equal(t, BannerIDQuotaExceeded, fromRules.BannerID)
		require.NotNil(t, fromRules.StartsAt, "startsAt is not a field a client may find missing")
		assert.Equal(t, now, *fromRules.StartsAt)
		require.NotNil(t, storedBanner(t, inst))

		// And the quota slot stays the stack's own, whatever the queue says.
		fromQueue := materialize(t, inst, 111)
		fromQueue.Category = CategoryQuota
		assert.ErrorIs(t, ApplyCommand(fromQueue), ErrInvalidCommand)
		fromRules, err = Stored(inst, CategoryQuota)
		require.NoError(t, err)
		require.NotNil(t, fromRules)
		assert.Equal(t, BannerIDQuotaExceeded, fromRules.BannerID)
	})
}

func TestApplyCommandToAnOrganization(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)
	useCommandContexts(t)

	orgCommand := func(t *testing.T, orgID string, revision int64) Command {
		t.Helper()
		cmd := fixture(t, "organization")
		cmd.Tenant = orgID
		cmd.Revision = revision
		return cmd
	}

	t.Run("every instance of the organization gets the banner", func(t *testing.T) {
		orgID := fmt.Sprintf("acme-org-%d", time.Now().UnixNano())
		first := newInstance(t, commandContext, "en", orgID)
		second := newInstance(t, commandContext, "fr", orgID)

		other := newInstance(t, commandContext, "en", orgID+"-other")
		other.OrgDomain = orgID
		require.NoError(t, couchdb.UpdateDoc(prefixer.GlobalPrefixer, other))

		require.NoError(t, ApplyCommand(orgCommand(t, orgID, 7)))

		for _, inst := range []*instance.Instance{first, second} {
			stored := storedBanner(t, inst)
			require.NotNil(t, stored, inst.Domain)
			assert.Equal(t, "billing.restricted", stored.BannerID)
		}
		assert.Equal(t, "fr", storedBanner(t, second).Lang, "each member reads its own language")
		assert.Nil(t, storedBanner(t, other), "a matching organization domain must not select another tenant")

		clear := orgCommand(t, orgID, 8)
		clear = Command{Category: clear.Category, Tenant: clear.Tenant, Revision: clear.Revision, Timestamp: clear.Timestamp, Clear: true}
		require.NoError(t, ApplyCommand(clear))
		assert.Nil(t, storedBanner(t, first))
		assert.Nil(t, storedBanner(t, second))
	})

	t.Run("a replay reaches a member provisioned after the command", func(t *testing.T) {
		orgID := fmt.Sprintf("acme-org-%d", time.Now().UnixNano())
		first := newInstance(t, commandContext, "en", orgID)
		require.NoError(t, ApplyCommand(orgCommand(t, orgID, 7)))
		before := storedBanner(t, first)
		require.NotNil(t, before)

		joined := newInstance(t, commandContext, "en", orgID)
		require.NoError(t, ApplyCommand(orgCommand(t, orgID, 7)))

		require.NotNil(t, storedBanner(t, joined), "an equal revision resolves membership again")
		assert.Equal(t, before.DocRev, storedBanner(t, first).DocRev, "and leaves the members it already reached alone")
	})

	t.Run("a disallowed category skips only that instance", func(t *testing.T) {
		orgID := fmt.Sprintf("acme-org-%d", time.Now().UnixNano())
		accepting := newInstance(t, commandContext, "en", orgID)
		refusing := newInstance(t, refusedContext, "en", orgID)

		require.NoError(t, ApplyCommand(orgCommand(t, orgID, 7)))
		before := storedBanner(t, accepting)
		require.NotNil(t, before)
		assert.Nil(t, storedBanner(t, refusing))
		stored, err := storedCommand(refusing, CategoryBilling)
		require.NoError(t, err)
		assert.Nil(t, stored, "skipping must not advance the member's revision")

		conf := config.GetConfig()
		conf.Contexts[refusedContext] = map[string]interface{}{
			"enable_banners":            true,
			"banner_command_categories": []interface{}{CategoryBilling},
		}
		t.Cleanup(func() {
			conf.Contexts[refusedContext] = map[string]interface{}{"enable_banners": true}
		})

		// Replay reaches a previously skipped member after its context allows
		// the category, without changing members that already accepted it.
		require.NoError(t, ApplyCommand(orgCommand(t, orgID, 7)))
		assert.Equal(t, before.DocRev, storedBanner(t, accepting).DocRev)
		assert.NotNil(t, storedBanner(t, refusing))
	})

	t.Run("an organization with no instance is a no-op", func(t *testing.T) {
		assert.NoError(t, ApplyCommand(orgCommand(t, fmt.Sprintf("empty-org-%d", time.Now().UnixNano()), 7)))
	})
}

func TestCommandStateIsNotInTheAppWritableDoctype(t *testing.T) {
	config.UseTestFile(t)
	needCouchDB(t)
	useCommandContexts(t)

	inst := newInstance(t, commandContext, "en", "")
	require.NoError(t, ApplyCommand(materialize(t, inst, 42)))

	stored, err := storedCommand(inst, CategoryBilling)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, consts.BannerCommands, stored.DocType())
	assert.NotEqual(t, consts.Banners, stored.DocType(),
		"an application allowed to record a dismissal must not reach the ordering record")
	assert.Equal(t, int64(42), stored.Revision)
	assert.False(t, stored.Clear)
	assert.Equal(t, "banner-command-42", stored.EventID)
}
