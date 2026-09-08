package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/banner"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

// TestUserCreatedHandlerStoresMatrixID checks that the Matrix ID a user.created
// message carries lands in the instance settings document, which is where
// buildRequest reads it from.
func TestUserCreatedHandlerStoresMatrixID(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	// A forced OIDC context lets a message through without a passphrase hash,
	// which is not what this test is about.
	contextName := "matrix-id-test"
	conf := config.GetConfig()
	conf.Authentication = map[string]interface{}{
		contextName: map[string]interface{}{"disable_password_authentication": true},
	}

	newInstance := func(t *testing.T) string {
		t.Helper()
		domain := fmt.Sprintf("matrix-id-%d.example", time.Now().UnixNano())
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain:      domain,
			Email:       "alice@example.org",
			ContextName: contextName,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(domain) })
		return inst.Domain
	}

	storedMatrixID := func(t *testing.T, domain string) string {
		t.Helper()
		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		settings, err := inst.SettingsDocument()
		require.NoError(t, err)
		id, _ := settings.M["matrix_id"].(string)
		return id
	}

	handle := func(t *testing.T, domain, matrixID string) error {
		t.Helper()
		body, err := json.Marshal(rabbitmq.UserCreatedMessage{
			TwakeID:       "alice",
			WorkplaceFqdn: domain,
			MatrixID:      matrixID,
		})
		require.NoError(t, err)

		return rabbitmq.NewUserCreatedHandler().
			Handle(context.Background(), amqp.Delivery{Body: body})
	}

	t.Run("stores the matrix id it receives", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, "@al.ice:example.org"))
		require.Equal(t, "@al.ice:example.org", storedMatrixID(t, domain))
	})

	t.Run("a redelivery leaves the same value in place", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, "@al.ice:example.org"))
		require.NoError(t, handle(t, domain, "@al.ice:example.org"))
		require.Equal(t, "@al.ice:example.org", storedMatrixID(t, domain))
	})

	t.Run("a malformed matrix id is dropped, not stored", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, "al.ice@example.org"))
		require.Empty(t, storedMatrixID(t, domain))
	})

	t.Run("a message without a matrix id stores nothing", func(t *testing.T) {
		domain := newInstance(t)

		require.NoError(t, handle(t, domain, ""))
		require.Empty(t, storedMatrixID(t, domain))
	})
}

// TestBillingLifecycleHandlerMaterializesBanners drives the handler from the
// wire message to the stored document, where the fan-out, the origin and the
// attempt count come together. The rules are covered by the banner package.
func TestBillingLifecycleHandlerMaterializesBanners(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "billing-banner-test"
	config.GetConfig().Contexts = map[string]interface{}{
		contextName: map[string]interface{}{"enable_banners": true},
	}

	newInstance := func(t *testing.T, orgDomain string) string {
		t.Helper()
		domain := fmt.Sprintf("billing-%d.example", time.Now().UnixNano())
		_, err := lifecycle.Create(&lifecycle.Options{
			Domain:      domain,
			Email:       "alice@example.org",
			ContextName: contextName,
			OrgDomain:   orgDomain,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(domain) })
		return domain
	}

	handle := func(t *testing.T, routingKey string, msg rabbitmq.BillingLifecycleMessage) {
		t.Helper()
		body, err := json.Marshal(msg)
		require.NoError(t, err)
		require.NoError(t, rabbitmq.NewBillingLifecycleHandler().
			Handle(context.Background(), amqp.Delivery{RoutingKey: routingKey, Body: body}))
	}

	stored := func(t *testing.T, domain string) *banner.Banner {
		t.Helper()
		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		b, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		return b
	}

	// The handler orders on the event time, so each message needs a later one.
	clock := time.Now().Unix()
	at := func() int64 { clock++; return clock }

	t.Run("a retry escalates then clears on recovery", func(t *testing.T) {
		domain := newInstance(t, "")

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 1,
			Timestamp: at(),
		})
		b := stored(t, domain)
		require.NotNil(t, b, "past_due must produce a banner")
		require.Equal(t, banner.BannerIDBillingGrace(1), b.BannerID)
		require.Equal(t, banner.SeverityInfo, b.Severity)
		require.Equal(t, banner.SurfaceBanner, b.Surface)
		require.True(t, b.Dismissible)

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 2,
			Timestamp: at(),
		})
		b = stored(t, domain)
		require.NotNil(t, b)
		require.Equal(t, banner.BannerIDBillingGrace(2), b.BannerID, "the attempt count must reach the document")
		require.Equal(t, banner.SeverityWarning, b.Severity)

		handle(t, rabbitmq.RoutingKeyPaymentRecovered, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", Timestamp: at(),
		})
		require.Nil(t, stored(t, domain), "a recovery clears the banner whatever the payload says")
	})

	t.Run("the same attempt again writes nothing", func(t *testing.T) {
		domain := newInstance(t, "")

		msg := rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 2, Timestamp: at(),
		}
		handle(t, rabbitmq.RoutingKeyPaymentFailed, msg)
		first := stored(t, domain)
		require.NotNil(t, first)

		// A redelivery, and the retry after it, both say what the document
		// already says. A new revision here would wake every realtime client.
		handle(t, rabbitmq.RoutingKeyPaymentFailed, msg)
		msg.Timestamp = at()
		handle(t, rabbitmq.RoutingKeyPaymentFailed, msg)

		again := stored(t, domain)
		require.NotNil(t, again)
		require.Equal(t, first.DocRev, again.DocRev, "an unchanged evaluation must not write")
	})

	t.Run("a dismissal survives a retry and not an escalation", func(t *testing.T) {
		domain := newInstance(t, "")

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 1, Timestamp: at(),
		})

		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		b := stored(t, domain)
		require.NotNil(t, b)
		dismissed := time.Now().UTC().Truncate(time.Second)
		b.DismissedAt = &dismissed
		require.NoError(t, couchdb.UpdateDoc(inst, b))

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 1, Timestamp: at(),
		})
		b = stored(t, domain)
		require.NotNil(t, b)
		require.NotNil(t, b.DismissedAt, "the same attempt must not reopen what the user closed")

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 2, Timestamp: at(),
		})
		b = stored(t, domain)
		require.NotNil(t, b)
		require.Equal(t, banner.BannerIDBillingGrace(2), b.BannerID)
		require.Nil(t, b.DismissedAt, "an escalation is a new occurrence the user has not seen")
	})

	t.Run("an event that arrives late loses to the one already applied", func(t *testing.T) {
		domain := newInstance(t, "")

		late := at()
		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 2, Timestamp: at(),
		})
		require.Equal(t, banner.BannerIDBillingGrace(2), stored(t, domain).BannerID)

		// Delivery is unordered, so the first attempt can arrive after the
		// second. Applying it would walk the user back down the escalation.
		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 1, Timestamp: late,
		})
		require.Equal(t, banner.BannerIDBillingGrace(2), stored(t, domain).BannerID,
			"the older event must not overwrite the newer one")

		// Same for a recovery that Stripe recorded before the failure.
		handle(t, rabbitmq.RoutingKeyPaymentRecovered, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: domain, Status: "active", Timestamp: late,
		})
		require.NotNil(t, stored(t, domain), "a stale recovery must not clear a later failure")
	})

	t.Run("giving up restricts an organization and releases a single subscriber", func(t *testing.T) {
		orgDomain := fmt.Sprintf("org-%d.example", time.Now().UnixNano())
		member := newInstance(t, orgDomain)
		solo := newInstance(t, "")

		for _, domain := range []string{member, solo} {
			handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
				WorkplaceFqdn: domain, Status: "past_due", AttemptCount: 3,
				Timestamp: at(),
			})
			require.NotNil(t, stored(t, domain), domain)
		}

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			Domain: orgDomain, Status: "unpaid", AttemptCount: 4,
			Timestamp: at(),
		})
		b := stored(t, member)
		require.NotNil(t, b, "the organization keeps its plan until it is cut")
		require.Equal(t, banner.BannerIDBillingRestricted, b.BannerID)
		require.Equal(t, banner.SurfaceModal, b.Surface)

		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: solo, Status: "unpaid", AttemptCount: 4,
			Timestamp: at(),
		})
		require.Nil(t, stored(t, solo), "a single subscriber drops to the free tier instead")
	})

	t.Run("an organization member is restricted however the event is addressed", func(t *testing.T) {
		orgDomain := fmt.Sprintf("org-%d.example", time.Now().UnixNano())
		member := newInstance(t, orgDomain)

		// Addressed to the instance, not fanned out from the organization.
		// What decides is the instance, which still keeps the plan its
		// organization pays for, not the shape the Cloudery happened to use.
		handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: member, Status: "unpaid", AttemptCount: 4, Timestamp: at(),
		})
		b := stored(t, member)
		require.NotNil(t, b, "an organization member must not be silently told nothing")
		require.Equal(t, banner.BannerIDBillingRestricted, b.BannerID)
	})
}
