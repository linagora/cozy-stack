## Banners

This is the publisher reference for backend banner commands. The stack stores
one `io.cozy.banners` document per category and instance. See
[ADR 054](https://github.com/linagora/twake-workplace-private/blob/main/documentation/docs/adrs/adr-054.md)
for the platform design.

### Configuration

Enable banners and allow the publisher's categories in each recipient context:

```yaml
contexts:
  b2b_twake_default:
    enable_banners: true
    banner_command_categories:
      - billing
      - trial
```

Broker credentials, permissions and bindings control who can publish. Each
category must have one owner; overlapping audiences with independent revision
counters need separate categories. `quota` is reserved for the stack's rules.

Disabled instances are skipped. If any enabled recipient disallows the category,
the whole command is rejected before any writes. Disabling banners stops writes
but does not remove existing documents.

### Commands

Publish JSON on the `platform` exchange, consumed by `stack.banner.commands`.
The routing key selects the operation:

- `banner.materialize`: create or replace the banner in a category.
- `banner.clear`: delete the banner in a category; nonempty presentation fields
  are rejected.

See [RabbitMQ configuration](rabbitmq.md#configuration) for queue declarations
and [shared fixtures](../model/banner/testdata) for complete examples.

`banner.materialize`:

```json
{
  "workplaceFqdn": "alice.twake.app",
  "eventId": "banner-command-42",
  "revision": 42,
  "timestamp": 1788944400,
  "category": "billing",
  "bannerId": "billing.grace.cycle-a.attempt-2",
  "severity": "warning",
  "surface": "banner",
  "dismissible": true,
  "text": { "en": "We could not charge your card.", "fr": "Nous n'avons pas pu débiter votre carte." },
  "cta": {
    "label": { "en": "Update payment method", "fr": "Mettre à jour le moyen de paiement" },
    "url": "https://manager.example.org/linagora/twake_prod/premium"
  }
}
```

`banner.clear`:

```json
{
  "workplaceFqdn": "alice.twake.app",
  "eventId": "banner-command-43",
  "revision": 43,
  "timestamp": 1788944400,
  "category": "billing"
}
```

| Field | Required | Contract |
| --- | --- | --- |
| `category` | always | Matches `^[a-z][a-z0-9-]{0,31}$`; `quota` is rejected. |
| `workplaceFqdn` / `domain` | exactly one | Plain host name: a single instance / a B2B organization whose members receive the command. |
| `revision` | always | Positive counter, increasing per target and category. |
| `timestamp` | always | Decision time in positive epoch seconds, within the RFC3339 range. Does not order commands. |
| `eventId` | no | Correlation ID, at most 256 bytes. |
| `bannerId` | materialize | Matches `^[a-z0-9.-]{1,64}$`. Keep it for the same occurrence to preserve dismissal; change it for a new occurrence. |
| `severity` | materialize | `info`, `warning` or `error`. |
| `surface` | materialize | `banner` or `modal`. |
| `text` | materialize | Locale map with nonempty `en`; at most 1024 bytes per locale. |
| `title` | no | Locale map with nonempty `en` when supplied; at most 256 bytes per locale. |
| `cta`, `secondaryCta` | no | Each has a locale-map `label` (nonempty `en`, at most 128 bytes per locale) and an absolute `https` `url` (at most 2048 bytes). A secondary CTA requires a primary one. |
| `dismissible` | no | Defaults to false. A modal without a CTA is made dismissible. |
| `priority` | no | 0–1000; defaults to 0. Quota banners use 50 and 100. |
| `startsAt`, `endsAt` | no | RFC3339. If both are supplied, `startsAt` must precede `endsAt`. An explicit start replaces the stored start; omission preserves it for the same occurrence when compatible with the end, otherwise defaults to the command's decision time. |

Each locale map accepts at most 32 locales with keys of 1–35 bytes. The JSON
body is limited to 256 KiB, including whitespace and unknown fields.
`_id`, `_rev`, `dismissedAt` and `cozyMetadata` are not command fields and are
ignored if supplied.

### Localization

The publisher supplies all wording. The stack selects the instance's locale
only if it is complete for every supplied text and label; otherwise the whole
banner falls back to `en`. The stored `lang` identifies the selected language.
Any complete publisher-supplied locale is supported, independently of the
stack's translation catalogs.

On an instance language change, existing banners are re-localized from retained
commands without republishing. Deleted banners and older records without
retained wording are left unchanged.

### Revisions and recovery

- Revisions at or below the last accepted revision for an instance and category
  are ignored, even after a clear. Only a changed decision needs a new revision;
  the publisher must ensure newer revisions carry newer state.
- Retry with the original revision, event ID and payload. Replays complete
  partial organization deliveries and reach newly provisioned members while
  leaving recipients that already accepted the revision unchanged.
- Enabling banners does not bootstrap them: the publisher must republish.
- Invalid commands and missing workplaces fail delivery. The broker requeues
  failures without a delay until its configured delivery limit is exhausted;
  configure dead lettering as described in [RabbitMQ](rabbitmq.md#dead-letter-exchange-dlx-and-dead-letter-queue-dlq).
  Fix the cause and explicitly replay dead-lettered commands with their original
  operation routing key (`banner.materialize` or `banner.clear`).
- The stack sends no application acknowledgement. A broker confirm means the
  broker accepted the message, not that a banner was stored or displayed.
