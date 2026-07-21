## RabbitMQ

Integration with RabbitMQ: configuration, topology, and handler semantics.

### Overview

Cozy Stack can consume messages from RabbitMQ and dispatch them to Go handlers. The consumers are managed by background managers which:

- establish and monitor the AMQP connection with their node (with optional TLS),
- declare exchanges and queues (if configured),
- bind queues to routing keys,
- start per-queue consumers with QoS and redelivery limits,
- dispatch deliveries to queue-specific handlers.

### Configuration

RabbitMQ is configured in `cozy.yaml` under the `rabbitmq` key.

Key fields:

- `enabled`: Enable communication with RabbitMQ nodes.
- `nodes`: Map of RabbitMQ node configuration by context
  - `<context>`:
    - `url`: AMQP URL, e.g. `amqp://guest:guest@localhost:5672/`.
    - `tls`: Optional TLS settings (`root_ca`, `insecure_skip_validation`, `server_name`).
- `exchanges[]`: List of exchanges the stack should consume from.
  - `name`: Exchange name.
  - `kind`: Exchange type (e.g. `topic`).
  - `durable`: Whether the exchange is durable.
  - `declare_exchange`: If true, the exchange is declared on startup.
  - `dlx_name`, `dlq_name`: Optional defaults for queues under this exchange.
  - `queues[]`: List of queues to consume.
    - `name`: Queue name.
    - `bindings[]`: Routing keys to bind to the exchange.
    - `declare`: If true, declare the queue on startup.
    - `prefetch`: Per-consumer QoS prefetch.
    - `delivery_limit`: x-delivery-limit for quorum queues.
    - `declare_dlx`: If true, declare the Dead Letter Exchange (DLX) on startup.
    - `declare_dlq`: If true, declare the Dead Letter Queue (DLQ) on startup.
    - `dlx_name`, `dlq_name`: Optional overrides per queue.

Example:

```yaml
rabbitmq:
  enabled: true
  nodes:
    default:
      enabled: true
      url: amqp://guest:guest@localhost:5672/
      tls:
        # root_ca: /etc/ssl/certs/ca.pem
        insecure_skip_validation: false
        # server_name: rabbit.internal
  exchanges:
    - name: auth
      kind: topic
      durable: true
      declare_exchange: false
      queues:
        - name: stack.user.password.updated
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.auth.dlx
          dlq_name: stack.dead.letter.user.password.updated
          dl_routing_key: password.updated.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - user.password.updated
        - name: stack.user.created
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.auth.dlx
          dlq_name: stack.dead.letter.user.created
          dl_routing_key: user.created.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - user.created
        - name: stack.user.phone.updated
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.auth.dlx
          dlq_name: stack.dead.letter.user.phone.updated
          dl_routing_key: user.phone.updated.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - user.phone.updated
    - name: billing
      kind: topic
      durable: true
      declare_exchange: false
      queues:
        - name: stack.domain.subscription.changed
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.billing.dlx
          dlq_name: stack.dead.letter.domain.subscription.changed
          dl_routing_key: domain.subscription.changed.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - domain.subscription.changed
    - name: b2b
      kind: topic
      durable: true
      declare_exchange: false
      queues:
        - name: stack.user.lifecycle.queue
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.b2b.dlx
          dlq_name: stack.dead.letter.user.lifecycle
          dl_routing_key: domain.user.deleted.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - domain.user.deleted
        - name: stack.b2b.group.lifecycle
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.b2b.dlx
          dlq_name: stack.dead.letter.b2b.group.lifecycle
          dl_routing_key: b2b.group.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - b2b.group.created
            - b2b.group.updated
            - b2b.group.deleted
            - b2b.group.member.added
            - b2b.group.member.removed
        - name: stack.app.commands.queue
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.b2b.dlx
          dlq_name: stack.dead.letter.app.commands
          dl_routing_key: app.installation.requested.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - app.installation.requested
```

### Dead Letter Exchange (DLX) and Dead Letter Queue (DLQ)

The RabbitMQ integration supports Dead Letter Exchange (DLX) and Dead Letter Queue (DLQ) functionality for handling failed messages:

- **DLX (Dead Letter Exchange)**: An exchange where messages are sent when they cannot be delivered to their original destination.
- **DLQ (Dead Letter Queue)**: A queue bound to the DLX that receives failed messages for analysis or reprocessing.

#### Configuration

DLX and DLQ can be configured at both exchange and queue levels:

- **Exchange level**: Set `dlx_name` and `dlq_name` in the exchange configuration to provide defaults for all queues under that exchange.
- **Queue level**: Set `dlx_name` and `dlq_name` in the queue configuration to override exchange defaults.
- **Declaration control**: Use `declare_dlx` and `declare_dlq` flags to control whether the DLX and DLQ are automatically declared on startup.

#### Example with DLX/DLQ

```yaml
rabbitmq:
  enabled: true
  nodes:
    default:
      enabled: true
      url: amqp://guest:guest@localhost:5672/
    linagora_default:
      enabled: false
      url: amqp://guest:guest@localhost:5673/
  exchanges:
    - name: auth
      kind: topic
      durable: true
      declare_exchange: false
      queues:
        - name: stack.user.password.updated
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.auth.dlx
          dlq_name: stack.dead.letter.user.password.updated
          dl_routing_key: password.updated.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - user.password.updated
        - name: stack.user.created
          declare: true
          declare_dlx: true
          declare_dlq: true
          dlx_name: stack.auth.dlx
          dlq_name: stack.dead.letter.user.created
          dl_routing_key: user.created.dead
          prefetch: 8
          delivery_limit: 5
          bindings:
            - user.created
```

#### Behavior

- When `declare_dlx: true` and a `dlx_name` is provided, the DLX is declared as a fanout exchange on startup.
- When `declare_dlq: true` and a `dlq_name` is provided, the DLQ is declared and bound to the DLX on startup.
- If queue-level `dlx_name`/`dlq_name` are not specified, exchange-level defaults are used.
- Messages that exceed the `delivery_limit` or are rejected will be sent to the DLX and routed to the DLQ.

### Publishers

The Stack also publishes messages to RabbitMQ. Publishers do not declare any
queue or exchange on the Stack side: the exchange must already exist on the
broker, and a queue must be bound by the consuming service. Publishes use the
AMQP `mandatory` flag, so a publish to an exchange with no matching binding
fails with `PublishReturnedError` and the caller is expected to surface the
failure to the user.

#### `auth` exchange

Routing key: `user.deletion.requested`. Published from
`POST /settings/instance/deletion/force` when a user requests permanent
deletion of their account. The payload is the `UserDeletionRequestedMessage`
struct in `pkg/rabbitmq/contracts.go`.

#### `migration` exchange

Routing key: `nextcloud.migration.requested`. Published from
`POST /remote/nextcloud/migration` when a user starts a Nextcloud-to-Cozy
bulk migration. The payload is the `NextcloudMigrationRequestedMessage`
struct in `pkg/rabbitmq/contracts.go`:

```json
{
  "migrationId": "d4e5f6a7b8c94d0ea1b2c3d4e5f6a7b8",
  "workplaceFqdn": "alice.cozy.example.com",
  "accountId": "a1b2c3d4e5f6",
  "sourcePath": "/",
  "timestamp": 1712563200
}
```

Credentials are never in the payload: they live in the `io.cozy.accounts`
document referenced by `accountId`. The Stack populates `MessageID` with the
migration ID for cross-system tracing. The consuming service is responsible
for declaring its queue, binding it to this exchange, and processing the
messages; if no queue is bound when the Stack publishes, the user receives
`503` and the tracking document is marked `failed`.

### Handlers

Handlers implement a simple interface:

```go
type Handler interface {
    Handle(ctx context.Context, d amqp.Delivery) error
}
```

Returning `nil` acknowledges the message. Returning a non-nil error causes the message to be requeued (subject to broker policies and delivery limits).

Queue names are mapped to handlers in the stack. For example:

- `user.password.updated` → updates an instance passphrase when a `user.password.updated` routing key is received.
- `user.created` → validates and processes user creation events.
- `user.phone.updated` → updates the phone number stored in user settings.
- `domain.user.deleted` on the `b2b` exchange → removes externally managed organization contacts.

Message schemas are JSON and validated in the handler. Example payload for `user.password.updated`:

```json
{
  "twakeId": "string",
  "iterations": 100000,
  "hash": "base64",
  "publicKey": "base64",
  "privateKey": "cipherString",
  "key": "cipherString",
  "timestamp": 1726040000,
  "domain": "example.cozy.cloud"
}
```

Example payload for `user.created`:

```json
{
  "twakeId": "string",
  "mobile": "string",
  "internalEmail": "string",
  "iterations": 100000,
  "hash": "base64",
  "publicKey": "base64",
  "privateKey": "cipherString",
  "key": "cipherString",
  "timestamp": 1726040000,
  "workplaceFqdn": "alice.example.twake.app",
  "organizationId": "org-uuid-123",
  "organizationDomain": "example.com",
  "canUpgrade": false
}
```


Example payload for `b2b/domain.user.deleted`:

```json
{
  "emitter": "admin-panel",
  "type": "user.deleted",
  "workplaceFqdn": "alice.example.twake.app",
  "internalEmail": "alice@example.com",
  "reason": "user deleted",
  "organizationId": "org-uuid-123",
  "userId": "alice",
  "domain": "example.com"
}
```

The delete handler removes the single matching contact found by `internalEmail`
when `metadata.external` is true. 

Example payload for `b2b.group.created`:

```json
{
  "timestamp": "2026-06-16T10:30:00.000Z",
  "organizationId": "org123",
  "id": "engineering",
  "name": "engineering",
  "description": "Engineering team",
  "color": "#3366FF",
  "createdAt": "2026-06-16T10:30:00.000Z",
  "members": []
}
```

The Stack consumes `b2b.group.created`, `b2b.group.updated`,
`b2b.group.deleted`, `b2b.group.member.added`, and
`b2b.group.member.removed` from one lifecycle queue. Groups are replicated as
managed `io.cozy.contacts.groups` documents in every instance with the matching
`org_id`. Group membership is stored on managed external contacts via
`relationships.groups.data`; existing generated-ID external contacts are reused
by matching email or Cozy URL.

Example payload for `user.phone.updated`:

```json
{
  "twakeId": "string",
  "mobile": "+33123456789",
  "internalEmail": "string",
  "workplaceFqdn": "example.twake.app"
}
```

### Lifecycle

On startup, if `rabbitmq.enabled` is true:

1. The RabbitMQ service starts a manager for every context with configuration.
2. The managers create an AMQP connection with their node (using TLS if configured) and retry with exponential backoff.
3. They declare configured exchanges and queues (if `declare_*` flags are set).
4. They declare Dead Letter Exchanges and Dead Letter Queues (if `declare_dlx`/`declare_dlq` flags are set).
5. They bind queues to their routing keys and start consumers.
6. They expose a readiness channel internally so tests can wait until consumption is active.
7. They monitor the connection and restart consumers upon reconnection.

### Adding a new queue handler

Follow these steps to introduce a new queue and its handler.

1) Define the message schema and the handler

Create a handler type that implements the `Handle(ctx, d)` method and an accompanying message struct.

```go
// Example message payload consumed from the queue
type ExampleEvent struct {
    ID        string `json:"id"`
    Action    string `json:"action"`
    Timestamp int64  `json:"timestamp"`
}

// ExampleHandler processes ExampleEvent messages
type ExampleHandler struct{}

func NewExampleHandler() *ExampleHandler { return &ExampleHandler{} }

func (h *ExampleHandler) Handle(ctx context.Context, d amqp.Delivery) error {
    var msg ExampleEvent
    if err := json.Unmarshal(d.Body, &msg); err != nil {
        return fmt.Errorf("example: invalid payload: %w", err)
    }
    if msg.ID == "" {
        return fmt.Errorf("example: missing id")
    }
    // TODO: implement business logic
    return nil // ack
}
```

2) Register the handler for a queue name

Map the queue name to the handler so the manager knows which handler to use. This usually happens where queues are built from config.

```go
switch configQueue.Name {
case "example.queue":
    handler = NewExampleHandler()
}
```

3) Configure the exchange and queue in `cozy.yaml`

Add the queue under an exchange with the routing keys to bind.

```yaml
rabbitmq:
  enabled: true
  nodes:
    default:
      url: amqp://guest:guest@localhost:5672/
  exchanges:
    - name: auth
      kind: topic
      durable: true
      declare_exchange: true
      queues:
        - name: example.queue
          declare: true
          prefetch: 8
          delivery_limit: 5
          bindings:
            - example.created
            - example.updated
```

4) Publish a message (example)

```go
ch.PublishWithContext(ctx,
    "auth",            // exchange
    "example.created", // routing key
    false, false,
    amqp.Publishing{
        DeliveryMode: amqp.Persistent,
        ContentType:  "application/json",
        Body:         []byte(`{"id":"123","action":"created","timestamp":1726040000}`),
    },
)
```
