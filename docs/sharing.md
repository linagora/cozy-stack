[Table of contents](README.md#table-of-contents)

# Sharing

The owner of a cozy instance can share access to her documents to other users.

## Sharing by links

A client-side application can propose sharing by links:

1. The application must have a public route in its manifest. See
   [Apps documentation](apps.md#routes) for how to do that.
2. The application can create a set of permissions for the shared documents,
   with codes. Another code called `shortcode` is also generated and associated
   with the code. This `shortcode` is much smaller (but secure), which is
   easier to be shared. See [permissions documentation](permissions.md) for the
   details.
3. The application can then create a shareable link (e.g.
   `https://calendar.cozy.example.net/public?sharecode=eiJ3iepoaihohz1Y`) by
   putting together the app sub-domain, the public route path, and a code for
   the permissions set.
4. The app can then send this link by mail, via the [jobs system](jobs.md), or
   just give it to the user, so he can transmit it to her friends via chat or
   other ways.

When someone opens the shared link, the stack will load the public route, find
the corresponding `index.html` file, and replace `{{.Token}}` inside it by a
token with the same set of permissions that `sharecode` offers. This token can
then be used as a `Bearer` token in the `Authorization` header for requests to
the stack (or via cozy-client-js).

NB: The `shortcode` can also be used as the `Bearer`. In this case, the
corresponding `sharecode` will be matched on server-side

If necessary, the application can list the permissions for the token by calling
`/permissions/self` with this token.

## Cozy to cozy sharing

The owner of a cozy instance can send and synchronize documents to others cozy
users.

### Intents

When a sharing is authorized, the user is redirected to their cozy on the
application that was used for the sharing (when possible). It's possible to use
a specific route to do so, via the intents. The application must declare an
intent in its manifest for the action `SHARING`. The doctype of the intent must
be the same as the doctype of the first rule of the sharing. In the redirect
URL, the query string will have a `sharing` parameter with the sharing ID (but
no intent parameter).

### Routes

#### POST /sharings/

Create a new sharing. The sharing rules and recipients must be specified. The
`description`, `preview_path`, and `open_sharing` fields are optional. The
`app_slug` field is optional and is the slug of the web app by default.

[See the doc on io.cozy.sharings for in-depth explanation of all attributes](https://docs.cozy.io/en/cozy-doctypes/docs/io.cozy.sharings/).

To create a sharing, no permissions on `io.cozy.sharings` are needed: an
application can create a sharing on the documents for whose it has a permission.

##### Request

```http
POST /sharings/ HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "attributes": {
      "description": "sharing test",
      "preview_path": "/preview-sharing",
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    },
    "relationships": {
      "recipients": {
        "data": [
          {
            "id": "2a31ce0128b5f89e40fd90da3f014087",
            "type": "io.cozy.contacts"
          },
          {
            "id": "51bbc980acb0013cb5f618c04daba326",
            "type": "io.cozy.contacts.groups"
          }
        ]
      }
    }
  }
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "meta": {
      "rev": "1-4859c6c755143adf0838d225c5e97882"
    },
    "attributes": {
      "description": "sharing test",
      "preview_path": "/preview-sharing",
      "app_slug": "drive",
      "owner": true,
      "created_at": "2018-01-04T12:35:08Z",
      "updated_at": "2018-01-04T13:45:43Z",
      "members": [
        {
          "status": "owner",
          "public_name": "Alice",
          "email": "alice@example.net",
          "instance": "alice.example.net"
        },
        {
          "status": "mail-not-sent",
          "name": "Bob",
          "email": "bob@example.net"
        },
        {
          "status": "mail-not-sent",
          "name": "Gaby",
          "email": "gaby@example.net",
          "only_in_groups": true,
          "groups": [0]
        }
      ],
      "groups": [
        {
          "id": "51bbc980acb0013cb5f618c04daba326",
          "name": "G. people",
          "color": "#22AA55",
          "addedBy": 0
        }
      ],
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    },
    "links": {
      "self": "/sharings/ce8835a061d0ef68947afe69a0046722"
    }
  }
}
```

### GET /sharings/:sharing-id/discovery

If no preview_path is set, it's an URL to this route that will be sent to the
users to notify them that someone wants to share something with them. On this
page, they can fill the URL of their Cozy (if the user has already filled its
Cozy URL in a previous sharing, the form will be pre-filled and the user will
just have to click OK).

#### Query-String

| Parameter | Description                                                                               |
| --------- | ----------------------------------------------------------------------------------------- |
| state     | a code that identify the recipient                                                        |
| shortcut  | true means that accepting the sharing will just create a shortcut on the recipient's Cozy |

#### Example

```http
GET /sharings/ce8835a061d0ef68947afe69a0046722/discovery?state=eiJ3iepoaihohz1Y&shortcut=true HTTP/1.1
Host: alice.example.net
```

### POST /sharings/:sharing-id/discovery

Give to the cozy of the sharer the URL of the Cozy of one recipient. The sharer
will register its-self as an OAuth client on the recipient cozy, and then will
ask the recipient to accept the permissions on its instance.

This route exists in two versions, the version is selected by the HTTP header
`Accept`

#### Classical (`x-www-form-urlencoded`)

| Parameter | Description                                                                               |
| --------- | ----------------------------------------------------------------------------------------- |
| state     | a code that identify the recipient                                                        |
| url       | the URL of the Cozy for the recipient                                                     |
| shortcut  | true means that accepting the sharing will just create a shortcut on the recipient's Cozy |

##### Example

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/discovery HTTP/1.1
Host: alice.example.org
Content-Type: application/x-www-form-urlencoded
Accept: text/html

state=eiJ3iepoaihohz1Y&url=https://bob.example.net/
```

```http
HTTP/1.1 302 Moved Temporarily
Location: https://bob.example.net/auth/sharing?...
```

#### JSON

This version can be more convenient for applications that implement the preview
page. To do that, an application must give a `preview_path` when creating the
sharing. This path must be a public route of this application. The recipients
will receive a link to the application subdomain, on this page, and with a
`sharecode` in the query string (like for a share by link).

To know the `sharing-id`, it's possible to ask `GET /permissions/self`, with the
`sharecode` in the `Authorization` header (it's a JWT token). In the response,
the `source_id` field will be `io.cozy.sharings/<sharing-id>`.

##### Parameters

| Parameter | Description                           |
| --------- | ------------------------------------- |
| sharecode | a code that identify the recipient    |
| url       | the URL of the Cozy for the recipient |

##### Example

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/discovery HTTP/1.1
Host: alice.example.org
Content-Type: application/x-www-form-urlencoded
Accept: application/json

sharecode=eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJhcHAiLCJpYXQiOjE1MjAzNDM4NTc&url=https://bob.example.net/
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "redirect": "https://bob.example.net/auth/sharing?..."
}
```

### POST /sharings/:sharing-id/preview-url

This internal route can be used by the stack to get the URL where a member can
preview the sharing.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/discovery-url HTTP/1.1
Host: alice.example.net
Content-Type: application/json
```

```json
{
  "state": "eiJ3iepoaihohz1Y"
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "url": "https://drive.alice.example.net/preview?sharecode=..."
}
```

### GET /sharings/:sharing-id

Get the information about a sharing. This includes the content of the rules, the
members, as well as the already shared documents for this sharing.

For a member, we can have the following fields:

- a contact name (`name`), that is the name of this user as it appears in its
  contact document (if there is one such document)
- a public name (`public_name`), that is the name this user has put on his
  cozy as a public name (it is used for sending emails for example)
- an email addresse (`email`)
- an instance URL (`instance`)
- and a status (`status`).

**Notes:**

- the first member is always the sharer
- to display the list of members to a user, the `name` should be use if
  available, and if it is not the case, you can use the `public_name` or the
  `email`
- on a recipient, the only member with an `instance` is the local user.

#### Request

```http
GET /sharings/ce8835a061d0ef68947afe69a0046722 HTTP/1.1
Host: alice.example.net
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "meta": {
      "rev": "1-4859c6c755143adf0838d225c5e97882"
    },
    "attributes": {
      "description": "sharing test",
      "preview_path": "/preview-sharing",
      "app_slug": "drive",
      "owner": true,
      "created_at": "2018-01-04T12:35:08Z",
      "updated_at": "2018-01-04T13:45:43Z",
      "initial_number_of_files_to_sync": 42,
      "members": [
        {
          "status": "owner",
          "public_name": "Alice",
          "email": "alice@example.net",
          "instance": "alice.example.net"
        },
        {
          "status": "ready",
          "name": "Bob",
          "email": "bob@example.net"
        }
      ],
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    },
    "relationships": {
      "shared_docs": {
        "data": [
          {
            "id": "612acf1c-1d72-11e8-b043-ef239d3074dd",
            "type": "io.cozy.files"
          },
          {
            "id": "a34528d2-13fb-9482-8d20-bf1972531225",
            "type": "io.cozy.files"
          }
        ]
      }
    },
    "links": {
      "self": "/sharings/ce8835a061d0ef68947afe69a0046722"
    }
  }
}
```

### PATCH /sharings/:id

This endpoint allows to update the description of a sharing.

#### Request

```http
PATCH /sharings/ce8835a061d0ef68947afe69a0046722 HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
  "data": {
    "type": "sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "attributes": {
      "description": "this is an updated description"
    }
  }
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "meta": {
      "rev": "2-8f7c18cb8c17"
    },
    "attributes": {
      "description": "this is an updated description",
      "preview_path": "/preview-sharing",
      "app_slug": "drive",
      "owner": true,
      "created_at": "2018-01-04T12:35:08Z",
      "updated_at": "2018-01-04T13:45:43Z",
      "initial_number_of_files_to_sync": 42,
      "members": [
        {
          "status": "owner",
          "public_name": "Alice",
          "email": "alice@example.net",
          "instance": "alice.example.net"
        },
        {
          "status": "ready",
          "name": "Bob",
          "email": "bob@example.net"
        }
      ],
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    },
    "relationships": {
      "shared_docs": {
        "data": [
          {
            "id": "612acf1c-1d72-11e8-b043-ef239d3074dd",
            "type": "io.cozy.files"
          },
          {
            "id": "a34528d2-13fb-9482-8d20-bf1972531225",
            "type": "io.cozy.files"
          }
        ]
      }
    },
    "links": {
      "self": "/sharings/ce8835a061d0ef68947afe69a0046722"
    }
  }
}
```

### GET /sharings/news

It returns the number of shortcuts to a sharing that have not been seen.

#### Request

```http
GET /sharings/news HTTP/1.1
Host: alice.example.net
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "meta": {
    "count": 5
  }
}
```

### GET /sharings/doctype/:doctype

Get information about all the sharings that have a rule for the given doctype.
This includes the content of the rules, the members, as well as the already
shared documents for this sharing.
A `shared_docs` query parameter is supported to control whether or not the
shared docs should be included in the response. Default is true.
This can be costful in case of large sharings, so it can be a good idea to set
it to false for performances. 

#### Request

```http
GET /sharings/doctype/io.cozy.files?shared_docs=true HTTP/1.1
Host: alice.example.net
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": [
    {
      "type": "io.cozy.sharings",
      "id": "ce8835a061d0ef68947afe69a0046722",
      "attributes": {
        "description": "sharing test",
        "preview_path": "/preview-sharing",
        "app_slug": "drive",
        "owner": true,
        "created_at": "2018-01-04T12:35:08Z",
        "updated_at": "2018-01-04T13:45:43Z",
        "members": [
          {
            "status": "owner",
            "public_name": "Alice",
            "email": "alice@example.net",
            "instance": "alice.example.net"
          },
          {
            "status": "ready",
            "name": "Bob",
            "email": "bob@example.net"
          }
        ],
        "rules": [
          {
            "title": "Hawaii",
            "doctype": "io.cozy.files",
            "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
            "add": "sync",
            "update": "sync",
            "remove": "sync"
          }
        ]
      },
      "meta": {
        "rev": "1-4859c6c755143adf0838d225c5e97882"
      },
      "links": {
        "self": "/sharings/ce8835a061d0ef68947afe69a0046722"
      },
      "relationships": {
        "shared_docs": {
          "data": [
            {
              "id": "612acf1c-1d72-11e8-b043-ef239d3074dd",
              "type": "io.cozy.files"
            },
            {
              "id": "a34528d2-13fb-9482-8d20-bf1972531225",
              "type": "io.cozy.files"
            }
          ]
        }
      }
    },
    {
      "type": "io.cozy.sharings",
      "id": "b4e58d039c03d01742085de5e505284e",
      "attributes": {
        "description": "another sharing test",
        "preview_path": "/preview-sharing",
        "app_slug": "drive",
        "owner": true,
        "created_at": "2018-02-04T12:35:08Z",
        "updated_at": "2018-02-04T13:45:43Z",
        "members": [
          {
            "status": "owner",
            "public_name": "Alice",
            "email": "alice@example.net",
            "instance": "alice.example.net"
          },
          {
            "status": "ready",
            "name": "Bob",
            "email": "bob@example.net"
          }
        ],
        "rules": [
          {
            "title": "Singapore",
            "doctype": "io.cozy.files",
            "values": ["e18e30e2-8eda-1bde-afce-edafc6b1a91b"],
            "add": "sync",
            "update": "sync",
            "remove": "sync"
          }
        ]
      },
      "meta": {
        "rev": "1-7ac5f1252a0c513186a5d35b1a6fd350"
      },
      "links": {
        "self": "/sharings/b4e58d039c03d01742085de5e505284e"
      },
      "relationships": {
        "shared_docs": {
          "data": [
            {
              "id": "dcc52bee-1277-a6b3-b36f-369ffd81a4ee",
              "type": "io.cozy.files"
            }
          ]
        }
      }
    }
  ],
  "meta": {
    "count": 3
  }
}
```

### PUT /sharings/:sharing-id

The sharer's cozy sends a request to this route on the recipient's cozy to
create a sharing request, with information about the sharing. This request can
be used for two scenarios:

1. This request will be displayed to the recipient just before its final
   acceptation of the sharing, to be sure they know what will be shared.
2. This request will be used to create a shortcut (in that case, a query-string
   parameter `shortcut=true&url=...` will be added).

Drive sharings always include in the payload a credentials entry containing the
OAuth state generated for each recipient. When the recipient Cozy is configured
with `sharing.auto_accept_trusted = true`, receiving this state enqueues a
background job that performs the same handshake as a manual acceptance and
POSTs the answer back to the owner's Cozy.

Trusted members are determined by the recipient Cozy only. The recipient checks
that the sender belongs to a trusted domain (or has been marked as a trusted
contact) before launching the auto-accept flow.

Members are trusted when their instance domain exactly matches or is a
subdomain of any of the configured trusted domains. The behaviour can be tuned
via the configuration:

```yaml
sharing:
  auto_accept_trusted: true
  auto_accept_trusted_contacts: true
  contexts:
    default:
      trusted_domains:
        - example.com
    white-label:
      auto_accept_trusted: false
```

`trusted_domains` must be defined inside a context. Use the `default` context
to configure a global rule, or override it per custom context.

If `auto_accept_trusted` is disabled on the recipient Cozy, the request is kept
in the pending state even if the sharer provided the state in the credentials
payload.

### Contact-Based Trust

In addition to domain-based trust, contacts can be marked as trusted when you
accept a sharing from them. This provides a more granular trust model:

- **First sharing**: User manually accepts, sender's contact is automatically marked as trusted
- **Subsequent sharings**: Automatically accepted because the contact is trusted

Contact-based trust works independently of domain-based trust (and can be
disabled via `sharing.auto_accept_trusted_contacts` or the corresponding
context override):
- A contact from an untrusted domain can still be trusted
- This allows trusting specific individuals without trusting entire domains
- Contact trust takes effect even if domain-based trust is not configured

**Example workflow:**
1. Alice receives a Drive sharing from `bob@external.com` (untrusted domain)
2. Alice manually accepts the sharing
3. Bob's contact is automatically marked as trusted in Alice's Cozy
4. Future Drive sharings from Bob are auto-accepted (if `auto_accept_trusted` is enabled)

**Trust determination:**
A member is considered trusted if **either**:
- Their instance domain matches a configured trusted domain (domain-based trust)
- Their contact has been marked as trusted (contact-based trust)

The contact's `trustedForSharing` field is set to `true` when a sharing is accepted.

#### Request

```http
PUT /sharings/ce8835a061d0ef68947afe69a0046722 HTTP/1.1
Host: bob.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "attributes": {
      "description": "sharing test",
      "preview_path": "/preview-sharing",
      "app_slug": "drive",
      "owner": true,
      "created_at": "2018-01-04T12:35:08Z",
      "updated_at": "2018-01-04T13:45:43Z",
      "members": [
        {
          "status": "owner",
          "public_name": "Alice",
          "email": "alice@example.net",
          "instance": "alice.example.net"
        },
        {
          "status": "mail-not-sent",
          "email": "bob@example.net",
          "instance": "bob.example.net"
        }
      ],
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    }
  }
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "meta": {
      "rev": "1-f579a69a9fa5dd720010a1dbb82320be"
    },
    "attributes": {
      "description": "sharing test",
      "preview_path": "/preview-sharing",
      "app_slug": "drive",
      "owner": true,
      "created_at": "2018-01-04T12:35:08Z",
      "updated_at": "2018-01-04T13:45:43Z",
      "members": [
        {
          "status": "owner",
          "public_name": "Alice",
          "email": "alice@example.net",
          "instance": "alice.example.net"
        },
        {
          "status": "mail-not-sent",
          "name": "Bob",
          "email": "bob@example.net",
          "instance": "bob.example.net"
        }
      ],
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    },
    "links": {
      "self": "/sharings/ce8835a061d0ef68947afe69a0046722"
    }
  }
}
```

### POST /sharings/:sharing-id/answer

This route is used by the Cozy of a recipient to exchange credentials with the
Cozy of the sharer, after the recipient has accepted a sharing.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/answer HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings.answer",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "attributes": {
      "public_name": "Bob",
      "state": "eiJ3iepoaihohz1Y",
      "client": {...},
      "access_token": {...}
    }
  }
}
```

When the sharing has a rule for bitwarden organization, the `attributes` also
have a `bitwarden` object with `user_id` and `public_key`, to make it possible
to share documents end to end encrypted.

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings.answer",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "attributes": {
      "client": {...},
      "access_token": {...}
    }
  }
}
```

### POST /sharings/:sharing-id/public-key

This route can be used by a sharing member to send their new public key after
they have chosen their password for the first time (delegated authentication).

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/public-key HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
Authorization: Bearer ...
```

```json
{
  "data": {
    "type": "io.cozy.sharings.answer",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "attributes": {
      "bitwarden": {
        "user_id": "0dc76ad9b1cf3a979b916b3155001830",
        "public_key": "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA1wboB9QutvRKGswphAre6xi4boYxSw4IjXqXDTV297Cq/MB2cBklj+sMmRQNTkU266HFSTGp3jDVcegAsHMpVVlTKZnWW+gSP2+vSyGs9NUvG8JfLday1iuOntHJQkfYiZ7+BfBYFtx6iWRnbnegickyoS7PWibLP5lmEzZnFtrGcRT8urSfN61qlOVD1u+eqtif5tabdU6eyNWUMLCQYgtaGb1nNht/xDgbpBc3b2XbEF0tBJRFR5571EH1h//Ae7IT+pYuBZB9BoPAHj4fhQm3++oNjemUJbaVi0dM4KNfQ89z1lBBCh5lxAGPlpOjapN0qGPgSov8B9U+qXlmzQIDAQAB"
      }
    }
  }
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /sharings/:sharing-id/recipients

This route allows the sharer to add new recipients (and groups of recipients)
to a sharing. It can also be used by a recipient when the sharing has
`open_sharing` set to true if the recipient doesn't have the `read_only` flag.
Adding a recipient who is already active is idempotent: that recipient is
ignored, while the other recipients from the same request are still processed.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/recipients HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "relationships": {
      "recipients": {
        "data": [
          {
            "id": "ce7b1dfbd460039159f228298a29b2aa",
            "type": "io.cozy.contacts"
          }
        ]
      },
      "read_only_recipients": {
        "data": [
          {
            "id": "e15384a1223ae2501cc1c4fa94008ea0",
            "type": "io.cozy.contacts"
          }
        ]
      }
    }
  }
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "meta": {
      "rev": "1-4859c6c755143adf0838d225c5e97882"
    },
    "attributes": {
      "description": "sharing test",
      "preview_path": "/preview-sharing",
      "app_slug": "drive",
      "owner": true,
      "created_at": "2018-01-04T12:35:08Z",
      "updated_at": "2018-01-04T13:45:43Z",
      "members": [
        {
          "status": "owner",
          "public_name": "Alice",
          "email": "alice@example.net",
          "instance": "alice.example.net"
        },
        {
          "status": "ready",
          "name": "Bob",
          "public_name": "Bob",
          "email": "bob@example.net"
        },
        {
          "status": "pending",
          "name": "Charlie",
          "email": "charlie@example.net"
        },
        {
          "status": "pending",
          "name": "Dave",
          "email": "dave@example.net",
          "read_only": true
        }
      ],
      "rules": [
        {
          "title": "Hawaii",
          "doctype": "io.cozy.files",
          "values": ["612acf1c-1d72-11e8-b043-ef239d3074dd"],
          "add": "sync",
          "update": "sync",
          "remove": "sync"
        }
      ]
    },
    "relationships": {
      "shared_docs": {
        "data": [
          {
            "id": "612acf1c-1d72-11e8-b043-ef239d3074dd",
            "type": "io.cozy.files"
          },
          {
            "id": "a34528d2-13fb-9482-8d20-bf1972531225",
            "type": "io.cozy.files"
          }
        ]
      }
    },
    "links": {
      "self": "/sharings/ce8835a061d0ef68947afe69a0046722"
    }
  }
}
```

### POST /sharings/:sharing-id/recipients/delegated

This is an internal route for the stack. It is called by the recipient cozy on
the owner cozy to add recipients and groups to the sharing. It is used for
delegated recipient additions from recipient Cozys, including shared-drive
recipient invitations. Data for direct recipients should contain an email
address but if it is not known, an instance URL can also be provided. For
shared drives, each delegated recipient or group can also carry a `read_only`
flag, and the owner applies the drive-specific reshare rules when processing
the request. The owner persists the complete member and credential batch before
sending shortcuts, so immediate auto-acceptance can safely resolve the
invitation state.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/recipients/delegated HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "relationships": {
      "recipients": {
        "data": [
          {
            "email": "dave@example.net"
          }
        ]
      },
      "groups": {
        "data": [
          {
            "id": "b57cd790b2f4013c3ced18c04daba326",
            "name": "Dance",
            "addedBy": 1
          }
        ]
      }
    }
  }
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "dave@example.net": "uS6wN7fTYaLZ-GdC_P6UWA"
}
```

### POST /sharings/:sharing-id/members/:index/invitation

This is an internal route for the stack. It is called by the recipient cozy on
the owner cozy to send an invitation.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/members/4/invitation HTTP/1.1
Host: alice.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings.members",
    "attributes": {
      "email": "diana@example.net"
    }
  }
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "diana@example.net": "uS6wN7fTYaLZ-GdC_P6UWA"
}
```

### DELETE /sharings/:sharing-id/groups/:group-index/:member-index

This is an internal route for the stack. It is called by the recipient cozy on
the owner cozy to remove a member of a sharing from a group.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/groups/0/1 HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### PUT /sharings/:sharing-id/recipients

This internal route is used to update the list of members (their states, emails
and names) and the list of groups on the recipients cozy. The token used for
this route can be the access token for a sharing where synchronization is
active, or the sharecode for a member who has only a shortcut to the sharing on
their Cozy instance.

#### Request

```http
PUT /sharings/ce8835a061d0ef68947afe69a0046722/recipients HTTP/1.1
Host: bob.example.net
Content-Type: application/vnd.api+json
```

```json
{
  "data": [
    {
      "status": "owner",
      "public_name": "Alice",
      "email": "alice@example.net",
      "instance": "alice.example.net"
    },
    {
      "status": "ready",
      "name": "Bob",
      "public_name": "Bob",
      "email": "bob@example.net"
    },
    {
      "status": "ready",
      "name": "Charlie",
      "public_name": "Charlie",
      "email": "charlie@example.net"
    },
    {
      "status": "pending",
      "name": "Dave",
      "email": "dave@example.net",
      "read_only": true
    }
  ],
  "included": [
    {
      "id": "51bbc980acb0013cb5f618c04daba326",
      "name": "G. people",
      "addedBy": 0
    }
  ]
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### GET /sharings/:sharing-id/recipients/:index/avatar

This route can be used to get an image that shows the avatar of a member of
this sharing. No permission is required to use this route, you just need to
know the sharing-id to use it. If no image has been chosen, a fallback will be
used, depending of the `fallback` parameter in the query-string:

- `initials`: a generated image with the initials of the owner's public name. This is the default behavior.
- `404`: just a 404 - Not found error.

If the `initials` fallback is used and the member has not yet seen the sharing,
the background of the image will be grey.

**Note**: 0 for the index means the sharer.

#### Request

```http
GET /sharings/ce8835a061d0ef68947afe69a0046722/recipients/2/avatar HTTP/1.1
Host: bob.example.net
Accept: image/*
```

### POST /sharings/:sharing-id/recipients/:index/readonly

This route is used to add the read-only flag on a recipient of a sharing.

**Note**: 0 is not accepted for `index`, as it is the sharer him-self.

##### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/recipients/3/readonly HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /sharings/:sharing-id/recipients/self/readonly

This is an internal route for the stack. It's used to inform the recipient's
cozy that it is no longer in read-write mode. It also gives it an access token
with a short validity (1 hour) to let it try to synchronize its last changes
before going to read-only mode.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/recipients/self/readonly HTTP/1.1
Host: bob.example.net
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings.answer",
    "id": "4dadbcae3f2d7a982e1b308eea000751",
    "attributes": {
      "client": {...},
      "access_token": {...}
    }
  }
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/recipients/:index/readonly

This route is used to remove the read-only flag on a recipient of a sharing.

**Note**: 0 is not accepted for `index`, as it is the sharer him-self.

##### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/recipients/3/readonly HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/recipients/self/readonly

This is an internal route for the stack. It's used to inform the recipient's
cozy that it is no longer in read-only mode, and to give it the credentials for
sending its updates.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/recipients/self/readonly HTTP/1.1
Host: bob.example.net
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings.answer",
    "id": "4dadbcae3f2d7a982e1b308eea000751",
    "attributes": {
      "client": {...},
      "access_token": {...}
    }
  }
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/recipients

This route is used by an application on the owner's cozy to revoke the sharing
for all the members. After that, the sharing active flag will be false, the
credentials for all members will be revoked, the members that have accepted the
sharing will have their cozy informed that the sharing has been revoked, and
pending members can no longer accept this sharing.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/recipients HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/recipients/:index

This route can be only be called on the cozy instance of the sharer to revoke
only one recipient of the sharing. The parameter is the index of this recipient
in the `members` array of the sharing.
The status for this member will be set to `revoked`, its cozy will be informed of the
revokation, and the credentials for this cozy will be deleted.

**Note**: 0 is not accepted for `index`, as it is the sharer him-self.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/recipients/1 HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/groups/:index

This route can be only be called on the cozy instance of the sharer to revoke a
group of the sharing. The parameter is the index of this recipient in the
`groups` array of the sharing. The `removed` property for this group will be
set to `true`, and it will revoke the members of this group unless they are
still part of the sharing via another group or as direct recipients.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/groups/0 HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/recipients/self

This route can be used by an application in the cozy of a recipient to remove it
from the sharing.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/recipients/self HTTP/1.1
Host: bob.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id

This is an internal route used by the cozy of the sharing's owner to inform a
recipient's cozy that it was revoked.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722 HTTP/1.1
Host: bob.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/answer

This is an internal route used by a recipient's cozy to inform the owner's cozy
that this recipient no longer wants to be part of the sharing.

#### Request

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/answer HTTP/1.1
Host: alice.example.net
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /sharings/:sharing-id/recipients/self/moved

This route can be used to inform that a Cozy has been moved to a new address.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/recipients/self/moved HTTP/1.1
Host: bob.example.net
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings.moved",
    "id": "ce8835a061d0ef68947afe69a0046722",
    "attributes": {
      "new_instance": "https://alice.newcozy.example",
      "access_token": "xxx",
      "refresh_token": "xxx"
    }
  }
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /sharings/:sharing-id/\_revs_diff

This endpoint is used by the sharing replicator of the stack to know which
documents must be sent to the other cozy. It is inspired by
http://docs.couchdb.org/en/stable/api/database/misc.html#db-revs-diff.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/_revs_diff HTTP/1.1
Host: bob.example.net
Accept: application/json
Content-Type: application/json
Authorization: Bearer ...
```

```json
{
  "io.cozy.files/29631902-2cec-11e8-860d-435b24c2cc58": [
    "2-4a7e4ae49c4366eaed8edeaea8f784ad"
  ],
  "io.cozy.files/44f5752a-2cec-11e8-b227-abfc3cfd4b6e": [
    "4-2ee767305024673cfb3f5af037cd2729",
    "4-efc54218773c6acd910e2e97fea2a608"
  ]
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "io.cozy.files/44f5752a-2cec-11e8-b227-abfc3cfd4b6e": {
    "missing": ["4-2ee767305024673cfb3f5af037cd2729"],
    "possible_ancestors": ["3-753875d51501a6b1883a9d62b4d33f91"]
  }
}
```

### POST /sharings/:sharing-id/\_bulk_docs

This endpoint is used by the sharing replicator of the stack to send documents
in a bulk to the other cozy. It is inspired by
http://docs.couchdb.org/en/stable/api/database/bulk-api.html#db-bulk-docs.

**Note**: we force `new_edits` to `false`.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/_bulk_docs HTTP/1.1
Host: bob.example.net
Accept: application/json
Content-Type: application/json
Authorization: Bearer ...
```

```json
{
  "io.cozy.files": [
    {
      "_id": "44f5752a-2cec-11e8-b227-abfc3cfd4b6e",
      "_rev": "4-2ee767305024673cfb3f5af037cd2729",
      "_revisions": {
        "start": 4,
        "ids": [
          "2ee767305024673cfb3f5af037cd2729",
          "753875d51501a6b1883a9d62b4d33f91"
        ]
      }
    }
  ]
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
[]
```

### GET /sharings/:sharing-id/io.cozy.files/:file-id

This is an internal endpoint used by a stack to get information about a folder.
It is used when a cozy sent to another cozy a file or folder inside a folder
that was trashed (and trash was emptied): the recipient does no longer have
information about the parent directory. To resolve the conflict, it recreates
the missing parent directory by asking the other cozy informations about it.

#### Request

```http
GET /sharings/ce8835a061d0ef68947afe69a0046722/io.cozy.files/6d245d072be5522bd3a6f273dd000c65 HTTP/1.1
Host: alice.example.net
Accept: application/json
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "_id": "6d245d072be5522bd3a6f273dd000c65",
  "_rev": "1-de4ec176ffa9ddafe8bdcc739dc60fed",
  "type": "directory",
  "name": "phone",
  "dir_id": "6d245d072be5522bd3a6f273dd007396",
  "created_at": "2016-09-19T12:35:08Z",
  "updated_at": "2016-09-19T12:35:08Z",
  "tags": ["bills"]
}
```

### PUT /sharings/:sharing-id/io.cozy.files/:file-id/metadata

This is an internal endpoint used by a stack to send the new metadata about a
file that has changed.

#### Request

```http
PUT /sharings/ce8835a061d0ef68947afe69a0046722/io.cozy.files/0c1116b028c6ae6f5cdafb949c088265/metadata HTTP/1.1
Host: bob.example.net
Accept: application/json
Content-Type: application/json
Authorization: Bearer ...
```

```json
{
  "_id": "4b24ab130b2538b7b444fc65430198ad",
  "_rev": "1-356bf77c03baa1da851a2be1f06aba81",
  "_revisions": {
    "start": 1,
    "ids": ["356bf77c03baa1da851a2be1f06aba81"]
  },
  "type": "file",
  "name": "cloudy.jpg",
  "dir_id": "4b24ab130b2538b7b444fc65430188cd",
  "created_at": "2018-01-03T16:10:36.885807013+01:00",
  "updated_at": "2018-01-03T16:10:36.885807013+01:00",
  "size": "84980",
  "md5sum": "SuRJOiD/QPwDUpKpQujcVA==",
  "mime": "image/jpeg",
  "class": "image",
  "executable": false,
  "trashed": false,
  "tags": [],
  "metadata": {
    "datetime": "2018-01-03T16:10:36.89118949+01:00",
    "extractor_version": 2,
    "height": 1200,
    "width": 1600
  }
}
```

#### Response

If only the metadata has changed (not the content), the response will be a 204:

```http
HTTP/1.1 204 No Content
```

Else, the content will need to be uploaded:

```
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "key": "dcd478c6-46cf-11e8-9c3f-535468cbce7b"
}
```

### PUT /sharings/:sharing-id/io.cozy.files/:key

Upload the content of a file (new file or its content has changed since the last
synchronization).

#### Request

```http
PUT /sharings/ce8835a061d0ef68947afe69a0046722/io.cozy.files/dcd478c6-46cf-11e8-9c3f-535468cbce7b HTTP/1.1
Host: bob.example.net
Content-Type: image/jpeg
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 204 No Content
```

### PUT /sharings/:sharing-id/metadata

This internal route lets the sharer's Cozy propagate sharing metadata (currently
the description) to the recipients once a rename or manual edit occurs. It is
triggered by the replicator using the sharing-scoped token obtained during the
OAuth flow.

#### Request

```http
PUT /sharings/ce8835a061d0ef68947afe69a0046722/metadata HTTP/1.1
Host: bob.example.net
Authorization: Bearer ...
Content-Type: application/json
```

```json
{
  "description": "New sharing description"
}
```

#### Response

```http
HTTP/1.1 204 No Content
```

#### Error responses

- `400 Bad Request` when the JSON payload cannot be parsed or the description is
  longer than the allowed limit.
- `401 Unauthorized` when the request is missing or carries an invalid token.
- `403 Forbidden` when the token does not grant write access to this sharing.
- `404 Not Found` when the sharing identifier does not exist on the recipient
  Cozy.

### POST /sharings/:sharing-id/reupload

This is an internal route for the stack. It is called when the disk quota of an
instance is increased to ask for the others instances on this sharing to try to
reupload files without waiting for the normal retry period.

#### Request

```http
POST /sharings/ce8835a061d0ef68947afe69a0046722/reupload HTTP/1.1
Host: bob.example.net
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /sharings/:sharing-id/initial

This internal route is used by the sharer to inform a recipient's cozy that the
initial sync is finished.

```http
DELETE /sharings/ce8835a061d0ef68947afe69a0046722/initial HTTP/1.1
Host: bob.example.net
Authorization: Bearer ...
```

#### Response

```http
HTTP/1.1 204 No Content
```

## Real-time via websockets

You can subscribe to the [realtime](realtime.md) API for the normal doctypes,
but also for a special `io.cozy.sharings.initial_sync` doctype. For this
doctype, you can give the id of a sharing and you will be notified when a file
will be received during the initial synchronisation (`UPDATED`), and when the
sync will be done (`DELETED`).

### Example

```
client > {"method": "AUTH",
          "payload": "xxAppOrAuthTokenxx="}
client > {"method": "SUBSCRIBE",
          "payload": {"type": "io.cozy.sharings.initial_sync", "id": "ce8835a061d0ef68947afe69a0046722"}}
server > {"event": "UPDATED",
          "payload": {"id": "ce8835a061d0ef68947afe69a0046722", "type": "io.cozy.sharings.initial_sync", "doc": {"count": 12}}}
server > {"event": "UPDATED",
          "payload": {"id": "ce8835a061d0ef68947afe69a0046722", "type": "io.cozy.sharings.initial_sync", "doc": {"count": 13}}}
server > {"event": "DELETED",
          "payload": {"id": "ce8835a061d0ef68947afe69a0046722", "type": "io.cozy.sharings.initial_sync"}}
```
