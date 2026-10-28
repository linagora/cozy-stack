[Table of contents](README.md#table-of-contents)

# Contacts

## Routes

### POST /contacts/myself

This endpoint returns the information about the io.cozy.contacts document for
the owner of this instance, the "myself" contact. If the document does not
exist, it is recreated with some basic fields.

A permission on the `io.cozy.contacts` document is required.

#### Request

```http
POST /contacts/myself HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.contacts",
    "id": "bf91cce0-ef48-0137-2638-543d7eb8149c",
    "attributes": {
      "fullname": "Alice",
      "email": [
        { "address": "alice@example.com", "primary": true }
      ]
    },
    "meta": {
      "rev": "1-6516671ec"
    }
  }
}
```

### GET /contacts/suggest

This endpoint suggests groups and people the user can share with, for the text
they are typing. The context's `centralized_org_directory` flag selects the
sources:

- When false or absent, it searches only the instance's local contacts and groups,
  including personal groups and replicated organization groups. It does not query
  the organization instance or the calendar side service, even if
  `common_contacts` is enabled. This also works without an organization.
- When true, it searches managed groups on the organization instance. For people,
  it asks the calendar side service's autocomplete API when `common_contacts` is
  enabled and `contacts_search` is configured (see `cozy.example.yaml`), then
  looks each result up by email on the organization instance, then locally.
  Without that service, or when the call fails or times out, it searches contact
  names and emails on the organization instance and locally. If no organization
  is available, only the local instance is used for database lookups.

Trashed contacts, people without an email, and the user are left out.

Read permissions on the whole `io.cozy.contacts` and `io.cozy.contacts.groups`
doctypes are required.

#### Query-String

| Parameter | Description                                         |
| --------- | --------------------------------------------------- |
| q         | the text to search, with at least 2 characters      |
| limit     | the maximal number of results (default 20, max 256) |

#### Request

```http
GET /contacts/suggest?q=dup HTTP/1.1
```

#### Response

Groups matching by name come first, followed by people in the order of the
search. The limit applies to the combined list after filtering people. In
centralized mode, instances without an organization return people only. A group
has type `io.cozy.contacts.groups`, its stable document `id`, and `name` and
`color` attributes. When a group has no color, `color` is an empty string. For
people, `meta.kind` is `member` when the document was found on the organization
instance, and `contact` when it was found on the instance. A person with no
matching document comes back without `id` and `meta`, with their name and email:
the client shares with them by email.

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": [
    {
      "type": "io.cozy.contacts.groups",
      "id": "b2b-group-1234567890abcdef1234567890abcdef",
      "attributes": { "name": "Dupont Team", "color": "#3366ff" }
    },
    {
      "type": "io.cozy.contacts",
      "id": "6f2a3c0e9b1d4e5f8a7b6c5d4e3f2a1b",
      "attributes": {
        "fullname": "Jean Dupont",
        "email": [{ "address": "jean.dupont@org.example", "primary": true }],
        "cozy": [{ "url": "https://jdupont.org.example", "primary": true }]
      },
      "meta": { "kind": "member" }
    },
    {
      "type": "io.cozy.contacts",
      "attributes": {
        "fullname": "Paul Dupuis",
        "email": [{ "address": "paul@friends.example", "primary": true }]
      }
    }
  ]
}
```
