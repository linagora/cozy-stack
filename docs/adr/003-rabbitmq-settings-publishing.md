# ADR: Replace Common Settings HTTP API with RabbitMQ Publishing

## Status

**Proposed**

## Context

The cozy-stack currently synchronizes user settings with an external "common settings" application via HTTP API calls. 
This integration is implemented in `model/settings/common/common.go` and performs:

1. **HTTP POST** to `/api/admin/user/settings` when creating settings
2. **HTTP GET** to `/api/admin/user/settings/{nickname}` to check remote version
3. **HTTP PUT** to `/api/admin/user/settings/{nickname}` to update settings

This approach has several issues:

- **Synchronous blocking**: HTTP calls block the settings update operation
- **Version conflict complexity**: The code checks remote versions before updates, creating complex conflict resolution logic
- **Tight coupling**: Direct HTTP dependency on the common settings app
- **Failure propagation**: HTTP failures can affect the main settings update flow
- **No longer needed**: The common settings app is being deprecated, so the stack no longer needs to support conflict resolution

The codebase already has a mature RabbitMQ infrastructure for message consumption (password updates, user creation, phone updates, subscription changes, app lifecycle). 
This infrastructure can be extended for publishing.

## Proposal

Replace the HTTP API calls with RabbitMQ message publishing using a fire-and-forget pattern:

### Architecture

```mermaid
flowchart LR
    subgraph CozyStack["cozy-stack"]
        Publisher["Publisher"]
    end

    subgraph Storage["Storage"]
        CouchDB["CouchDB<br/>(settings)"]
    end

    subgraph MessageBroker["Message Broker"]
        RabbitMQ["RabbitMQ<br/>(exchange: settings)"]
    end

    subgraph Consumer["TWP app"]
        CommonSettings["Common Settings<br/>(consume)"]
    end

    Publisher -->|"1. Update settings"| CouchDB
    Publisher -->|"2. Publish message"| RabbitMQ
    RabbitMQ -->|"user.settings.updated"| CommonSettings
```

### Key Changes

1. **New Publisher Infrastructure** (`pkg/rabbitmq/publisher.go`)
   - Reuses existing `RabbitMQConnection` for connection management
   - Provides thread-safe publishing with automatic channel recovery
   - JSON serialization with persistent delivery mode

2. **Service Layer Extension** (`pkg/rabbitmq/service.go`)
   - Add publishing capabilities alongside existing consumers
   - Context-aware publisher selection (same as consumers)

3. **Simplified Common Settings Module** (`model/settings/common/common.go`)
   - Replace HTTP calls with RabbitMQ publishing
   - Remove version conflict checking (local DB is source of truth)
   - Fire-and-forget: log errors but don't fail the operation

### Configuration

```yaml
rabbitmq:
  enabled: true
  nodes:
    default:
      url: amqp://guest:guest@localhost:5672/
  publishing:
    common_settings:
      exchange: "settings"
      routing_key: "user.settings.updated"
```

### Message Format

Preserve existing message structure for backward compatibility:

```json
{
  "source": "cozy-stack",
  "nickname": "user-slug",
  "request_id": "domain_1234567890",
  "timestamp": 1234567890,
  "version": 2,
  "payload": {
    "language": "en",
    "timezone": "Europe/Paris",
    "first_name": "John",
    "last_name": "Doe",
    "display_name": "John Doe",
    "email": "john@example.com",
    "phone": "+33612345678",
    "matrix_id": "@john:example.com",
    "avatar": "https://cozy.example.com/public/avatar?v=2"
  }
}
```

## Alternatives

## Decision

## Consequences

