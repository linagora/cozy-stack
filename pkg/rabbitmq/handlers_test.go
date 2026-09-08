package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/banner"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
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

// TestBillingLifecycleHandlerRoutesTrialEvents checks that a trial.changed
// message reaches the trial rule rather than the payment one, and that the two
// categories keep their own document.
func TestBillingLifecycleHandlerRoutesTrialEvents(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "trial-banner-test"
	conf := config.GetConfig()
	conf.Contexts = map[string]interface{}{
		contextName: map[string]interface{}{"enable_banners": true},
	}

	eventAt := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	endsAt := eventAt.Add(14 * 24 * time.Hour)

	newInstance := func(t *testing.T) *instance.Instance {
		t.Helper()
		domain := fmt.Sprintf("trial-banner-%d.example", time.Now().UnixNano())
		inst, err := lifecycle.Create(&lifecycle.Options{
			Domain: domain, Email: "alice@example.org", ContextName: contextName,
		})
		require.NoError(t, err)
		t.Cleanup(func() { _ = lifecycle.Destroy(domain) })
		return inst
	}

	handle := func(t *testing.T, routingKey string, msg rabbitmq.BillingLifecycleMessage) error {
		t.Helper()
		if msg.Timestamp == 0 {
			msg.Timestamp = eventAt.Unix()
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)
		return rabbitmq.NewBillingLifecycleHandler().
			Handle(context.Background(), amqp.Delivery{RoutingKey: routingKey, Body: body})
	}

	t.Run("a running trial materializes the trial banner", func(t *testing.T) {
		inst := newInstance(t)

		require.NoError(t, handle(t, rabbitmq.RoutingKeyTrialChanged, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: inst.Domain, Status: "trialing", TrialEndsAt: endsAt,
		}))

		stored, err := banner.Stored(inst, banner.CategoryTrial)
		require.NoError(t, err)
		require.NotNil(t, stored, "the trial rule never ran, or trialing was rewritten to active")
		assert.Equal(t, banner.BannerIDTrialEnding, stored.BannerID)
		require.NotNil(t, stored.EndsAt)
		assert.True(t, endsAt.Equal(*stored.EndsAt), "the deadline the client expires the banner on")

		billing, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		assert.Nil(t, billing, "a trial event must not write a payment banner")
	})

	t.Run("a converted trial removes the banner", func(t *testing.T) {
		inst := newInstance(t)

		require.NoError(t, handle(t, rabbitmq.RoutingKeyTrialChanged, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: inst.Domain, Status: "trialing", TrialEndsAt: endsAt,
		}))
		require.NoError(t, handle(t, rabbitmq.RoutingKeyTrialChanged, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: inst.Domain, Status: "active", TrialEndsAt: endsAt, Timestamp: eventAt.Add(time.Hour).Unix(),
		}))

		stored, err := banner.Stored(inst, banner.CategoryTrial)
		require.NoError(t, err)
		assert.Nil(t, stored)
	})

	t.Run("the two categories coexist", func(t *testing.T) {
		inst := newInstance(t)

		require.NoError(t, handle(t, rabbitmq.RoutingKeyTrialChanged, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: inst.Domain, Status: "trialing", TrialEndsAt: endsAt,
		}))
		require.NoError(t, handle(t, rabbitmq.RoutingKeyPaymentFailed, rabbitmq.BillingLifecycleMessage{
			WorkplaceFqdn: inst.Domain, Status: "unpaid",
		}))

		trial, err := banner.Stored(inst, banner.CategoryTrial)
		require.NoError(t, err)
		assert.NotNil(t, trial, "the payment banner evicted the trial one")

		billing, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		assert.NotNil(t, billing, "the trial banner evicted the payment one")
	})
}

// TestBillingLifecycleMessageParsesTheTrialDeadline pins the wire shape: the
// Cloudery renders the date with a numeric offset rather than Z. It has to
// land as a usable time, or the whole message dead-letters and no banner ever
// appears.
func TestBillingLifecycleMessageParsesTheTrialDeadline(t *testing.T) {
	var msg rabbitmq.BillingLifecycleMessage
	require.NoError(t, json.Unmarshal([]byte(
		`{"workplaceFqdn":"a.example","status":"trialing","timestamp":1784721600,"trialEndsAt":"2026-08-05T12:00:00+00:00"}`), &msg))
	assert.True(t, time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC).Equal(msg.TrialEndsAt))
}
