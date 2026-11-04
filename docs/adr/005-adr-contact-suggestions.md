# ADR: Contact suggestions in the stack

## Status

Proposed

## Date

2026-09-14

## Context

Recipient suggestions in Drive have three problems:

1. They do not scale. The share modal [loads every contact and group](https://github.com/cozy/cozy-libs/blob/537656b7a2d55612500f7747d37707633d93dfa1/packages/cozy-sharing/src/components/ShareRecipientsInput.jsx#L35) of the instance each time it opens, [page by page](https://github.com/cozy/cozy-libs/blob/537656b7a2d55612500f7747d37707633d93dfa1/packages/cozy-sharing/src/queries/queries.js#L56), then [filters them in the browser](https://github.com/cozy/cozy-libs/blob/537656b7a2d55612500f7747d37707633d93dfa1/packages/cozy-sharing/src/components/ShareAutosuggest.jsx#L43). It only works because every member is copied to every instance, which is what the [storage ADR](006-adr-organization-contacts-storage.md) stops doing. Cached or not, an instance holding thousands of member documents so the browser can filter them is the problem.
2. They differ from the other apps. Mail and Calendar search their own index, Drive filters its local copy, so a colleague found in Mail can be missing in Drive.
3. There is no contact search route and no index on contact names, so the stack cannot answer instead of the client.

The [common contacts ADR](https://github.com/linagora/twake-workplace-private/pull/1608) already syncs contacts to every app through the `twake:contacts:common` exchange. [ADR 058](https://github.com/linagora/twake-workplace-private/pull/1640) puts an autocomplete API in the calendar side service, shared by every app. This ADR describes how the stack uses that API. Where members and contacts are stored is covered by the [storage ADR](006-adr-organization-contacts-storage.md).

## Decision

- The stack adds a `GET /contacts/suggest` route, called by the share modal as the user types.
- With `common_contacts` enabled and `contacts_search` configured, the route calls the autocomplete API of the calendar side service, asking only for people with an email, and maps each result to a contact document by email.
- Without that service, the route searches the user's instance directly, plus the org instance when `centralized_org_directory` is enabled.
- The route suggests groups before people, searching group names in those same databases and returning member counts. Groups remain the stack's own documents; organization groups are built from `b2b.group.*`.

```mermaid
sequenceDiagram
  participant M as share modal
  participant S as cozy-stack
  participant C as calendar side service
  participant I as org or user instance

  M->>S: GET /contacts/suggest?q=dup
  S->>I: groups by name and member counts
  I-->>S: groups
  S->>C: search "dup" for the user, with an email
  C-->>S: names and emails
  S->>I: contacts by email
  I-->>S: contacts
  S-->>M: groups, then people
```

### The autocomplete API

The calendar side service exposes the autocomplete on a dedicated port, for backends only ([#1106](https://github.com/linagora/twake-calendar-side-service/issues/1106), [#1107](https://github.com/linagora/twake-calendar-side-service/pull/1107)). The stack sends:

```http
POST /api/people/search
Authorization: Bearer <secret>

{
  "user": "jean.dupont@org.tld",
  "q": "dup",
  "objectTypes": ["contact"],
  "hasFields": ["emailAddresses"],
  "limit": 20,
  "offset": 0
}
```

- `user` is the logged in user, by mail address. The bearer identifies the stack, not the person, so the stack names them here. It sends the `email` of the instance.
- `hasFields` leaves out the people without an email, before the service cuts to the top N. The stack cannot do that itself: of 30 results with 20 of them without an email, it would show 10 suggestions and never fetch the next matches.
- `matrixIds` and `phoneNumbers` come with [#1108](https://github.com/linagora/twake-calendar-side-service/issues/1108). The stack reads `emailAddresses` only.
- `limit` is between 1 and 256, and `offset` plus `limit` is at most 1000. The route's 20 fits.

A result carries an `id`, `names` and `emailAddresses`, in display name order, and no CardDAV path.

The search covers the organization address book, so a colleague comes up even when nobody has them in a personal address book.

Each context configures the endpoint and the shared secret:

```yaml
contexts:
  my-context:
    common_contacts: true
    centralized_org_directory: true
    contacts_search:
      url: https://calendar-side-service.example.com:81/api/people/search
      token: abcdef
```

A context without `contacts_search` or with `common_contacts` disabled uses the database search. Calendar search does not require `centralized_org_directory`.

So the stack looks each result up by email, with the `contacts-by-email` view, on the org instance first when `centralized_org_directory` is enabled, then on the user's instance. Where it is found gives its kind: a member on the org instance, a personal contact on the user's instance.

A contact can have no email, and someone without one cannot be invited. `hasFields` keeps them out, and the standalone search applies the same rule. The share modal already hides them today, it only lists contacts with an email or a cozy URL.

### The route

`GET /contacts/suggest?q=<text>&limit=<n>` on the user's instance, with whole-doctype `GET` on `io.cozy.contacts` and `io.cozy.contacts.groups`. `q` needs at least 2 characters after trimming and `limit` defaults to 20 (maximum 256), shared between groups and people. The route does not paginate.

```json
{
  "data": [
    {
      "type": "io.cozy.contacts.groups",
      "id": "b2b-group-1234567890abcdef1234567890abcdef",
      "attributes": { "name": "Dupont Team", "color": "#3366ff", "member_count": 12 }
    },
    {
      "type": "io.cozy.contacts",
      "id": "6f2a",
      "attributes": {
        "fullname": "Jean Dupont",
        "email": [{ "address": "jean.dupont@org.tld", "primary": true }],
        "cozy": [{ "url": "https://jdupont.org.tld", "primary": true }]
      },
      "meta": { "kind": "org_member" }
    }
  ]
}
```

- Groups come first, followed by people in the order of the search.
- People use the `io.cozy.contacts` field names, so cozy-sharing can render them as today. Groups have a stable ID, name, color and `member_count` (non-trashed contacts, including those without an email), without member documents.
- For people, `meta.kind` is `org_member` or `contact`, depending on where the document was found.
- A person with no matching document comes back without an `id` or `kind`, with the name and email from the search. The client shares it by email.

### Standalone mode

Without the calendar side service, the stack runs a case-insensitive `$regex` Mango query on names and emails, on the user's instance and, in centralized mode, the org instance.

No existing index helps: `contacts-by-email` is an exact lookup, `by-groups` and `by-me` answer other reads, and a `$regex` cannot use an index anyway. So it stays a scan, which is fine on a standalone instance with few contacts.

## What changes for the clients

- With `contactsSuggestEnabled` and a compatible cozy-client, the online picker stops loading full contact and group lists and calls `GET /contacts/suggest` as the user types (300 ms debounce, 2 characters minimum). Group counts come from the API. API groups are suggested only when `sharing.show-recipient-groups` is enabled.
- cozy-sharing sends local contacts and groups by ID, and organization members or people without a local contact as `{email}`. Today, when the typed email matches nothing, it [creates the contact itself](https://github.com/cozy/cozy-libs/blob/537656b7a2d55612500f7747d37707633d93dfa1/packages/cozy-sharing/src/helpers/contacts.js#L33) so the recipient has a document to point at. In API mode, it stops, and the stack does it instead: it creates the contact, shares, then publishes the address asynchronously on `twake:contacts:collected` when `common_contacts` is enabled. It updates that document when Sabre sends it back ([storage ADR](006-adr-organization-contacts-storage.md)). Nothing waits on the calendar side service.
- Offline, with older clients, or with `contactsSuggestEnabled` disabled, the local picker remains available using locally available data. It expands groups into members when `sharing.show-recipient-groups` is disabled.

## What to do when things fail

- Calendar side service down or slow: on failure or after a 1-second timeout, the route falls back to the database search and logs it.
- Org instance unreachable: members should come back without an `id` and be shared by email. Currently, errors reading the org contacts can still fail suggestions or sharing; this fallback is incomplete.

## Consequences

- In online API mode, opening the share modal no longer downloads the organization.
- Drive uses the same people search as the other apps when the calendar side service is enabled.
- If the calendar side service is unavailable, suggestions use the database fallback.

## Open questions

- Is the current 1-second timeout appropriate for the expected latency? @chibenwa
