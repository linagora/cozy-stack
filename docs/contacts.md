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
they are typing. It searches these sources:

- Groups come from the instance's database. When the context's
  `centralized_org_directory` flag is true, the managed groups of the
  organization instance come first, and local copies of them are skipped.
- People come from the calendar side service's autocomplete API when
  `common_contacts` is enabled and `contacts_search` is configured (see
  `cozy.example.yaml`). Each result is looked up by email on the organization
  instance in centralized mode, then locally.
- Without that service, or when the call fails or times out, people are searched
  by name and email in the same databases as groups: the organization instance
  in centralized mode, then the local instance. This also works without an
  organization.

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
search. The limit applies to the combined list after filtering people. A group
has type `io.cozy.contacts.groups`, its stable document `id`, and `name` and
`color` attributes, plus `member_count` (the number of non-trashed contacts in that
group, including contacts without an email). Counts come from the same local or
organization database as the group, and empty groups return zero. The API does
not return member documents. When a group has no color, `color` is an empty string. For
people, `meta.kind` is `org_member` when the document was found on the organization
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
      "attributes": { "name": "Dupont Team", "color": "#3366ff", "member_count": 12 }
    },
    {
      "type": "io.cozy.contacts",
      "id": "6f2a3c0e9b1d4e5f8a7b6c5d4e3f2a1b",
      "attributes": {
        "fullname": "Jean Dupont",
        "email": [{ "address": "jean.dupont@org.example", "primary": true }],
        "cozy": [{ "url": "https://jdupont.org.example", "primary": true }]
      },
      "meta": { "kind": "org_member" }
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
