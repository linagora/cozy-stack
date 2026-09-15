package banner_test

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/banner"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Load the real catalogs, then use the shared database setup.
func TestMain(m *testing.M) {
	for _, locale := range []string{"en", "fr"} {
		po, err := os.ReadFile("../../assets/locales/" + locale + ".po")
		if err != nil {
			panic(err)
		}
		i18n.LoadLocale(locale, "", po)
	}
	os.Exit(testutils.RunTestMainWithCouchDB(m))
}

func fixture(t *testing.T, name string) banner.Command {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	require.NoError(t, err)
	var cmd banner.Command
	require.NoError(t, json.Unmarshal(raw, &cmd))
	return cmd
}

var now = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

const gigabyte = 1000 * 1000 * 1000

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
			"banner": map[string]interface{}{
				"enabled":            true,
				"command_categories": []interface{}{banner.CategoryBilling, banner.CategoryTrial},
				"cta_hosts":          []interface{}{"manager.example.org", "twake.app"},
			},
		},
		refusedContext: map[string]interface{}{
			"banner": map[string]interface{}{"enabled": true},
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
func materialize(t *testing.T, inst *instance.Instance, revision int64) banner.Command {
	t.Helper()
	cmd := fixture(t, "materialize")
	cmd.WorkplaceFqdn = inst.Domain
	cmd.Revision = revision
	return cmd
}

func clearCommand(t *testing.T, inst *instance.Instance, revision int64) banner.Command {
	t.Helper()
	cmd := fixture(t, "clear")
	cmd.WorkplaceFqdn = inst.Domain
	cmd.Revision = revision
	cmd.Clear = true
	return cmd
}

func storedBanner(t *testing.T, inst *instance.Instance) *banner.Banner {
	t.Helper()
	stored, err := banner.Stored(inst, banner.CategoryBilling)
	require.NoError(t, err)
	if stored != nil && stored.Cleared {
		return nil
	}
	return stored
}

func storedState(t *testing.T, inst *instance.Instance) *banner.Banner {
	t.Helper()
	stored, err := banner.Stored(inst, banner.CategoryBilling)
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
	testutils.NeedCouchdb(t)
	useCommandContexts(t)

	t.Run("a materialize creates the document a client reads", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")

		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 42)))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, "banner-billing", stored.DocID, "one document per category")
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", stored.BannerID)
		assert.Equal(t, "stack", stored.Metadata.CreatedByApp, "clients gate trust on this")
		assert.Equal(t, banner.DocTypeVersion, stored.Metadata.DocTypeVersion)
		assert.Equal(t, banner.TriggerCommand, stored.Source.Trigger)
	})

	t.Run("an unchanged newer decision persists its ordering", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 10)))
		created := storedBanner(t, inst)
		require.NotNil(t, created)

		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 11)))
		again := storedBanner(t, inst)
		require.NotNil(t, again)
		assert.NotEqual(t, created.DocRev, again.DocRev)
		assert.Equal(t, int64(11), again.Revision)

		// The stale clear is what the old timestamp guard let through: the
		// document it would compare against never moved.
		require.NoError(t, banner.ApplyCommand(clearCommand(t, inst, 10)))
		assert.NotNil(t, storedBanner(t, inst), "a clear older than the last decision changes nothing")
	})

	t.Run("a newer translation is retained even when the displayed language is unchanged", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		original := materialize(t, inst, 12)
		require.NoError(t, banner.ApplyCommand(original))
		updated := materialize(t, inst, 13)
		updated.Text["fr"] = "Veuillez vérifier votre carte."
		require.NoError(t, banner.ApplyCommand(updated))
		assert.Equal(t, original.Text["en"], storedBanner(t, inst).Text)
		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "fr"}))
		assert.Equal(t, updated.Text["fr"], storedBanner(t, inst).Text)
	})

	t.Run("a clear expires the document and outlives a stale materialize", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 20)))
		require.NotNil(t, storedBanner(t, inst))

		require.NoError(t, banner.ApplyCommand(clearCommand(t, inst, 21)))
		assert.Nil(t, storedBanner(t, inst))

		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 20)))
		assert.Nil(t, storedBanner(t, inst), "the expired document keeps the cleared revision")
	})

	t.Run("materializing after a clear starts a fresh occurrence", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 22)))
		dismiss(t, inst)
		require.NoError(t, banner.ApplyCommand(clearCommand(t, inst, 23)))
		cleared := storedState(t, inst)
		require.True(t, cleared.Cleared)
		assert.Nil(t, cleared.Accepted)
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 24)))
		require.NotNil(t, storedBanner(t, inst))
		assert.Nil(t, storedBanner(t, inst).DismissedAt)
	})

	t.Run("a dismissal does not survive the occurrence becoming blocking", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 25)))
		dismiss(t, inst)
		blocking := materialize(t, inst, 26)
		blocking.Dismissible = false
		require.NoError(t, banner.ApplyCommand(blocking))
		assert.Nil(t, storedBanner(t, inst).DismissedAt)
	})

	t.Run("a redelivery of the same revision changes nothing", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 30)))
		first := storedBanner(t, inst)
		require.NotNil(t, first)

		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 30)))
		again := storedBanner(t, inst)
		require.NotNil(t, again)
		assert.Equal(t, first.DocRev, again.DocRev)
	})

	// A revision reused with different wording is a backend bug the stack
	// cannot repair, so it is ignored like any other non-newer revision rather
	// than given a rejection path of its own.
	t.Run("a revision reused for another payload is ignored", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 40)))

		other := materialize(t, inst, 40)
		other.BannerID = "billing.restricted"
		require.NoError(t, banner.ApplyCommand(other))
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", storedBanner(t, inst).BannerID)
	})

	t.Run("the same occurrence keeps a dismissal the user recorded", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 50)))

		dismissed := storedBanner(t, inst)
		require.NotNil(t, dismissed)
		at := time.Now().UTC().Truncate(time.Second)
		dismissed.DismissedAt = &at
		require.NoError(t, couchdb.UpdateDoc(inst, dismissed))

		// A redelivery of the same revision, then a newer command with new
		// wording for the same occurrence.
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 50)))
		require.NotNil(t, storedBanner(t, inst).DismissedAt, "a retry must not resurrect a closed banner")

		reworded := materialize(t, inst, 51)
		reworded.Text["en"] = "We could not charge your card. This is the last attempt."
		require.NoError(t, banner.ApplyCommand(reworded))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Contains(t, stored.Text, "last attempt")
		require.NotNil(t, stored.DismissedAt, "same occurrence, same dismissal")

		// A new occurrence is a message the user has not seen.
		escalated := materialize(t, inst, 52)
		escalated.BannerID = "billing.grace.cycle-a.attempt-3"
		require.NoError(t, banner.ApplyCommand(escalated))
		assert.Nil(t, storedBanner(t, inst).DismissedAt)
	})

	t.Run("the instance locale decides the wording", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 60)))

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
		require.NoError(t, banner.ApplyCommand(scheduled))

		// A later decision on the same occurrence that states no window: the
		// scheduled start now lives only on the public document.
		reworded := materialize(t, inst, 77)
		reworded.StartsAt, reworded.EndsAt = nil, &ends
		require.NoError(t, banner.ApplyCommand(reworded))
		require.Equal(t, starts, storedBanner(t, inst).StartsAt.UTC())

		// An application deletes it, as its write access lets it.
		require.NoError(t, couchdb.DeleteDoc(inst, storedBanner(t, inst)))
		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "en"}))

		assert.Nil(t, storedBanner(t, inst),
			"recreating it would move a March 2027 banner to the decision time")
	})

	t.Run("a language change re-picks a locale the backend already sent", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 61)))
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
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 62)))

		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "de"}))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, consts.DefaultLocale, stored.Lang)
	})

	t.Run("a language change keeps a dismissal and a cleared category", func(t *testing.T) {
		inst := newInstance(t, commandContext, "fr", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 63)))
		dismiss(t, inst)

		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "en"}))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.Equal(t, "en", stored.Lang)
		require.NotNil(t, stored.DismissedAt, "a new language is not a new occurrence")

		require.NoError(t, banner.ApplyCommand(clearCommand(t, inst, 64)))
		require.NoError(t, lifecycle.Patch(inst, &lifecycle.Options{Locale: "fr"}))
		assert.Nil(t, storedBanner(t, inst), "a cleared category stays cleared")
	})

	t.Run("a moved window is applied at both ends", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 70)))

		// The same occurrence, moved wholesale into the next year. Applying
		// only the new end would leave a window the backend never asked for.
		moved := materialize(t, inst, 71)
		starts := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
		ends := time.Date(2027, 3, 10, 0, 0, 0, 0, time.UTC)
		moved.StartsAt, moved.EndsAt = &starts, &ends
		require.NoError(t, banner.ApplyCommand(moved))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		require.NotNil(t, stored.StartsAt)
		require.NotNil(t, stored.EndsAt)
		assert.Equal(t, starts, stored.StartsAt.UTC())
		assert.Equal(t, ends, stored.EndsAt.UTC())
	})

	t.Run("a command that states no window keeps the occurrence's start", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 72)))
		began := storedBanner(t, inst).StartsAt
		require.NotNil(t, began)

		reworded := materialize(t, inst, 73)
		reworded.StartsAt, reworded.EndsAt = nil, nil
		reworded.Text["en"] = "We could not charge your card. This is the last attempt."
		require.NoError(t, banner.ApplyCommand(reworded))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		require.NotNil(t, stored.StartsAt)
		assert.Equal(t, began.UTC(), stored.StartsAt.UTC(),
			"rewording an occurrence must not restart it")
	})

	t.Run("an end moved on its own is accepted, even into the past", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 74)))
		began := storedBanner(t, inst).StartsAt
		require.NotNil(t, began)

		// The backend closes the window without restating the start. Judging
		// this at intake against the decision time would refuse it.
		ended := materialize(t, inst, 75)
		ended.Timestamp = time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC).Unix()
		ends := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		ended.StartsAt, ended.EndsAt = nil, &ends
		require.NoError(t, banner.ApplyCommand(ended))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		require.NotNil(t, stored.EndsAt)
		assert.Equal(t, ends, stored.EndsAt.UTC())
		require.NotNil(t, stored.StartsAt)
		assert.Equal(t, began.UTC(), stored.StartsAt.UTC(), "the occurrence keeps its own start")
	})

	t.Run("a category the context does not accept is skipped", func(t *testing.T) {
		inst := newInstance(t, refusedContext, "en", "")

		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 80)))
		assert.Nil(t, storedBanner(t, inst))
		stored, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		assert.Nil(t, stored, "skipping must not advance the revision")
	})

	t.Run("a CTA host the context does not accept is skipped", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		cmd := materialize(t, inst, 85)
		cmd.CTA.URL = "https://evil.example/pay"

		require.NoError(t, banner.ApplyCommand(cmd))
		stored, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		assert.Nil(t, stored, "skipping must not advance the revision")
	})

	t.Run("an instance that displays no banner is a no-op", func(t *testing.T) {
		inst := newInstance(t, noBannerContext, "en", "")

		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 90)))
		assert.Nil(t, storedBanner(t, inst))
	})

	t.Run("an unknown workplace is retried, not rejected", func(t *testing.T) {
		cmd := fixture(t, "materialize")
		cmd.WorkplaceFqdn = fmt.Sprintf("missing-%d.example", time.Now().UnixNano())

		err := banner.ApplyCommand(cmd)
		require.Error(t, err)
		assert.NotErrorIs(t, err, banner.ErrInvalidCommand,
			"a workplace still being provisioned is indistinguishable from a deleted one")
	})

	t.Run("an invalid command never reaches an instance", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		cmd := materialize(t, inst, 100)
		cmd.Severity = "critical"

		assert.ErrorIs(t, banner.ApplyCommand(cmd), banner.ErrInvalidCommand)
		assert.Nil(t, storedBanner(t, inst))
	})

	t.Run("a blocking modal with no way out is made closable", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")
		cmd := materialize(t, inst, 105)
		cmd.Surface = banner.SurfaceModal
		cmd.Dismissible = false
		cmd.CTA, cmd.SecondaryCTA = nil, nil
		require.NoError(t, banner.ApplyCommand(cmd))

		stored := storedBanner(t, inst)
		require.NotNil(t, stored)
		assert.True(t, stored.Dismissible, "a reload is not a way out, it brings the same banner back")
	})

	t.Run("a commanded banner and a quota banner coexist", func(t *testing.T) {
		inst := newInstance(t, commandContext, "en", "")

		quota := banner.EvaluateQuota(banner.QuotaState{Used: 10 * gigabyte, Quota: 10 * gigabyte}, now)
		require.NoError(t, banner.Materialize(inst, banner.CategoryQuota, quota, now))
		require.NoError(t, banner.ApplyCommand(materialize(t, inst, 110)))

		fromRules, err := banner.Stored(inst, banner.CategoryQuota)
		require.NoError(t, err)
		require.NotNil(t, fromRules)
		assert.Equal(t, banner.BannerIDQuotaExceeded, fromRules.BannerID)
		require.NotNil(t, fromRules.StartsAt, "startsAt is not a field a client may find missing")
		assert.Equal(t, now, *fromRules.StartsAt)
		require.NotNil(t, storedBanner(t, inst))

		// And the quota slot stays the stack's own, whatever the queue says.
		fromQueue := materialize(t, inst, 111)
		fromQueue.Category = banner.CategoryQuota
		assert.ErrorIs(t, banner.ApplyCommand(fromQueue), banner.ErrInvalidCommand)
		fromRules, err = banner.Stored(inst, banner.CategoryQuota)
		require.NoError(t, err)
		require.NotNil(t, fromRules)
		assert.Equal(t, banner.BannerIDQuotaExceeded, fromRules.BannerID)
	})
}

func TestApplyCommandToAnOrganization(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)

	orgCommand := func(t *testing.T, orgID string, revision int64) banner.Command {
		t.Helper()
		cmd := fixture(t, "organization")
		cmd.OrgID = orgID
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

		require.NoError(t, banner.ApplyCommand(orgCommand(t, orgID, 7)))

		for _, inst := range []*instance.Instance{first, second} {
			stored := storedBanner(t, inst)
			require.NotNil(t, stored, inst.Domain)
			assert.Equal(t, "billing.restricted", stored.BannerID)
		}
		assert.Equal(t, "fr", storedBanner(t, second).Lang, "each member reads its own language")
		assert.Nil(t, storedBanner(t, other), "a matching organization domain must not select another tenant")

		clear := orgCommand(t, orgID, 8)
		clear = banner.Command{Category: clear.Category, OrgID: clear.OrgID, Revision: clear.Revision, Timestamp: clear.Timestamp, Clear: true}
		require.NoError(t, banner.ApplyCommand(clear))
		assert.Nil(t, storedBanner(t, first))
		assert.Nil(t, storedBanner(t, second))
	})

	t.Run("a replay reaches a member provisioned after the command", func(t *testing.T) {
		orgID := fmt.Sprintf("acme-org-%d", time.Now().UnixNano())
		first := newInstance(t, commandContext, "en", orgID)
		require.NoError(t, banner.ApplyCommand(orgCommand(t, orgID, 7)))
		before := storedBanner(t, first)
		require.NotNil(t, before)

		joined := newInstance(t, commandContext, "en", orgID)
		require.NoError(t, banner.ApplyCommand(orgCommand(t, orgID, 7)))

		require.NotNil(t, storedBanner(t, joined), "an equal revision resolves membership again")
		assert.Equal(t, before.DocRev, storedBanner(t, first).DocRev, "and leaves the members it already reached alone")
	})

	t.Run("a disallowed category skips only that instance", func(t *testing.T) {
		orgID := fmt.Sprintf("acme-org-%d", time.Now().UnixNano())
		accepting := newInstance(t, commandContext, "en", orgID)
		refusing := newInstance(t, refusedContext, "en", orgID)

		require.NoError(t, banner.ApplyCommand(orgCommand(t, orgID, 7)))
		before := storedBanner(t, accepting)
		require.NotNil(t, before)
		assert.Nil(t, storedBanner(t, refusing))
		stored, err := banner.Stored(refusing, banner.CategoryBilling)
		require.NoError(t, err)
		assert.Nil(t, stored, "skipping must not advance the member's revision")

		conf := config.GetConfig()
		conf.Contexts[refusedContext] = map[string]interface{}{
			"banner": map[string]interface{}{
				"enabled":            true,
				"command_categories": []interface{}{banner.CategoryBilling},
				"cta_hosts":          []interface{}{"manager.example.org", "twake.app"},
			},
		}
		t.Cleanup(func() {
			conf.Contexts[refusedContext] = map[string]interface{}{"banner": map[string]interface{}{"enabled": true}}
		})

		// Replay reaches a previously skipped member after its context allows
		// the category, without changing members that already accepted it.
		require.NoError(t, banner.ApplyCommand(orgCommand(t, orgID, 7)))
		assert.Equal(t, before.DocRev, storedBanner(t, accepting).DocRev)
		assert.NotNil(t, storedBanner(t, refusing))
	})

	t.Run("an organization with no instance is a no-op", func(t *testing.T) {
		assert.NoError(t, banner.ApplyCommand(orgCommand(t, fmt.Sprintf("empty-org-%d", time.Now().UnixNano()), 7)))
	})
}

func TestCommandStateSharesTheBannerDocument(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)

	inst := newInstance(t, commandContext, "en", "")
	require.NoError(t, banner.ApplyCommand(materialize(t, inst, 42)))

	stored, err := banner.Stored(inst, banner.CategoryBilling)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.Equal(t, consts.Banners, stored.DocType())
	assert.Equal(t, int64(42), stored.Revision)
	assert.False(t, stored.Cleared)
	assert.Equal(t, "banner-command-42", stored.EventID)
}

func TestClearBeforeMaterialize(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	useCommandContexts(t)
	inst := newInstance(t, commandContext, "en", "")
	require.NoError(t, banner.ApplyCommand(clearCommand(t, inst, 2)))
	cleared := storedState(t, inst)
	require.True(t, cleared.Cleared)
	require.NotNil(t, cleared.EndsAt)
	assert.True(t, cleared.EndsAt.Before(time.Now()))
	require.NoError(t, banner.ApplyCommand(materialize(t, inst, 1)))
	assert.Equal(t, cleared.DocRev, storedState(t, inst).DocRev)
	require.NoError(t, banner.ApplyCommand(materialize(t, inst, 3)))
	require.NotNil(t, storedBanner(t, inst))
	assert.False(t, storedState(t, inst).Cleared)
}
