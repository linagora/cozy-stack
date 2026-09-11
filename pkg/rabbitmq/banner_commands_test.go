package rabbitmq_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

// TestBannerCommandsThroughTheBroker runs the queue as it is configured in
// cozy.example.yaml. It is the only place the two halves meet: a command no
// retry can fix has to end up in the dead letter queue rather than come back on
// the queue forever, and that is the broker's delivery limit doing it, not the
// handler.
func TestBannerCommandsThroughTheBroker(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped with --short")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)

	const (
		dlxName = "stack.platform.dlx"
		dlqName = "stack.dead.letter.banner.commands"
	)

	contextName := "banner-commands-broker-test"
	conf := config.GetConfig()
	previous := conf.Contexts
	conf.Contexts = map[string]interface{}{
		contextName: map[string]interface{}{
			"enable_banners":            true,
			"banner_command_categories": []interface{}{"billing"},
		},
	}
	t.Cleanup(func() { conf.Contexts = previous })

	domain := fmt.Sprintf("banner-broker-%d.example", time.Now().UnixNano())
	_, err := lifecycle.Create(&lifecycle.Options{
		Domain:      domain,
		Email:       "alice@example.org",
		ContextName: contextName,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lifecycle.Destroy(domain) })

	MQ := testutils.StartRabbitMQ(t, false, false)
	defer MQ.Stop(context.Background(), 30*time.Second)

	exchangeCfg := config.RabbitExchange{
		Name:            rabbitmq.ExchangePlatform,
		Kind:            "topic",
		Durable:         true,
		DeclareExchange: true,
		Queues: []config.RabbitQueue{{
			Name:          rabbitmq.QueueBannerCommands,
			Declare:       true,
			DeclareDLX:    true,
			DeclareDLQ:    true,
			DLXName:       dlxName,
			DLQName:       dlqName,
			DLRoutingKey:  "banner.commands.dead",
			Prefetch:      8,
			DeliveryLimit: 5,
			Bindings: []string{
				rabbitmq.RoutingKeyBannerMaterialize,
				rabbitmq.RoutingKeyBannerClear,
			},
		}},
	}

	specs := rabbitmq.BuildExchangeSpecs([]config.RabbitExchange{exchangeCfg})
	require.Len(t, specs, 1)
	require.Len(t, specs[0].Queues, 1, "the queue name must still map to a handler")

	connection, err := rabbitmq.BuildConnection(config.RabbitMQNode{Enabled: true, URL: MQ.AMQPURL})
	require.NoError(t, err)
	mgr, err := rabbitmq.NewRabbitMQManager(connection, specs).Start(testCtx(t))
	require.NoError(t, err)
	defer mgr.Shutdown(testCtx(t))
	require.NoError(t, mgr.WaitReady(testCtx(t)))

	_, ch := testutils.CreateRabbitConnection(t, MQ)
	defer ch.Close()

	publish := func(t *testing.T, key string, body []byte) {
		t.Helper()
		require.NoError(t, ch.PublishWithContext(testCtx(t), rabbitmq.ExchangePlatform, key, false, false,
			amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "application/json", Body: body}))
	}

	command := func(t *testing.T, name string, revision int64, edit func(map[string]interface{})) []byte {
		t.Helper()
		raw, err := os.ReadFile("../../model/banner/testdata/" + name + ".json")
		require.NoError(t, err)
		var payload map[string]interface{}
		require.NoError(t, json.Unmarshal(raw, &payload))
		payload["workplaceFqdn"] = domain
		payload["revision"] = revision
		if edit != nil {
			edit(payload)
		}
		body, err := json.Marshal(payload)
		require.NoError(t, err)
		return body
	}

	stored := func() *banner.Banner {
		inst, err := lifecycle.GetInstance(domain)
		if err != nil {
			return nil
		}
		doc, err := banner.Stored(inst, banner.CategoryBilling)
		if err != nil {
			return nil
		}
		return doc
	}

	t.Run("a materialize published on the platform exchange reaches the instance", func(t *testing.T) {
		publish(t, rabbitmq.RoutingKeyBannerMaterialize, command(t, "materialize", 1, nil))

		testutils.WaitForOrFail(t, 20*time.Second, func() bool { return stored() != nil })
		assert.Equal(t, "billing.grace.cycle-a.attempt-2", stored().BannerID)
	})

	t.Run("a clear removes it, and a redelivered older command does not bring it back", func(t *testing.T) {
		publish(t, rabbitmq.RoutingKeyBannerClear, command(t, "clear", 2, nil))
		testutils.WaitForOrFail(t, 20*time.Second, func() bool { return stored() == nil })

		publish(t, rabbitmq.RoutingKeyBannerMaterialize, command(t, "materialize", 1, nil))
		// The queue processes serially. A rejected marker proves the stale
		// command finished without overwriting the state we want to inspect.
		publish(t, "banner.materialize", []byte(`{"eventId":"stale-replay-barrier"}`))
		dead, ok := testutils.GetOneFromQueue(t, MQ, dlqName, 30*time.Second)
		require.True(t, ok)
		require.Contains(t, string(dead.Body), "stale-replay-barrier")
		assert.Nil(t, stored(), "a stale delivery must leave the category cleared")
	})

	t.Run("a malformed command reaches the dead letter queue", func(t *testing.T) {
		publish(t, rabbitmq.RoutingKeyBannerMaterialize, []byte(`{"category":"billing",`))

		dead, ok := testutils.GetOneFromQueue(t, MQ, dlqName, 30*time.Second)
		require.True(t, ok, "a payload no retry can fix must not be requeued forever")
		assert.Contains(t, string(dead.Body), `"category":"billing"`)
		assertDeadLettered(t, dead)
	})

	t.Run("a command for a category the context refuses reaches the dead letter queue", func(t *testing.T) {
		publish(t, rabbitmq.RoutingKeyBannerMaterialize, command(t, "materialize", 4, func(p map[string]interface{}) {
			p["category"] = "trial"
		}))

		dead, ok := testutils.GetOneFromQueue(t, MQ, dlqName, 30*time.Second)
		require.True(t, ok)
		assert.Contains(t, string(dead.Body), `"category":"trial"`)
		assertDeadLettered(t, dead)
	})
}

func assertDeadLettered(t *testing.T, dead *amqp.Delivery) {
	t.Helper()
	deaths, ok := dead.Headers["x-death"].([]interface{})
	require.True(t, ok, "the message must carry the broker's dead letter record")
	require.NotEmpty(t, deaths)
	death, ok := deaths[0].(amqp.Table)
	require.True(t, ok)
	// A quorum queue dead letters on its delivery limit, so the reason names
	// the limit rather than the single rejection a handler could ask for.
	assert.Equal(t, "delivery_limit", death["reason"])
}
