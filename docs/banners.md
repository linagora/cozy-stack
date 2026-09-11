## Banners

A banner is a platform message displayed to the user by their applications: a
quota warning, a payment problem, a trial about to end. The stack stores one
`io.cozy.banners` document per category and the clients render whatever they
find. There is no computation behind a read: the rules run when an input
changes and the result is written to the instance database.

Banners are off unless the instance context enables them:

```yaml
contexts:
  b2b_twake_default:
    enable_banners: true
```

Turning the switch back off stops the writes and leaves the documents already
materialized in place, so a rollback needs a cleanup too.

### Producers

Two kinds of producer write the same documents through the same code:

- **In-process rules**, for what the stack owns. Disk usage is the only one
  today (`model/banner/quota.go`): the stack measures it, so the stack decides,
  and the wording comes from its own locale catalogs.
- **A backend on the bus**, for what the stack cannot verify. A payment status,
  a dunning step, a trial conversion are decisions another service already
  made, so they arrive as commands carrying their own wording. The stack
  validates, targets, orders, localizes and stores them; it decides nothing
  about what they say.

A producer never addresses a document. It names a category, and the stack does
the rest. `quota` is reserved to the in-process rules and a command naming it
is rejected.

### The command contract

Commands are consumed from the `stack.banner.commands` queue with two routing
keys. See [the RabbitMQ documentation](rabbitmq.md) for the queue declaration.
The fixtures in `model/banner/testdata` are the shared examples the publisher
is developed against.

**`banner.materialize`** puts a banner in a category, replacing whatever that
category holds:

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
  "priority": 150,
  "dismissible": true,
  "title": { "en": "Payment failed", "fr": "Échec du paiement" },
  "text": { "en": "We could not charge your card.", "fr": "Nous n'avons pas pu débiter votre carte." },
  "cta": {
    "label": { "en": "Update payment method", "fr": "Mettre à jour le moyen de paiement" },
    "url": "https://manager.example.org/linagora/twake_prod/premium"
  },
  "secondaryCta": {
    "label": { "en": "Contact support", "fr": "Contacter le support" },
    "url": "https://twake.app/support"
  },
  "startsAt": "2026-08-01T00:00:00Z",
  "endsAt": "2026-08-05T23:30:00Z"
}
```

**`banner.clear`** empties a category. It carries the addressing and ordering
fields only. Nonempty presentation fields are rejected:

```json
{
  "workplaceFqdn": "alice.twake.app",
  "eventId": "banner-command-43",
  "revision": 43,
  "timestamp": 1788944400,
  "category": "billing"
}
```

| Field | Required | Notes |
| --- | --- | --- |
| `category` | always | The slot to write. One document per category per instance. `quota` is refused. |
| `workplaceFqdn` | one of the two | A single instance. |
| `domain` | one of the two | A B2B organization: every instance under it gets the banner. |
| `revision` | always | A positive counter the backend increments per target and category. It is what orders commands. |
| `timestamp` | always | Positive epoch seconds representable in RFC3339, when the backend decided. Provenance, stamped on the document; it orders nothing. |
| `eventId` | no | The backend's correlation id, at most 256 bytes. Logged and retained, never a second ordering mechanism. |
| `bannerId` | materialize | Identifies the occurrence: a new one clears a dismissal, the same one keeps it. |
| `severity` | materialize | `info`, `warning` or `error`. |
| `surface` | materialize | `banner` or `modal`. |
| `text` | materialize | A map keyed by locale, complete in `en`. |
| `title` | no | Same shape as `text`. A client with no title names the dialog from the text. |
| `cta`, `secondaryCta` | no | `url` must be an absolute `https` URL. A secondary action needs a primary one. |
| `dismissible` | no | Defaults to false. A modal with neither a call to action nor a dismissal is made dismissible. |
| `priority` | no | 0 to 1000. The stack's own quota banners sit at 50 and 100. |
| `startsAt`, `endsAt` | no | RFC 3339. `startsAt` defaults to the decision time. |

The document also carries `source.trigger`, which is `banner.command` for
everything that arrives this way, and `cozyMetadata.createdByApp`, which stays
`stack` whoever asked: a client cannot be made to reason about a per-producer
author. `_id`, `_rev`, `dismissedAt` and `cozyMetadata` are not fields of the
command and a payload carrying them is ignored, not honored.

### Localization

`text`, `title` and every label are rendered by the backend, not by the stack.
The stack picks **one** locale for the whole banner: the instance's, if every
string the document needs exists in it, and `en` otherwise. `lang` names the
language the user actually reads. Falling back field by field would put a
French sentence above an English button.

The stack keeps every locale the command carried, on the private
`io.cozy.banners.commands` document, so changing an instance's language picks
one again without the backend publishing anything. The revision, the wording
and the decision time are unchanged: only the language moves. Re-localizing
rewrites a banner rather than restoring one, so a category whose document is
gone stays gone until the next command. A record written before the stack
retained the wording has nothing to pick from, and stays as it is too.

The languages available for a commanded banner are the ones the backend sends,
not the stack's `consts.SupportedLocales`: the stack renders nothing here, so
its own catalogs have no say. Those catalogs still decide the languages of what
the stack does write itself, the quota banners, and shipping a `.po` file is
not what enables one.

### Validation

The command is rejected, never repaired. An authorized backend can put
arbitrary text in front of a user, so anything unexpected in a payload is a
backend bug worth surfacing rather than something to guess at. A rejected
command fails the delivery, so the broker redelivers it up to the queue's
`delivery_limit` and then dead letters it.

- `category` matches `^[a-z][a-z0-9-]{0,31}$` and is not `quota`.
- exactly one of `domain` and `workplaceFqdn`, each a plain host name.
- `revision` and `timestamp` are above zero; the timestamp must serialize as an
  RFC3339 time (milliseconds sent as seconds are rejected).
- `bannerId` matches `^[a-z0-9.-]{1,64}$`.
- `severity` is one of `info`, `warning`, `error`.
- `surface` is one of `banner`, `modal`.
- `priority` is between 0 and 1000; the effective start (`startsAt`, or
  `timestamp` when omitted) is before `endsAt`. Window values must be
  representable in RFC3339.
- `text`, `title` and every label are present in the `en` fallback locale.
- a call to action has an absolute `https` URL, and a secondary one has a
  primary alongside it.
- lengths, in bytes, per locale: 256 for a title, 1024 for a text, 128 for a
  label, 2048 for a URL, 256 for `eventId`; at most 32 locales per map,
  with locale keys of 1–35 bytes. The JSON command is limited to 256 KiB,
  including whitespace and unknown fields at the transport boundary.
- clear commands reject nonempty presentation fields, including wording and
  windows; their addressing, timestamp and correlation fields are still validated.

An instance whose context has no `enable_banners` is a no-op rather than a
rejection: the backend knows its customers, not which of them display banners.
A workplace that is not here is retried rather than rejected, because the stack
cannot tell a deleted instance from one still being provisioned; the queue's
delivery limit is what bounds those retries.

### Authorization

The queue is the authority on who publishes: broker credentials, permissions
and bindings, not a field of the payload. The context settings say what that
publisher is allowed to say:

```yaml
contexts:
  b2b_twake_default:
    enable_banners: true
    banner_command_categories:
      - billing
      - trial
```

**One category, one owner.** Two producers writing the same category means last
writer wins by counters that were never comparable. Scopes that can be active
at the same time need separate categories.

A command for a category an enabled member context does not list is rejected
**before any member banner or revision record is written**. Authorization is
checked over the full resolved recipient list first. Repair the configuration
and explicitly replay a rejected command once it has been dead lettered.
Storage failures during fan-out are retried by the broker, and a replay of the
same revision finishes the members that were not reached, because nothing is
recorded for a member whose banner was not written.

### Ordering and retries

Bus delivery is at-least-once and unordered, so ordering cannot come from the
arrival time, and it cannot come from the visible document either: a clear
leaves none behind and an unchanged decision writes none. The stack keeps the
last accepted command per instance and category in `io.cozy.banners.commands`,
a separate doctype blocked from public reads and writes, including wildcard
application grants and the bulk/replication API, so an application cannot
rewrite the ordering record. It is a normal document, so it is included in the
instance's backups and migrations.

Under the instance's banner lock:

1. A command whose revision is not above the recorded one is ignored. That
   covers a redelivery and a stale command alike, including a revision the
   backend reused with different wording, which is a backend bug the stack
   cannot repair.
2. Otherwise the banner is written first and the record second. A process that
   dies between the two leaves the next delivery of that revision to do both
   again, and materialization is idempotent, so it heals itself. The reverse
   order would record a decision the user never saw.

This is what makes a clear survive a redelivered materialize, an unchanged
decision advance the ordering, and a partial organization fan-out finish on the
retry. A retry reuses the revision, correlation id and payload of the original;
only a changed decision needs a new revision. The backend allocates them, and
must serialize its own state refresh so a newer revision never carries an older
snapshot.

Re-publishing an unchanged organization command at its existing revision is how
a member provisioned after the fact is reached: the replay resolves membership
again and leaves the members it already reached untouched.

### Dismissals and occurrences

Re-materializing the same `bannerId` keeps a dismissal the user recorded, and
keeps the moment the occurrence began rather than the last evaluation. A new
`bannerId` is a message the user has not seen, so it clears the dismissal.
Escalating a dunning cycle, or starting a new one, is a new occurrence; changing
the wording of the current one is not.

### What the stack does not do

Nothing replays on its own: enabling `enable_banners` on a context materializes
nothing until the next command, and a backend that needs its banners to appear
has to publish them again. The stack sends no acknowledgement back: a broker
confirm means the broker accepted the message, not that any instance displays
it.
