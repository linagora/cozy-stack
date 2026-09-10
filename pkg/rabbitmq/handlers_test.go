package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/banner"
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

func TestBannerCommandHandler(t *testing.T) {
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	contextName := "banner-command-handler-test"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		contextName: map[string]interface{}{
			"enable_banners":            true,
			"banner_command_categories": []interface{}{"billing"},
		},
	}
	t.Cleanup(func() { conf.Contexts = previous })

	domain := fmt.Sprintf("banner-handler-%d.example", time.Now().UnixNano())
	_, err := lifecycle.Create(&lifecycle.Options{
		Domain:      domain,
		Email:       "alice@example.org",
		ContextName: contextName,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

	fixture := func(t *testing.T, name string, revision int64) []byte {
		t.Helper()
		raw, err := os.ReadFile("../../model/banner/testdata/" + name + ".json")
		require.NoError(t, err)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &payload))
		payload["workplaceFqdn"] = domain
		payload["revision"] = revision
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		return body
	}

	handle := func(t *testing.T, key string, body []byte) error {
		t.Helper()
		return rabbitmq.NewBannerCommandHandler().
			Handle(context.Background(), amqp.Delivery{RoutingKey: key, Body: body})
	}

	stored := func(t *testing.T) *banner.Banner {
		t.Helper()
		inst, err := lifecycle.GetInstance(domain)
		require.NoError(t, err)
		doc, err := banner.Stored(inst, banner.CategoryBilling)
		require.NoError(t, err)
		return doc
	}

	t.Run("the routing key materializes, then clears", func(t *testing.T) {
		require.NoError(t, handle(t, rabbitmq.RoutingKeyBannerMaterialize, fixture(t, "materialize", 1)))
		require.NotNil(t, stored(t))

		require.NoError(t, handle(t, rabbitmq.RoutingKeyBannerClear, fixture(t, "clear", 2)))
		require.Nil(t, stored(t))
	})

	t.Run("a payload that does not parse fails", func(t *testing.T) {
		err := handle(t, rabbitmq.RoutingKeyBannerMaterialize, []byte("{"))
		require.Error(t, err)
	})

	t.Run("an unexpected routing key fails", func(t *testing.T) {
		err := handle(t, "banner.something", fixture(t, "materialize", 3))
		require.Error(t, err)
	})

	t.Run("an invalid command fails", func(t *testing.T) {
		body := fixture(t, "materialize", 4)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		payload["severity"] = "critical"
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		err = handle(t, rabbitmq.RoutingKeyBannerMaterialize, body)
		require.Error(t, err)
		assert.ErrorIs(t, err, banner.ErrInvalidCommand)
	})

	t.Run("an oversized body is rejected before decoding", func(t *testing.T) {
		err := handle(t, rabbitmq.RoutingKeyBannerMaterialize, []byte(strings.Repeat(" ", banner.MaxCommandBytes+1)))
		assert.ErrorIs(t, err, banner.ErrInvalidCommand)
	})

	t.Run("invalid times never advance desired state", func(t *testing.T) {
		body := fixture(t, "materialize", 100)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		payload["timestamp"] = 1788944400000
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		err = handle(t, rabbitmq.RoutingKeyBannerMaterialize, body)
		assert.ErrorIs(t, err, banner.ErrInvalidCommand)
		// Reusing that revision with valid content succeeds only if the bad
		// command was rejected before the desired-state write.
		require.NoError(t, handle(t, rabbitmq.RoutingKeyBannerMaterialize, fixture(t, "materialize", 100)))
	})

	t.Run("a target that is not here yet is redelivered", func(t *testing.T) {
		body := fixture(t, "materialize", 5)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(body, &payload))
		payload["workplaceFqdn"] = fmt.Sprintf("provisioning-%d.example", time.Now().UnixNano())
		body, err := json.Marshal(payload)
		require.NoError(t, err)

		err = handle(t, rabbitmq.RoutingKeyBannerMaterialize, body)
		require.Error(t, err, "a workplace still being provisioned deserves another attempt")
	})
}
