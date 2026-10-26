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

This endpoint suggests the people the user can share with, for the text they
are typing. It asks the autocomplete API of the calendar side service when the
context enables `common_contacts` and configures `contacts_search` (see
`cozy.example.yaml`), and looks each result up by email on the organization
instance, then on the instance. Without `common_contacts` or `contacts_search`,
or when the call fails or times out, it searches the names and emails of the
contacts of both instances instead. Trashed contacts, people without an email,
and the user, are left out. Local lookup is available in either storage mode,
including when both feature flags are disabled.

A permission on the whole `io.cozy.contacts` doctype is required.

#### Query-String

| Parameter | Description                                         |
| --------- | --------------------------------------------------- |
| q         | the text to search, with at least 3 characters      |
| limit     | the maximal number of results (default 20, max 256) |

#### Request

```http
GET /contacts/suggest?q=dup HTTP/1.1
```

#### Response

The results keep the order of the search. `meta.kind` is `member` when the
document was found on the organization instance, and `contact` when it was
found on the instance. A person with no matching document comes back without
`id` and `meta`, with their name and email: the client shares with them by
email.

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": [
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
