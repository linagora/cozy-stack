package rabbitmq_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/rabbitmq"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServiceImplem(t *testing.T) {
	assert.Implements(t, (*rabbitmq.Service)(nil), new(rabbitmq.RabbitMQService))
}

func TestRabbitMQService(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, "RabbitMQService")
	inst := setup.GetTestInstance()

	defaultNode := testutils.StartRabbitMQ(t, true, false)
	twakeNode := testutils.StartRabbitMQ(t, true, false)

	cfg := config.RabbitMQ{
		Enabled: true,
		Nodes: map[string]config.RabbitMQNode{
			"default": {
				Enabled: true,
				URL:     defaultNode.AMQPURL,
			},
			"linagora_default": {
				Enabled: false,
				URL:     "amqp://linagora:password@localhost",
			},
			"twake_default": {
				Enabled: true,
				URL:     twakeNode.AMQPURL,
			},
		},
		Exchanges: []config.RabbitExchange{
			{
				Name:            rabbitmq.ExchangeAuth,
				Kind:            "topic",
				Durable:         false,
				DeclareExchange: true,
				Queues: []config.RabbitQueue{
					{
						Name:     rabbitmq.QueueUserPasswordUpdated,
						Bindings: []string{rabbitmq.RoutingKeyUserPasswordUpdated},
						Declare:  true,
					},
				},
			},
		},
	}

	t.Run("NewServiceBuildsManagerForEachNode", func(t *testing.T) {
		t.Parallel()

		svc, err := rabbitmq.NewService(cfg)
		require.NoError(t, err)

		require.Len(t, svc.Managers, 2)
		assert.NotNil(t, svc.Managers["default"])
		assert.NotNil(t, svc.Managers["twake_default"])
	})

	t.Run("StartManagersStartsEachManager", func(t *testing.T) {
		t.Parallel()

		svc, err := rabbitmq.NewService(cfg)
		require.NoError(t, err)

		managers, err := svc.StartManagers()
		require.NoError(t, err)
		require.Len(t, managers, 2)

		err = managers[0].WaitReady(context.Background())
		assert.Nil(t, err, "RabbitMQ manager for context default failed to start")

		err = managers[1].WaitReady(context.Background())
		assert.Nil(t, err, "RabbitMQ manager for context twake_default failed to start")
	})

	t.Run("RabbitMQManagersReceiveMessages", func(t *testing.T) {
		// TODO: find a way to make sure each message is handled only by the
		// appropriate manager.
		t.Parallel()

		svc, err := rabbitmq.NewService(cfg)
		require.NoError(t, err)

		managers, err := svc.StartManagers()
		require.NoError(t, err)
		for _, manager := range managers {
			require.NoError(t, manager.WaitReady(testCtx(t)))
		}

		slug, domain := inst.SlugAndDomain()

		// 1. Update instance password via default RabbitMQ node
		defaultSender := newTestMessageSender(t, defaultNode.AMQPURL, rabbitmq.ExchangeAuth, rabbitmq.RoutingKeyUserPasswordUpdated)
		hashText, hashB64 := hashPassphrase(t)
		msg := rabbitmq.PasswordChangeMessage{
			TwakeID:       slug,
			Iterations:    100000,
			Hash:          hashB64,
			PublicKey:     "PUB",
			Key:           "KEY",
			Timestamp:     time.Now().Unix(),
			WorkplaceFqdn: slug + "." + domain,
		}
		defaultSender.publish(msg)

		// Wait until the instance hash is updated
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			return err == nil && string(updated.PassphraseHash) == hashText
		})

		// 2. Update instance password via twake RabbitMQ node
		twakeSender := newTestMessageSender(t, twakeNode.AMQPURL, rabbitmq.ExchangeAuth, rabbitmq.RoutingKeyUserPasswordUpdated)
		hashText, hashB64 = hashPassphrase(t)
		msg = rabbitmq.PasswordChangeMessage{
			TwakeID:       slug,
			Iterations:    100000,
			Hash:          hashB64,
			PublicKey:     "PUB",
			Key:           "KEY",
			Timestamp:     time.Now().Unix(),
			WorkplaceFqdn: slug + "." + domain,
		}
		twakeSender.publish(msg)

		// Wait until the instance hash is updated
		testutils.WaitForOrFail(t, 10*time.Second, func() bool {
			updated, err := lifecycle.GetInstance(inst.Domain)
			return err == nil && string(updated.PassphraseHash) == hashText
		})
	})

	t.Run("PublishRoutesMessage", func(t *testing.T) {
		t.Parallel()

		svc, err := rabbitmq.NewService(cfg)
		require.NoError(t, err)

		const (
			queueName   = "test.user.deletion.requested"
			contextName = "twake_default"
		)

		testutils.DeclareBoundQueue(t, twakeNode, rabbitmq.ExchangeAuth, queueName, rabbitmq.RoutingKeyUserDeletionRequested)

		err = svc.Publish(context.Background(), rabbitmq.PublishRequest{
			ContextName: contextName,
			Exchange:    rabbitmq.ExchangeAuth,
			RoutingKey:  rabbitmq.RoutingKeyUserDeletionRequested,
			MessageID:   "msg-1",
			Payload: rabbitmq.UserDeletionRequestedMessage{
				WorkplaceFqdn: "alice.twake.app",
				Reason:        "user_request",
				RequestedBy:   "cozy-stack",
				RequestedAt:   time.Now().UnixMilli(),
			},
		})
		require.NoError(t, err)

		msg, ok := testutils.GetOneFromQueue(t, twakeNode, queueName, 5*time.Second)
		require.True(t, ok, "expected a published message in %s", queueName)
		assert.Equal(t, "application/json", msg.ContentType)
		assert.Equal(t, "msg-1", msg.MessageId)

		var payload rabbitmq.UserDeletionRequestedMessage
		require.NoError(t, json.Unmarshal(msg.Body, &payload))
		assert.Equal(t, "alice.twake.app", payload.WorkplaceFqdn)
		assert.Equal(t, "user_request", payload.Reason)
		assert.Equal(t, "cozy-stack", payload.RequestedBy)
		assert.NotZero(t, payload.RequestedAt)
	})

	t.Run("PublishReturnsUnroutableWhenNoBinding", func(t *testing.T) {
		t.Parallel()

		svc, err := rabbitmq.NewService(cfg)
		require.NoError(t, err)

		conn, ch := testutils.CreateRabbitConnection(t, defaultNode)
		defer conn.Close()
		defer ch.Close()
		require.NoError(t, ch.ExchangeDeclare(rabbitmq.ExchangeAuth, "topic", false, false, false, false, nil))

		err = svc.Publish(context.Background(), rabbitmq.PublishRequest{
			ContextName: "default",
			Exchange:    rabbitmq.ExchangeAuth,
			RoutingKey:  rabbitmq.RoutingKeyUserDeletionRequested,
			Payload: rabbitmq.UserDeletionRequestedMessage{
				WorkplaceFqdn: "bob.twake.app",
				Reason:        "user_request",
				RequestedBy:   "cozy-stack",
				RequestedAt:   time.Now().UnixMilli(),
			},
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, rabbitmq.ErrPublishReturned)

		var returned *rabbitmq.PublishReturnedError
		require.True(t, errors.As(err, &returned))
		assert.Equal(t, rabbitmq.ExchangeAuth, returned.Exchange)
		assert.Equal(t, rabbitmq.RoutingKeyUserDeletionRequested, returned.RoutingKey)
	})
}
