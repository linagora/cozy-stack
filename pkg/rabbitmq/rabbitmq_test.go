package rabbitmq_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden/settings"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/settings/common"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"

	"golang.org/x/net/context"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/tests/testutils"

	"sync/atomic"

	"github.com/cozy/cozy-stack/pkg/config/config"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/require"
)

func TestRabbitMQManager(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	t.Run("ConsumeAll_NoLoss", func(t *testing.T) {
		t.Parallel()

		// Start broker
		rabbitmq := testutils.StartRabbitMQ(t, true, false)
		totalMessages := 10

		// Build manager with one exchange and one queue using PasswordChangeHandler wrapper
		handler := newCountingTestHandler()
		mgr, err := initRabbitMQManager(rabbitmq.AMQPURL, "auth", "stack.user.password.updated", "password.updated", handler)
		require.NoError(t, err)
		_, err = mgr.Start(testCtx(t))
		require.NoError(t, err)

		// Wait for manager to declare topology
		require.NoError(t, mgr.WaitReady(testCtx(t)))

		sender := newTestMessageSender(t, rabbitmq.AMQPURL, "auth", "password.updated")
		// Publish all messages
		for i := 0; i < totalMessages; i++ {
			msg := testMessage{TimeStamp: time.Now().UnixMilli()}
			sender.publish(msg)
		}

		// Await full consumption
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			return handler.total >= totalMessages
		})
	})

	t.Run("ConsumeAll_WithReconnection", func(t *testing.T) {
		t.Parallel()

		// Start broker
		MQ := testutils.StartRabbitMQ(t, true, false)
		totalMessages := 20

		// Build manager with one exchange and one queue using PasswordChangeHandler wrapper
		handler := newCountingTestHandler()
		mgr, err := initRabbitMQManager(MQ.AMQPURL, "auth", "stack.user.password.updated", "password.updated", handler)
		require.NoError(t, err)
		_, err = mgr.Start(testCtx(t))
		require.NoError(t, err)

		require.NoError(t, mgr.WaitReady(testCtx(t)))

		sender := newTestMessageSender(t, MQ.AMQPURL, "auth", "password.updated")
		// Publish half of the messages
		for i := 0; i < totalMessages/2; i++ {
			sender.publish(testMessage{TimeStamp: time.Now().UnixMilli()})
		}

		// restart the container
		MQ.Stop(context.Background(), 30*time.Second)
		MQ.Restart(context.Background(), 30*time.Second)

		// publish second half
		sender = newTestMessageSender(t, MQ.AMQPURL, "auth", "password.updated")
		// Publish half of the messages
		for i := 0; i < totalMessages/2; i++ {
			sender.publish(testMessage{TimeStamp: time.Now().UnixMilli()})
		}

		// Await full consumption
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			return handler.total >= totalMessages
		})
		require.NoError(t, mgr.Shutdown(testCtx(t)))
	})
}

func TestHandlers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	// Start RabbitMQ broker
	MQ := testutils.StartRabbitMQ(t, false, false)
	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	t.Run("ChangePasswordWithKey", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "ChangePasswordWithKey")
		inst := setup.GetTestInstance()

		slug, _ := SplitDomain(t, inst.Domain)

		// Capture current Bitwarden public/private keys to ensure they are not changed
		bwBefore, err := settings.Get(inst)
		require.NoError(t, err)
		prevPub, prevPriv := bwBefore.PublicKey, bwBefore.PrivateKey

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message
		hashText, hashB64 := hashPassphrase(t)
		msg := rabbitmq.PasswordChangeMessage{
			TwakeID:       slug,
			Iterations:    100000,
			Hash:          hashB64,
			PublicKey:     "PUB",
			Key:           "KEY",
			Timestamp:     time.Now().Unix(),
			WorkplaceFqdn: inst.Domain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"password.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance hash is updated
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			return err == nil && string(updated.PassphraseHash) == hashText
		})

		// Also ensure the key was updated
		bw, err := settings.Get(inst)
		require.NoError(t, err)
		require.Equal(t, msg.Key, bw.Key)
		// Public and private keys must remain unchanged when only Key is provided
		require.Equal(t, prevPub, bw.PublicKey)
		require.Equal(t, prevPriv, bw.PrivateKey)
	})

	t.Run("ChangePasswordWithKeys", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "ChangePasswordWithoutKeys")
		inst := setup.GetTestInstance()

		slug, _ := SplitDomain(t, inst.Domain)

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message
		hashText, hashB64 := hashPassphrase(t)
		msg := rabbitmq.PasswordChangeMessage{
			TwakeID:       slug,
			Iterations:    100000,
			Hash:          hashB64,
			PublicKey:     "PUB",
			PrivateKey:    "PRIV",
			Key:           "KEY",
			Timestamp:     time.Now().Unix(),
			WorkplaceFqdn: inst.Domain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"password.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance hash is updated
		testutils.WaitForOrFail(t, 30*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			return err == nil && string(updated.PassphraseHash) == hashText
		})
	})

	t.Run("ChangePasswordWithoutKeys", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "ChangePasswordWithoutKeys")
		inst := setup.GetTestInstance()

		slug, _ := SplitDomain(t, inst.Domain)

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message
		hashText, hashB64 := hashPassphrase(t)
		msg := rabbitmq.PasswordChangeMessage{
			TwakeID:       slug,
			Iterations:    100000,
			Hash:          hashB64,
			WorkplaceFqdn: inst.Domain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"password.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance hash is updated
		testutils.WaitForOrFail(t, 30*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			return err == nil && string(updated.PassphraseHash) == hashText
		})
	})

	t.Run("CreateUserWithKey", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "CreateUserWithKey")
		inst := setup.GetTestInstance()

		slug, _ := SplitDomain(t, inst.Domain)

		// Capture current Bitwarden public/private keys to ensure they are not changed
		bwBefore, err := settings.Get(inst)
		require.NoError(t, err)
		prevPub, prevPriv := bwBefore.PublicKey, bwBefore.PrivateKey

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message
		hashText, hashB64 := hashPassphrase(t)
		msg := rabbitmq.UserCreatedMessage{
			TwakeID:       slug,
			Iterations:    100000,
			Hash:          hashB64, // send base64-encoded to exercise decode path
			PublicKey:     "PUB",
			Key:           "KEY",
			Timestamp:     time.Now().Unix(),
			WorkplaceFqdn: inst.Domain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		time.Sleep(1 * time.Second)
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"user.created",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance hash is updated (should equal decoded textual scrypt hash)
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			return err == nil && string(updated.PassphraseHash) == hashText
		})

		// Also ensure the key was updated
		bw, err := settings.Get(inst)
		require.NoError(t, err)
		require.Equal(t, msg.Key, bw.Key)
		// Public and private keys must remain unchanged when only Key is provided
		require.Equal(t, prevPub, bw.PublicKey)
		require.Equal(t, prevPriv, bw.PrivateKey)
	})

	t.Run("CreateUserWithoutHashInForcedOIDCContext", func(t *testing.T) {
		setup := setUpRabbitMQConfig(t, MQ, "CreateUserWithoutHashInForcedOIDCContext")
		cfg := config.GetConfig()
		prevAuthentication := cfg.Authentication
		const oidcContext = "oidc-no-password-context"
		cfg.Authentication = map[string]interface{}{
			oidcContext: map[string]interface{}{
				"disable_password_authentication": true,
			},
		}
		t.Cleanup(func() {
			cfg.Authentication = prevAuthentication
		})

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "no-hash-org-" + suffix + ".example"
		orgID := "org-no-hash-" + suffix
		targetEmail := "target-" + suffix + "@example.com"
		target := setup.GetTestInstance(&lifecycle.Options{
			Domain:      "no-hash-target-" + suffix + ".local",
			ContextName: oidcContext,
			OrgDomain:   orgDomain,
			OrgID:       orgID,
			Email:       targetEmail,
			PublicName:  "Target User",
		})
		other := createInstanceInOrg(
			t,
			"no-hash-other-"+suffix+".local",
			orgDomain,
			orgID,
			"other-"+suffix+"@example.com",
			"Other User",
		)

		initialHash := string(target.PassphraseHash)
		require.NotEmpty(t, initialHash)
		require.NotNil(t, target.PasswordDefined)
		require.False(t, *target.PasswordDefined)

		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		slug, _ := SplitDomain(t, target.Domain)
		msg := rabbitmq.UserCreatedMessage{
			TwakeID:            slug,
			Mobile:             "+33700000000",
			InternalEmail:      targetEmail,
			Timestamp:          time.Now().Unix(),
			WorkplaceFqdn:      target.Domain,
			OrganizationID:     orgID,
			OrganizationDomain: orgDomain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"user.created",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			matches, err := contact.FindAllByEmail(other, targetEmail)
			if err != nil {
				return false
			}
			for _, doc := range matches {
				if doc.IsExternal() && doc.IsTrusted() {
					return true
				}
			}
			return false
		})

		updated, err := lifecycle.GetInstance(target.Domain)
		require.NoError(t, err)
		require.Equal(t, initialHash, string(updated.PassphraseHash))
		require.NotNil(t, updated.PasswordDefined)
		require.False(t, *updated.PasswordDefined)
	})

	t.Run("DeleteUserHandler", func(t *testing.T) {
		setup := setUpRabbitMQConfig(t, MQ, "DeleteUserHandler")
		_ = setup.GetTestInstance()

		suffix := fmt.Sprintf("%d", time.Now().UnixNano())
		orgDomain := "delete-handler-" + suffix + ".example"
		orgID := "org-delete-handler-" + suffix
		target := createInstanceInOrg(t, "delete-handler-alice-"+suffix+".local", orgDomain, orgID, "alice@example.com", "Alice")
		bob := createInstanceInOrg(t, "delete-handler-bob-"+suffix+".local", orgDomain, orgID, "bob@example.com", "Bob")

		createContact(t, bob, "alice@example.com", target.PageURL("", nil), true, "Alice External")

		ch, err := getChannel(t, MQ)
		require.NoError(t, err)
		waitForDeclaredQueues(t, MQ, queuePassiveCheck{
			name:         rabbitmq.QueueB2BUserDeleted,
			dlRoutingKey: rabbitmq.RoutingKeyB2BUserDeleted,
		})

		msg := rabbitmq.UserDeletedMessage{
			Emitter:        "admin-panel",
			Type:           "user.deleted",
			WorkplaceFqdn:  target.Domain,
			InternalEmail:  "alice@example.com",
			Reason:         "user deleted",
			OrganizationID: orgID,
			UserID:         "alice",
			Domain:         orgDomain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		err = ch.PublishWithContext(
			testCtx(t),
			"b2b",
			rabbitmq.RoutingKeyB2BUserDeleted,
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			_, err := contact.FindAllByEmail(bob, "alice@example.com")
			return err == contact.ErrNotFound
		})
	})

	t.Run("UpdateUserPhone", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "UpdateUserPhone")
		inst := setup.GetTestInstance()

		slug, _ := SplitDomain(t, inst.Domain)

		// Configure common settings to enable the code path
		cfg := config.GetConfig()
		cfg.CommonSettings = map[string]config.CommonSettings{
			config.DefaultInstanceContext: {URL: "http://example.org", Token: "token"},
			"":                            {URL: "http://example.org", Token: "token"},
		}

		// Track if common settings HTTP call was made and capture request body
		var capturedRequestBody []byte
		commonSettingsCalled := false
		originalDoCommonHTTP := common.DoCommonHTTP
		common.DoCommonHTTP = func(method, urlStr, token string, body []byte) error {
			commonSettingsCalled = true
			capturedRequestBody = body
			return nil // Mock successful response
		}
		t.Cleanup(func() {
			common.DoCommonHTTP = originalDoCommonHTTP
		})

		// Stub remote getter to return in-sync version
		originalGetRemote := common.GetRemoteCommonSettings
		common.GetRemoteCommonSettings = func(inst *instance.Instance) (*common.UserSettingsRequest, error) {
			return &common.UserSettingsRequest{Version: inst.CommonSettingsVersion}, nil
		}
		t.Cleanup(func() {
			common.GetRemoteCommonSettings = originalGetRemote
		})

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message
		msg := rabbitmq.UserPhoneUpdatedMessage{
			TwakeID:       slug,
			Mobile:        "+33700000018",
			InternalEmail: fmt.Sprintf("%s@%s", slug, rabbitmq.DefaultDomain),
			WorkplaceFqdn: inst.Domain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"user.phone.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance phone setting is updated
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			if err != nil {
				return false
			}
			settingsDoc, err := updated.SettingsDocument()
			if err != nil {
				return false
			}
			phone, _ := settingsDoc.M["phone"].(string)
			return phone == msg.Mobile
		})

		updated, err := lifecycle.GetInstance(inst.Domain)
		require.NoError(t, err)
		settingsDoc, err := updated.SettingsDocument()
		require.NoError(t, err)
		phone, _ := settingsDoc.M["phone"].(string)
		require.Equal(t, msg.Mobile, phone)

		// Verify that common settings were called
		require.True(t, commonSettingsCalled, "Common settings should have been called when updating phone")

		// Verify that the phone field is present in the request body sent to common settings API
		require.NotNil(t, capturedRequestBody, "Request body should have been captured")
		var sentRequest common.UserSettingsRequest
		require.NoError(t, json.Unmarshal(capturedRequestBody, &sentRequest))
		require.Equal(t, msg.Mobile, sentRequest.Payload.Phone, "Phone should be present in the common settings request payload")
	})

	t.Run("UpdateUser2FA_Enable", func(t *testing.T) {
		setup := setUpRabbitMQConfig(t, MQ, "UpdateUser2FA_Enable")
		inst := setup.GetTestInstance()

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message: enable 2FA
		msg := rabbitmq.User2FAUpdatedMessage{
			TwoFactorEnabled: true,
			WorkplaceFqdn:    inst.Domain,
			InternalEmail:    fmt.Sprintf("user@%s", rabbitmq.DefaultDomain),
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"user.2fa.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance auth mode is updated
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			if err != nil {
				return false
			}
			return updated.AuthMode == instance.TwoFactorOIDC
		})

		updated, err := lifecycle.GetInstance(inst.Domain)
		require.NoError(t, err)
		require.Equal(t, instance.TwoFactorOIDC, updated.AuthMode)
	})

	t.Run("UpdateUserRecoveryEmail", func(t *testing.T) {
		setup := setUpRabbitMQConfig(t, MQ, "UpdateUserRecoveryEmail")
		inst := setup.GetTestInstance()

		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		msg := rabbitmq.UserRecoveryEmailUpdatedMessage{
			RecoveryEmail: "recovery@example.com",
			InternalEmail: fmt.Sprintf("user@%s", rabbitmq.DefaultDomain),
			WorkplaceFqdn: inst.Domain,
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"user.recovery-email.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			if err != nil {
				return false
			}
			settings, err := updated.SettingsDocument()
			if err != nil {
				return false
			}
			recoveryEmail, _ := settings.M["recovery_email"].(string)
			return recoveryEmail == "recovery@example.com"
		})

		updated, err := lifecycle.GetInstance(inst.Domain)
		require.NoError(t, err)
		settings, err := updated.SettingsDocument()
		require.NoError(t, err)
		require.Equal(t, "recovery@example.com", settings.M["recovery_email"])
	})

	t.Run("UpdateUser2FA_Disable", func(t *testing.T) {
		setup := setUpRabbitMQConfig(t, MQ, "UpdateUser2FA_Disable")
		inst := setup.GetTestInstance()

		// Pre-set the instance to TwoFactorOIDC
		err := lifecycle.UpdateAuthMode(inst, instance.TwoFactorOIDC)
		require.NoError(t, err)

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message: disable 2FA
		msg := rabbitmq.User2FAUpdatedMessage{
			TwoFactorEnabled: false,
			WorkplaceFqdn:    inst.Domain,
			InternalEmail:    fmt.Sprintf("user@%s", rabbitmq.DefaultDomain),
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"auth",
			"user.2fa.updated",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until the instance auth mode is reset
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			if err != nil {
				return false
			}
			return updated.AuthMode == instance.Basic
		})

		updated, err := lifecycle.GetInstance(inst.Domain)
		require.NoError(t, err)
		require.Equal(t, instance.Basic, updated.AuthMode)
	})

	t.Run("ChangeOrganizationSubscription", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "ChangeOrganizationSubscription")

		// Create instance outside organization
		inst := setup.GetTestInstance()

		// Create instances inside organization
		orgDomain := "myorg.com"
		orgInst1, err := lifecycle.Create(&lifecycle.Options{
			Domain:    "orgInst1.local",
			OrgDomain: orgDomain,
		})
		require.NoError(t, err, "Cannot create orgInst1")
		orgInst2, err := lifecycle.Create(&lifecycle.Options{
			Domain:    "orgInst2.local",
			OrgDomain: orgDomain,
		})
		require.NoError(t, err, "Cannot create orgInst2")
		t.Cleanup(func() {
			_ = lifecycle.Destroy(orgInst1.Domain)
			_ = lifecycle.Destroy(orgInst2.Domain)
		})

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message
		msg := rabbitmq.DomainSubscriptionChangedMessage{
			Domain:     orgDomain,
			IsPaying:   true,
			CanUpgrade: true,
			Features: rabbitmq.SubscriptionFeatures{
				Stack: rabbitmq.SubscriptionStackFeatures{
					FeatureSets: []string{"plan-uuid"},
					DiskQuota:   50000000000,
				},
			},
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"billing",
			"domain.subscription.changed",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait until orgInst1 is updated
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(orgInst1.Domain)
			if err != nil {
				return false
			}
			if len(updated.FeatureSets) == 0 {
				return false
			}
			return updated.FeatureSets[0] == msg.Features.Stack.FeatureSets[0]
		})

		// Wait until orgInst2 is updated (the handler updates instances sequentially)
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(orgInst2.Domain)
			if err != nil {
				return false
			}
			if len(updated.FeatureSets) == 0 {
				return false
			}
			return updated.FeatureSets[0] == msg.Features.Stack.FeatureSets[0]
		})

		// orgInst1 is updated
		updated, err := lifecycle.GetInstance(orgInst1.Domain)
		require.NoError(t, err)
		quota := updated.DiskQuota()
		require.Equal(t, msg.Features.Stack.DiskQuota, quota)
		require.ElementsMatch(t, msg.Features.Stack.FeatureSets, updated.FeatureSets)

		// orgInst2 is updated
		updated, err = lifecycle.GetInstance(orgInst2.Domain)
		require.NoError(t, err)
		quota = updated.DiskQuota()
		require.Equal(t, msg.Features.Stack.DiskQuota, quota)
		require.ElementsMatch(t, msg.Features.Stack.FeatureSets, updated.FeatureSets)

		// inst is not updated
		updated, err = lifecycle.GetInstance(inst.Domain)
		require.NoError(t, err)
		quota = updated.DiskQuota()
		require.Equal(t, inst.DiskQuota(), quota)
		require.ElementsMatch(t, inst.FeatureSets, updated.FeatureSets)
	})

	t.Run("InstallApp", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "InstallApp")
		inst := setup.GetTestInstance()

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose message with the new format
		msg := rabbitmq.AppInstallMessage{
			Emitter:       "admin-panel",
			Type:          "app.install",
			WorkplaceFqdn: inst.Domain,
			InternalEmail: "test@twake.app",
			Reason:        "admin user created",
			Slug:          "drive",
			Source:        "registry://drive/stable",
			Timestamp:     time.Now().Unix(),
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"stack",
			"app.install",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait for the message to be processed
		// Note: The app installation may fail if the app doesn't exist in the registry,
		// but we can at least verify the message was received and processed
		testutils.WaitForOrFail(t, 30*time.Second, func() bool {
			// Check if the app was installed (or if it already exists, that's OK too)
			// We check by trying to get the app
			_, errWebapp := app.GetBySlug(inst, "drive", consts.WebappType)
			_, errKonnector := app.GetBySlug(inst, "drive", consts.KonnectorType)
			// If either succeeds, the app exists (installation succeeded or already existed)
			return errWebapp == nil || errKonnector == nil
		})

		// Verify the app exists (either as webapp or konnector)
		_, errWebapp := app.GetBySlug(inst, "drive", consts.WebappType)
		_, errKonnector := app.GetBySlug(inst, "drive", consts.KonnectorType)
		require.True(t, errWebapp == nil || errKonnector == nil, "App should be installed (or already exist)")
	})

	t.Run("UninstallApp", func(t *testing.T) {
		// Configure RabbitMQ before starting the stack so it is initialized by the stack
		setup := setUpRabbitMQConfig(t, MQ, "UninstallApp")
		inst := setup.GetTestInstance()

		// First, install the drive app so we can uninstall it
		// Note: This assumes the app exists in the registry, or we can skip if it doesn't
		installer, err := app.NewInstaller(inst, app.Copier(consts.WebappType, inst), &app.InstallerOptions{
			Operation:  app.Install,
			Type:       consts.WebappType,
			SourceURL:  "registry://drive/stable",
			Slug:       "drive",
			Registries: inst.Registries(),
		})
		if err == nil {
			_, err = installer.RunSync()
			// If installation fails or app already exists, that's OK - we just need it to exist
		}

		// Publisher conn/channel
		ch, err := getChannel(t, MQ)
		require.NoError(t, err)

		// Compose uninstall message for the drive app
		msg := rabbitmq.AppInstallMessage{
			Emitter:       "admin-panel",
			Type:          "app.uninstall",
			WorkplaceFqdn: inst.Domain,
			InternalEmail: "test@twake.app",
			Reason:        "admin user demoted",
			Slug:          "drive",
		}
		body, err := json.Marshal(msg)
		require.NoError(t, err)

		// Publish
		err = ch.PublishWithContext(
			testCtx(t),
			"stack",
			"app.install",
			false,
			false,
			amqp.Publishing{
				DeliveryMode: amqp.Persistent,
				ContentType:  "application/json",
				Body:         body,
				MessageId:    fmt.Sprintf("%d", time.Now().UnixNano()),
			},
		)
		require.NoError(t, err)

		// Wait for the message to be processed
		// Check that the drive app was uninstalled (or didn't exist in the first place)
		testutils.WaitForOrFail(t, 30*time.Second, func() bool {
			// Check if the app no longer exists
			_, errWebapp := app.GetBySlug(inst, "drive", consts.WebappType)
			_, errKonnector := app.GetBySlug(inst, "drive", consts.KonnectorType)
			// If both fail, the app doesn't exist (uninstallation succeeded or app didn't exist)
			return errWebapp != nil && errKonnector != nil
		})

		// Verify the drive app no longer exists
		_, errWebapp := app.GetBySlug(inst, "drive", consts.WebappType)
		_, errKonnector := app.GetBySlug(inst, "drive", consts.KonnectorType)
		require.True(t, errWebapp != nil && errKonnector != nil, "App should be uninstalled (or not exist)")
	})
}

func TestUserDeletedHandlerValidation(t *testing.T) {
	t.Run("MissingWorkplaceFqdn", func(t *testing.T) {
		body, err := json.Marshal(rabbitmq.UserDeletedMessage{
			InternalEmail: "alice@example.com",
			Domain:        "example.com",
		})
		require.NoError(t, err)

		err = rabbitmq.NewUserDeletedHandler().Handle(testCtx(t), amqp.Delivery{
			RoutingKey: rabbitmq.RoutingKeyB2BUserDeleted,
			Body:       body,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing workplaceFqdn")
	})

	t.Run("MissingInternalEmail", func(t *testing.T) {
		body, err := json.Marshal(rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: "alice.example.com",
			Domain:        "example.com",
		})
		require.NoError(t, err)

		err = rabbitmq.NewUserDeletedHandler().Handle(testCtx(t), amqp.Delivery{
			RoutingKey: rabbitmq.RoutingKeyB2BUserDeleted,
			Body:       body,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing internalEmail")
	})

	t.Run("MissingOrganizationDomain", func(t *testing.T) {
		body, err := json.Marshal(rabbitmq.UserDeletedMessage{
			WorkplaceFqdn: "alice.example.com",
			InternalEmail: "alice@example.com",
		})
		require.NoError(t, err)

		err = rabbitmq.NewUserDeletedHandler().Handle(testCtx(t), amqp.Delivery{
			RoutingKey: rabbitmq.RoutingKeyB2BUserDeleted,
			Body:       body,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "missing organizationId or organization domain")
	})
}

func hashPassphrase(t *testing.T) (string, string) {
	t.Helper()
	passphrase := []byte("super-secret-create-user-key")
	hashBytes, err := crypto.GenerateFromPassphrase(passphrase)
	require.NoError(t, err)
	hashText := string(hashBytes)
	hashB64 := base64.StdEncoding.EncodeToString(hashBytes)
	return hashText, hashB64
}

func getChannel(t *testing.T, mq *testutils.RabbitFixture) (*amqp.Channel, error) {
	t.Helper()
	conn, err := amqp.Dial(mq.AMQPURL)
	require.NoError(t, err)
	ch, err := conn.Channel()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ch.Close(); _ = conn.Close() })
	return ch, err
}

type queuePassiveCheck struct {
	name         string
	dlRoutingKey string
}

func waitForDeclaredQueues(t *testing.T, mq *testutils.RabbitFixture, checks ...queuePassiveCheck) {
	t.Helper()

	testutils.WaitForOrFail(t, 10*time.Second, func() bool {
		conn, err := amqp.Dial(mq.AMQPURL)
		if err != nil {
			return false
		}
		defer func() { _ = conn.Close() }()

		ch, err := conn.Channel()
		if err != nil {
			return false
		}
		defer func() { _ = ch.Close() }()

		for _, check := range checks {
			_, err = ch.QueueInspect(check.name)
			if err != nil {
				return false
			}
		}
		return true
	})
}

func createInstanceInOrg(t *testing.T, domain, orgDomain, orgID, email, publicName string, phone ...string) *instance.Instance {
	t.Helper()
	opts := &lifecycle.Options{
		Domain:     domain,
		OrgDomain:  orgDomain,
		OrgID:      orgID,
		Email:      email,
		PublicName: publicName,
	}
	if len(phone) > 0 {
		opts.Phone = phone[0]
	}
	inst, err := lifecycle.Create(opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(inst.Domain) })
	return inst
}

func createContact(t *testing.T, inst *instance.Instance, email, cozyURL string, external bool, name string) *contact.Contact {
	t.Helper()
	c, err := contact.Create(inst, contact.CreateOptions{
		Email:    email,
		Name:     name,
		CozyURL:  cozyURL,
		External: external,
	})
	require.NoError(t, err)
	return c
}

func setUpRabbitMQConfig(t *testing.T, mq *testutils.RabbitFixture, name string) *testutils.TestSetup {
	cfg := config.GetConfig()

	cfg.RabbitMQ.Enabled = true
	cfg.RabbitMQ.Nodes = map[string]config.RabbitMQNode{
		"default": {
			Enabled: true,
			URL:     mq.AMQPURL,
		},
	}
	cfg.RabbitMQ.Exchanges = []config.RabbitExchange{
		{
			Name:            "auth",
			Kind:            "topic",
			Durable:         true,
			DeclareExchange: true,
			Queues: []config.RabbitQueue{
				{
					Name:     "stack.user.password.updated",
					Bindings: []string{"password.updated"},
					Prefetch: 4,
					Declare:  true,
				},
				{
					Name:     "stack.user.created",
					Bindings: []string{"user.created"},
					Prefetch: 4,
					Declare:  true,
				},
				{
					Name:     "stack.user.phone.updated",
					Bindings: []string{"user.phone.updated"},
					Prefetch: 4,
					Declare:  true,
				},
				{
					Name:     "stack.user.2fa.updated",
					Bindings: []string{"user.2fa.updated"},
					Prefetch: 4,
					Declare:  true,
				},
				{
					Name:     "stack.user.recovery-email.updated",
					Bindings: []string{"user.recovery-email.updated"},
					Prefetch: 4,
					Declare:  true,
				},
			},
		},
		{
			Name:            "billing",
			Kind:            "topic",
			Durable:         true,
			DeclareExchange: true,
			Queues: []config.RabbitQueue{
				{
					Name:     "stack.domain.subscription.changed",
					Bindings: []string{"domain.subscription.changed"},
					Prefetch: 4,
					Declare:  true,
				},
			},
		},
		{
			Name:            "b2b",
			Kind:            "topic",
			Durable:         true,
			DeclareExchange: true,
			Queues: []config.RabbitQueue{
				{
					Name:     "stack.b2b.user.deleted",
					Bindings: []string{rabbitmq.RoutingKeyB2BUserDeleted},
					Prefetch: 4,
					Declare:  true,
				},
			},
		},
		{
			Name:            "stack",
			Kind:            "topic",
			Durable:         true,
			DeclareExchange: true,
			Queues: []config.RabbitQueue{
				{
					Name:     "stack.app.commands.queue",
					Bindings: []string{"app.install", "app.uninstall"},
					Prefetch: 4,
					Declare:  true,
				},
			},
		},
	}

	setup := testutils.NewSetup(t, name)
	return setup
}

func initConnection(t *testing.T, mgr *rabbitmq.RabbitMQConnection) *amqp.Connection {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := mgr.Connect(ctx, 5)
	require.NoError(t, err)
	require.NotNil(t, conn)
	return conn
}

func declareTestQueue(t *testing.T, conn *amqp.Connection, name string) {
	t.Helper()
	ch, err := conn.Channel()
	require.NoError(t, err)
	_, err = ch.QueueDeclare(name, false, true, false, false, nil)
	require.NoError(t, err)
	err = ch.Close()
	require.NoError(t, err)
}

func TestConnection(t *testing.T) {
	t.Parallel()

	t.Run("InitConnectionAndQueue", func(t *testing.T) {
		t.Parallel()

		f := testutils.StartRabbitMQ(t, false, false)

		connMgr := rabbitmq.NewRabbitMQConnection(f.AMQPURL)
		conn := initConnection(t, connMgr)

		// queue declaration should work find
		declareTestQueue(t, conn, "testcontainers-queue")
		require.NoError(t, connMgr.Close(), "can")
	})

	t.Run("MonitorClose", func(t *testing.T) {
		t.Parallel()

		MQ := testutils.StartRabbitMQ(t, false, false)

		mgr := rabbitmq.NewRabbitMQConnection(MQ.AMQPURL)

		_ = initConnection(t, mgr)

		monitor := mgr.MonitorConnection()
		require.NotNil(t, monitor)

		// stop container
		MQ.Stop(context.Background(), 30*time.Second)

		// asserts a close notification is received.
		select {
		case <-time.After(30 * time.Second):
			t.Fatalf("did not receive connection close notification within timeout")
		case err := <-monitor:
			require.Error(t, err)
		}
	})

	t.Run("ReconnectAfterDowntime", func(t *testing.T) {
		t.Parallel()

		MQ := testutils.StartRabbitMQ(t, false, false)

		conn := rabbitmq.NewRabbitMQConnection(MQ.AMQPURL)

		// Initial connection
		_ = initConnection(t, conn)

		// Stop broker
		MQ.Stop(context.Background(), 30*time.Second)

		// Restart container,update manager, reconnect
		MQ.Restart(context.Background(), 30*time.Second)

		// update manager, reconnect
		rconn, err := conn.Connect(context.Background(), 10)
		require.NoError(t, err)
		require.NotNil(t, rconn)
	})

	t.Run("ConnectWithTLS", func(t *testing.T) {
		t.Parallel()

		f := testutils.StartRabbitMQ(t, false, true)

		cm := rabbitmq.NewRabbitMQConnection(f.AMQPURL)
		cm.TLSConfig = getTlsConfig(t)

		conn := initConnection(t, cm)

		declareTestQueue(t, conn, "tls-test-queue")
		require.NoError(t, cm.Close())
	})

	t.Run("ConnectWithTLSIgnoreCA", func(t *testing.T) {
		t.Parallel()

		f := testutils.StartRabbitMQ(t, false, true)

		cm := rabbitmq.NewRabbitMQConnection(f.AMQPURL)
		cm.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true}

		conn := initConnection(t, cm)

		declareTestQueue(t, conn, "tls-test-queue")
		require.NoError(t, cm.Close())
	})
}

func certRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(thisFile)
	return filepath.Join(testDir, "testdata")
}

func getTlsConfig(t *testing.T) *tls.Config {
	t.Helper()
	caPEM, err := os.ReadFile(filepath.Join(certRoot(), "ca.pem"))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(caPEM))
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	return tlsCfg
}

type countingTestHandler struct {
	total int
}

func newCountingTestHandler() *countingTestHandler {
	return &countingTestHandler{
		total: 0,
	}
}

func (h *countingTestHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	h.total++
	return nil
}

type countingFailingHandler struct{ calls atomic.Int64 }

func (h *countingFailingHandler) Handle(ctx context.Context, d amqp.Delivery) error {
	h.calls.Add(1)
	return fmt.Errorf("forced failure")
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func initRabbitMQManager(amqpURL string, exchangeName string, queueName string, routingKey string, handler *countingTestHandler) (*rabbitmq.RabbitMQManager, error) {
	nodeCfg := config.RabbitMQNode{
		Enabled: true,
		URL:     amqpURL,
	}
	exchangeCfg := &config.RabbitExchange{
		Name:            exchangeName,
		Kind:            "topic",
		DeclareExchange: true,
		Durable:         true,
	}
	queueCfg := &config.RabbitQueue{
		Name:          queueName,
		Bindings:      []string{routingKey},
		Declare:       true,
		Prefetch:      8,
		DeliveryLimit: 5,
	}

	connection, err := rabbitmq.BuildConnection(nodeCfg)
	if err != nil {
		return nil, err
	}
	exchange := rabbitmq.NewExchangeSpec(exchangeCfg)
	queue := rabbitmq.NewQueueSpec(queueCfg, handler, "", "")
	exchange.Queues = []rabbitmq.QueueSpec{queue}

	exchanges := []rabbitmq.ExchangeSpec{exchange}
	return rabbitmq.NewRabbitMQManager(connection, exchanges), nil
}

type testMessage struct {
	TimeStamp int64
}

type testMessageSender struct {
	t            *testing.T
	ch           *amqp.Channel
	exchangeName string
	routingKey   string
}

func newTestMessageSender(t *testing.T, amqpURL string, exchangeName string, routingKey string) *testMessageSender {
	conn := rabbitmq.NewRabbitMQConnection(amqpURL)
	pubConn, err := conn.Connect(context.Background(), 5)
	require.NoError(t, err)
	pubCh, err := pubConn.Channel()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pubCh.Close() })
	return &testMessageSender{
		t:            t,
		ch:           pubCh,
		exchangeName: exchangeName,
		routingKey:   routingKey,
	}
}

func (m *testMessageSender) publish(msg any) {
	m.t.Helper()
	body, err := json.Marshal(msg)
	require.NoError(m.t, err)
	require.NoError(m.t, m.ch.PublishWithContext(
		context.Background(),
		m.exchangeName,
		m.routingKey,
		false,
		false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		},
	))
}

func TestDLXDLQDeclaration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	t.Run("DeclareDLXAndDLQ_WhenConfigured", func(t *testing.T) {
		t.Parallel()

		MQ := testutils.StartRabbitMQ(t, true, false)
		defer MQ.Stop(context.Background(), 30*time.Second)

		// Create configuration with DLX/DLQ enabled
		exchangeCfg := &config.RabbitExchange{
			Name:            "test-exchange",
			Kind:            "topic",
			DeclareExchange: true,
			Durable:         true,
			DLXName:         "test-dlx",
			DLQName:         "test-dlq",
		}
		queueCfg := &config.RabbitQueue{
			Name:       "test-queue",
			Bindings:   []string{"test.routing.key"},
			Declare:    true,
			DeclareDLX: true,
			DeclareDLQ: true,
			DLXName:    "test-dlx",
			DLQName:    "test-dlq",
		}

		mgr := createAndStartManager(t, MQ, exchangeCfg, queueCfg, "test-dlx", "test-dlq")
		defer mgr.Shutdown(testCtx(t))

		verifyDLXExists(t, MQ, "test-dlx")
		verifyDLQExists(t, MQ, "test-dlq")
	})

	t.Run("DoNotDeclareDLXAndDLQ_WhenNotConfigured", func(t *testing.T) {
		t.Parallel()

		MQ := testutils.StartRabbitMQ(t, true, false)
		defer MQ.Stop(context.Background(), 30*time.Second)

		// Create configuration without DLX/DLQ enabled
		exchangeCfg := &config.RabbitExchange{
			Name:            "test-exchange",
			Kind:            "topic",
			DeclareExchange: true,
			Durable:         true,
		}
		queueCfg := &config.RabbitQueue{
			Name:       "test-queue",
			Bindings:   []string{"test.routing.key"},
			Declare:    true,
			DeclareDLX: false,
			DeclareDLQ: false,
		}

		mgr := createAndStartManager(t, MQ, exchangeCfg, queueCfg, "", "")
		defer mgr.Shutdown(testCtx(t))

		verifyDLXNotExists(t, MQ, "test-dlx")
		verifyDLQNotExists(t, MQ, "test-dlq")
	})

	t.Run("DeclareOnlyDLX_WhenOnlyDLXConfigured", func(t *testing.T) {
		t.Parallel()

		MQ := testutils.StartRabbitMQ(t, true, false)
		defer MQ.Stop(context.Background(), 30*time.Second)

		// Create configuration with only DLX enabled
		exchangeCfg := &config.RabbitExchange{
			Name:            "test-exchange",
			Kind:            "topic",
			DeclareExchange: true,
			Durable:         true,
			DLXName:         "test-dlx-only",
		}
		queueCfg := &config.RabbitQueue{
			Name:       "test-queue",
			Bindings:   []string{"test.routing.key"},
			Declare:    true,
			DeclareDLX: true,
			DeclareDLQ: false,
			DLXName:    "test-dlx-only",
		}

		mgr := createAndStartManager(t, MQ, exchangeCfg, queueCfg, "test-dlx-only", "")
		defer mgr.Shutdown(testCtx(t))

		verifyDLXExists(t, MQ, "test-dlx-only")
		verifyDLQNotExists(t, MQ, "test-dlq-only")
	})

	t.Run("DeclareOnlyDLQ_WhenOnlyDLQConfigured", func(t *testing.T) {
		t.Parallel()

		MQ := testutils.StartRabbitMQ(t, true, false)
		defer MQ.Stop(context.Background(), 30*time.Second)

		// Create configuration with only DLQ enabled
		exchangeCfg := &config.RabbitExchange{
			Name:            "test-exchange",
			Kind:            "topic",
			DeclareExchange: true,
			Durable:         true,
			DLQName:         "test-dlq-only",
		}
		queueCfg := &config.RabbitQueue{
			Name:       "test-queue",
			Bindings:   []string{"test.routing.key"},
			Declare:    true,
			DeclareDLX: false,
			DeclareDLQ: true,
			DLQName:    "test-dlq-only",
		}

		mgr := createAndStartManager(t, MQ, exchangeCfg, queueCfg, "", "test-dlq-only")
		defer mgr.Shutdown(testCtx(t))

		verifyDLXNotExists(t, MQ, "test-dlx-only")
		verifyDLQExists(t, MQ, "test-dlq-only")
	})
}

// createAndStartManager creates a RabbitMQ manager with the given configuration and starts it
func createAndStartManager(t *testing.T, mq *testutils.RabbitFixture, exchangeCfg *config.RabbitExchange, queueCfg *config.RabbitQueue, dlxName, dlqName string) *rabbitmq.RabbitMQManager {
	connection, err := rabbitmq.BuildConnection(config.RabbitMQNode{
		Enabled: true,
		URL:     mq.AMQPURL,
	})
	require.NoError(t, err)
	handler := newCountingTestHandler()
	exchange := rabbitmq.NewExchangeSpec(exchangeCfg)
	queue := rabbitmq.NewQueueSpec(queueCfg, handler, dlxName, dlqName)
	exchange.Queues = []rabbitmq.QueueSpec{queue}

	exchanges := []rabbitmq.ExchangeSpec{exchange}
	mgr := rabbitmq.NewRabbitMQManager(connection, exchanges)

	_, err = mgr.Start(testCtx(t))
	require.NoError(t, err)
	require.NoError(t, mgr.WaitReady(testCtx(t)))

	// Give some time for DLX/DLQ to be declared
	time.Sleep(100 * time.Millisecond)

	return mgr
}

func verifyDLXExists(t *testing.T, mq *testutils.RabbitFixture, dlxName string) {
	conn, ch := testutils.CreateRabbitConnection(t, mq)
	defer ch.Close()
	defer conn.Close()

	// Try to declare DLX with different parameters - should fail because it already exists
	err := ch.ExchangeDeclare(dlxName, "direct", true, false, false, false, nil)
	require.Error(t, err, "DLX %s should already be declared with different type", dlxName)
}

func verifyDLQExists(t *testing.T, mq *testutils.RabbitFixture, dlqName string) {
	conn, ch := testutils.CreateRabbitConnection(t, mq)
	defer ch.Close()
	defer conn.Close()

	// Try to declare DLQ with different parameters - should fail because it already exists
	_, err := ch.QueueDeclare(dlqName, false, false, false, false, nil)
	require.Error(t, err, "DLQ %s should already be declared with different durability", dlqName)
}

func verifyDLXNotExists(t *testing.T, mq *testutils.RabbitFixture, dlxName string) {
	conn, ch := testutils.CreateRabbitConnection(t, mq)
	defer ch.Close()
	defer conn.Close()

	// Try to declare DLX - should succeed because it doesn't exist
	err := ch.ExchangeDeclare(dlxName, "fanout", true, false, false, false, nil)
	require.NoError(t, err, "DLX %s should not be declared yet", dlxName)
}

func verifyDLQNotExists(t *testing.T, mq *testutils.RabbitFixture, dlqName string) {
	conn, ch := testutils.CreateRabbitConnection(t, mq)
	defer ch.Close()
	defer conn.Close()

	// Try to declare DLQ - should succeed because it doesn't exist
	_, err := ch.QueueDeclare(dlqName, true, false, false, false, nil)
	require.NoError(t, err, "DLQ %s should not be declared yet", dlqName)
}

func TestRouteToDLQ_OnDeliveryLimitExceeded(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	t.Parallel()

	MQ := testutils.StartRabbitMQ(t, true, false)
	defer MQ.Stop(context.Background(), 30*time.Second)

	// Configuraion of node
	nodeCfg := config.RabbitMQNode{
		Enabled: true,
		URL:     MQ.AMQPURL,
	}
	// Configuration per request
	exchangeCfg := &config.RabbitExchange{
		Name:            "auth",
		Kind:            "topic",
		Durable:         true,
		DeclareExchange: true,
		DLXName:         "auth.dlx",
	}
	queueCfg := &config.RabbitQueue{
		Name:          "user.password.updated",
		Declare:       true,
		Prefetch:      8,
		DeliveryLimit: 5,
		DeclareDLQ:    true,
		DLQName:       "stack.dead.letter.user.password.updated",
		Bindings:      []string{"password.updated"},
		// Declare DLX so the DLX exists even if not pre-created
		DeclareDLX: true,
		// Route dead letters via a specific routing key on a topic DLX
		DLRoutingKey: "password.updated.dead",
	}

	// Create manager with a failing handler to force redeliveries and count attempts
	handler := &countingFailingHandler{}
	connection, err := rabbitmq.BuildConnection(nodeCfg)
	require.NoError(t, err)
	exchange := rabbitmq.NewExchangeSpec(exchangeCfg)
	queue := rabbitmq.NewQueueSpec(queueCfg, handler, "auth.dlx", "stack.dead.letter.user.password.updated")
	exchange.Queues = []rabbitmq.QueueSpec{queue}
	mgr := rabbitmq.NewRabbitMQManager(connection, []rabbitmq.ExchangeSpec{exchange})

	// Start consumers
	_, err = mgr.Start(testCtx(t))
	require.NoError(t, err)
	require.NoError(t, mgr.WaitReady(testCtx(t)))

	// Publish one message to be routed to the main queue
	sender := newTestMessageSender(t, MQ.AMQPURL, "auth", "password.updated")
	original := testMessage{TimeStamp: time.Now().UnixMilli()}
	sender.publish(original)

	// Expect the message to be dead-lettered to the DLQ after exceeding delivery limit
	if msg, ok := testutils.GetOneFromQueue(t, MQ, "stack.dead.letter.user.password.updated", 60*time.Second); !ok {
		t.Fatalf("expected a message in DLQ but none received")
	} else {
		require.Equal(t, "application/json", msg.ContentType)
		var got testMessage
		require.NoError(t, json.Unmarshal(msg.Body, &got))
		require.Equal(t, original.TimeStamp, got.TimeStamp)
		// Ensure the message was attempted DeliveryLimit+1 times (final attempt causes DLQ)
		require.Equal(t, int64(queueCfg.DeliveryLimit+1), handler.calls.Load())

		// Verify dead-letter exchange and routing key were used by broker
		// The message should retain the DLX used and be routed with our configured key
		require.Equal(t, "password.updated.dead", msg.RoutingKey)
	}

	require.NoError(t, mgr.Shutdown(testCtx(t)))
}

func SplitDomain(t *testing.T, host string) (prefix string, base string) {
	t.Helper()
	const baseDomain = "cozy.local"
	return strings.TrimSuffix(host, "."+baseDomain), baseDomain
}
