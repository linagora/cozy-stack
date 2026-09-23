[Table of contents](README.md#table-of-contents)

# Permissions

## When the permissions are used?

The permissions are used when a request is made to cozy stack. It allows to let
the owner of the cozy instance controls the access to her data, files and
actions on them. The permissions are given in several contexts. Let's see them!

### Client-side apps

When the user installs a new client-side app, she is asked to accept an initial
set of permissions for this app. This set of permissions is described in the
manifest of the app.

Later, the application can gain more permissions via the [intents](intents.md)
and optional permissions. See below for more details.

When the authentified user access a client-side app, the app receives a token
from the stack that can be used in later requests to the stack as a proof of the
permissions it owns.

### External apps via OAuth2

An external application can ask for permissions via the OAuth2 dance, and use
them later with the access token. The permissions are in the `scope` parameter.

### Sharing with other users

The owner of a cozy instance can share some documents and files with other
users. It can be done in two ways:

-   If the other user also has a cozy, it can be a cozy-to-cozy sharing.
-   Else, the owner can give to him a link with a code or a shortcode.

## What is a permission?

A permission gives the right for a request having it to do something on the
stack. It is defined by four components.

### Type

`type` is the attribute used in JSON-API or the `doctype` for the Data System.

It is the only mandatory component. If just the `type` is specified, it gives
access to all the operations on this `type`. For example, a permission on type
`io.cozy.contacts` gives the right to create, read, update and delete any
contact, and to fetch all the contacts. A permission on type `io.cozy.files`
allow to access and modify any file or directory.

Some known types:

-   `io.cozy.files`, for files and folder in the [VFS](files.md)
-   `io.cozy.apps`, for [apps](apps.md)
-   `io.cozy.settings`, for the [settings](settings.md)
-   `io.cozy.jobs` and `io.cozy.triggers`, for [jobs](jobs.md)
-   `io.cozy.oauth.clients`, to list and revoke [OAuth 2 clients](auth.md)

It is also possible to use a wildcard to use a doctype and its sub-doctypes if
the doctype contains at least 3 `.`.
For example, `io.cozy.bank.*` will give access to `io.cozy.bank`,
`io.cozy.bank.accounts`, `io.cozy.bank.accounts.stats`,
`io.cozy.bank.settings`, etc. `io.cozy.*` will give you an error.

### Verbs

It says which HTTP verbs can be used for requests to the cozy-stack. `GET` will
give read-only access, `DELETE` can be used for deletions, etc. Verbs should be
declared in a list, like `["GET", "POST", "DELETE"]`, and use `["ALL"]` as a
shortcut for `["GET", "POST", "PUT", "PATCH", "DELETE"]` (it is the default).

**Note**: `HEAD` is implicitely implied when `GET` is allowed. `OPTIONS` for
Cross-Origin Resources Sharing is always allowed, the stack does not have the
informations about the permission when it answers the request.

### Values

It's possible to restrict the permissions to only some documents of a doctype,
or to just some files and folders. You can give a list of ids in `values`.

**Note**: a permission for a folder also gives permissions with same verbs for
files and folders inside it.

### Selector

By default, the `values` are checked with the `id`. But it's possible to use a
`selector` to filter on another `field`. In particular, it can be used for
sharing. A user may share a calendar and all the events inside it. It will be
done with two permissions. The first one is for the calendar:

```json
{
    "type": "io.cozy.calendars",
    "verbs": ["GET"],
    "values": ["1355812c-d41e-11e6-8467-53be4648e3ad"]
}
```

And the other is for the events inside the calendar:

```json
{
    "type": "io.cozy.events",
    "verbs": ["GET"],
    "selector": "calendar-id",
    "values": ["1355812c-d41e-11e6-8467-53be4648e3ad"]
}
```

## What format for a permission?

### JSON

The prefered format for permissions is JSON. Each permission is a map with the
`type`, `verbs`, `values` and `selector` see above, plus a `description` that
can be used to give more informations to the user. Only the `type` field is
mandatory.

In the manifest, the permissions are regrouped in a map. The key is not very
relevant, it's just here for localization. The same key is used in the `locales`
field to identify the permission.

Example:

```json
{
    "permissions": {
        "contacts": {
            "description": "Required for autocompletion on @name",
            "type": "io.cozy.contacts",
            "verbs": ["GET"]
        },
        "images": {
            "description": "Required for the background",
            "type": "io.cozy.files",
            "verbs": ["GET", "POST"],
            "values": ["io.cozy.files.music-dir"]
        },
        "mail": {
            "description": "Required to send a congratulations email to your friends",
            "type": "io.cozy.jobs",
            "selector": "worker",
            "values": ["sendmail"]
        }
    }
}
```

### Inline

OAuth2 as a `scope` parameter for defining the permissions given to the
application. But it's only a string, not a JSON. In that case, we use a space
delimited list of permissions, each permission is written compactly with `:`
between the components.

Example:

```
io.cozy.contacts io.cozy.files:GET:io.cozy.files.music-dir io.cozy.jobs:POST:sendmail:worker
```

**Note**: the `verbs` component can't be omitted when the `values` and
`selector` are used.

### Inspiration

-   [Access control on other similar platforms](https://news.ycombinator.com/item?id=12784999)

## Routes

### GET /permissions/self

List the permissions for a given token

#### Request

```http
GET /permissions/self HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
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
        "type": "io.cozy.permissions",
        "id": "5a9c1844-d427-11e6-ab36-2b684d437b0d",
        "attributes": {
            "type": "app",
            "source_id": "io.cozy.apps/my-awesome-game",
            "permissions": {
                "contacts": {
                    "description": "Required for autocompletion on @name",
                    "type": "io.cozy.contacts",
                    "verbs": ["GET"]
                },
                "images": {
                    "description": "Required for the background",
                    "type": "io.cozy.files",
                    "verbs": ["GET"],
                    "values": ["io.cozy.files.music-dir"]
                },
                "mail": {
                    "description": "Required to send a congratulations email to your friends",
                    "type": "io.cozy.jobs",
                    "selector": "worker",
                    "values": ["sendmail"]
                }
            }
        }
    }
}
```

#### Special case

The token can be a sharecode when a member of a sharing is previewing a
sharing. When it is the case, the member information from the sharing is
included in the response, like this:

```json
{
	"data": {
		"type": "io.cozy.permissions",
		"id": "bf716babb70b73b89c870fccce00233f",
		"attributes": {
			"type": "share-preview",
			"source_id": "io.cozy.sharings/bf716babb70b73b89c870fccce00233a",
			"permissions": {
				"Essai": {
					"type": "io.cozy.files",
					"verbs": ["GET"],
					"values": ["c2930bef3705096d63f5c8fb60019cc0"]
				}
			},
			"cozyMetadata": {
				"doctypeVersion": "",
				"metadataVersion": 1,
				"createdAt": "2020-10-19T16:12:31.440253797+02:00",
				"createdByApp": "drive",
				"updatedAt": "2020-10-19T16:12:31.440253797+02:00"
			}
		},
		"meta": {
			"rev": "1-18c3bc158cf5039f60fefe5fc2fcad67"
		},
		"links": {
			"self": "/permissions/bf716babb70b73b89c870fccce00233f",
			"related": "/sharings/bf716babb70b73b89c870fccce00233a"
		}
	},
	"included": [
		{
			"type": "io.cozy.sharings.members",
			"attributes": {
				"status": "seen",
				"name": "Bob",
				"email": "bob@cozy.localhost"
			}
		}
	]
}
```

### POST /permissions

Create a new set of permissions. It can also associates one or more codes to it,
via the `codes` parameter in the query string. These codes can then be sent to
other people as a way to give these permissions (sharing by links). For each
`code` created, a corresponding `shortcode` is generated and can be used to
shorten links for sharing. The `codes` parameter is a comma separed list of
values. The role of these values is to identify the codes if you want to revoke
some of them later. A `ttl` parameter can also be given to make the codes
expires after a delay ([bigduration
format](https://github.com/justincampbell/bigduration/blob/master/README.md)).

If the `ttl` does not exceed 1 hour, it is possible to add a `tinycode=true`
parameter to the query-string to have a shortcode of 6 digits.

**Note**: it is only possible to create a strict subset of the permissions
associated to the sent token.
Rules on `io.cozy.files` that target documents by id are also accepted when
the token can perform the requested verbs on each document (same check as
normal file access), so an app restricted by a selector such as
`referenced_by` can share files it can access. The same applies when adding
rules with `PATCH /permissions/:id`.

#### Request

```http
POST /permissions?codes=bob,jane&ttl=1D HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
        "type": "io.cozy.permissions",
        "attributes": {
            "source_id": "io.cozy.apps/my-awesome-game",
            "password": "HelloWorld!",
            "permissions": {
                "images": {
                    "type": "io.cozy.files",
                    "verbs": ["GET"],
                    "values": ["io.cozy.files.music-dir"]
                }
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
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "attributes": {
            "type": "share",
            "source_id": "io.cozy.apps/my-awesome-game",
            "codes": {
                "bob": "yuot7NaiaeGugh8T",
                "jane": "Yohyoo8BHahh1lie"
            },
            "shortcodes": {
                "bob": "abcdeFGHIJ01",
                "jane": "123456aBCdef"
            },
            "expires_at": 1483951978,
            "password": true,
            "permissions": {
                "images": {
                    "type": "io.cozy.files",
                    "verbs": ["GET"],
                    "values": ["io.cozy.files.music-dir"]
                }
            }
        }
    }
}
```

### GET /permissions/:id

Return the informations about a set of permissions

#### Request

```http
GET /permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
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
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "attributes": {
            "type": "share",
            "source_id": "io.cozy.apps/my-awesome-game",
            "codes": {
                "bob": "yuot7NaiaeGugh8T",
                "jane": "Yohyoo8BHahh1lie"
            },
            "shortcodes": {
                "bob": "abcdeFGHIJ01",
                "jane": "123456aBCdef"
            },
            "expires_at": 1483951978,
            "password": true,
            "permissions": {
                "images": {
                    "type": "io.cozy.files",
                    "verbs": ["GET"],
                    "values": ["io.cozy.files.music-dir"]
                }
            }
        }
    }
}
```

### PATCH /permissions/:id

Add permissions in this permissions set. It can be used in inter-apps context as
a way to give another app the permission for some data. For example, the contact
application can send a "_pick a photo_" intent to the photos application with
its permission id, and the photos app can then let the user choose a photo and
give the contacts application the permissions to use it.

This route also accepts a [document metadata](https://github.com/cozy/cozy-doctypes/#document-metadata) to update document informations.

Giving an empty string for `password` or `expires_at` will remove it (while
omitting the field will keep the old value).

#### Request to add / remove codes with a document metadata

```http
PATCH /permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "attributes": {
            "codes": {
                "jane": "Yohyoo8BHahh1lie"
            }
        },
        "cozyMetadata": {
            "doctypeVersion": 1,
            "metadataVersion": 1,
            "updatedAt": "2019-05-14T12:00:37.372193145+02:00"
        }
    }
}
```

#### Request to update the password and the expiration date of the sharing link

```http
PATCH /permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "attributes": {
            "password": "NewPassword",
            "expires_at": "2025-01-01T00:00:00Z"
        },
        "cozyMetadata": {
            "doctypeVersion": 1,
            "metadataVersion": 1,
            "updatedAt": "2019-05-14T12:00:37.372193145+02:00"
        }
    }
}
```

#### Request to remove the password and the expiration date of the sharing link

```http
PATCH /permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "attributes": {
            "password": "",
            "expires_at": ""
        },
        "cozyMetadata": {
            "doctypeVersion": 1,
            "metadataVersion": 1,
            "updatedAt": "2019-05-14T12:00:37.372193145+02:00"
        }
    }
}
```

#### Request to add permissions

```http
PATCH /permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
      "attributes": {
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "permissions": {
            "add-this": {
                "type": "io.cozy.files",
                "verbs": ["GET"],
                "values": ["some-picture-id"]
            }
        }
      }
    }
}
```

#### Request to remove permissions

```http
PATCH /permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
      "attributes": {
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "permissions": {
            "remove-this": {}
        }
      }
    }
}
```

#### Reponse

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
        "type": "io.cozy.permissions",
        "attributes": {
            "type": "share",
            "source_id": "io.cozy.apps/my-awesome-game",
            "codes": {
                "bob": "yuot7NaiaeGugh8T"
            },
            "shortcodes": {
                "bob": "abcdeFGHIJ01"
            },
            "expires_at": 1483951978,
            "permissions": {
                "images": {
                    "type": "io.cozy.files",
                    "verbs": ["GET"],
                    "values": ["io.cozy.files.music-dir"]
                }
            }
        }
    }
}
```

### DELETE /permissions/:id

Delete a set of permissions. For example, some permissions were used by a user
to share a photo album with her friends, and then she changed her mind and
cancel the sharing.

#### Request

```http
DELETE /permissions/fa11561c-d645-11e6-83df-cbf577804d55 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
```

#### Reponse

```http
HTTP/1.1 204 No Content
```

### POST /permissions/exists

List permissions for some documents

#### Request

```http
POST /permissions/exists HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
  "data": [
    { "type": "io.cozy.files", "id": "94375086-e2e2-11e6-81b9-5bc0b9dd4aa4" },
    { "type": "io.cozy.files", "id": "4cfbd8be-8968-11e6-9708-ef55b7c20863" },
    { "type": "io.cozy.files", "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6" },
    { "type": "io.cozy.files", "id": "94375086-e2e2-11e6-81b9-5bc0b9dd4aa4" }
  ]
}
```

#### Reponse

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": [
    { "type": "io.cozy.files", "id": "94375086-e2e2-11e6-81b9-5bc0b9dd4aa4",
      "verbs":["GET"] },
    { "type": "io.cozy.files", "id": "a340d5e0-d647-11e6-b66c-5fc9ce1e17c6",
      "verbs":["GET", "POST"] }
  ]
}
```

### PATCH /permissions/apps/:slug

Add permissions or remove permissions to the web application with specified
slug. It behaves like the `PATCH /permissions/:id` route. See this route for
more examples.

### PATCH /permissions/konnectors/:slug

Add permissions or remove permissions to the konnector with specified slug. It
behaves like the `PATCH /permissions/:id` route. See this route for more
examples.

### GET /permissions/doctype/:doctype/shared-by-link

List permissions for a doctype that are used for a "share by links". This
endpoint is paginated, with a default number of 30 items per page, and a limit
of 100 items per page.

#### Request

```http
GET /permissions/doctype/io.cozy.events/shared-by-link HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiYWRtaW4iOnRydWV9.TJVA95OrM7E2cBab30RMHrHDcEfxjoYZgeFONFh7HgQ
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

#### Reponse

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
    "data": [
        {
            "type": "io.cozy.permissions",
            "id": "c47f82396d09bfcd270343c5855b30a0",
            "attributes": {
                "type": "share",
                "permissions": {
                    "rule0": {
                        "type": "io.cozy.events",
                        "verbs": ["PATCH", "DELETE"],
                        "values": ["c47f82396d09bfcd270343c5855b0eea"]
                    }
                },
                "codes": {
                    "bob": "secret"
                },
                "shortcodes": {
                    "bob": "abcdeFGHIJ01"
                }
            },
            "meta": { "rev": "1-d46b6358683b80c8d59fc55d6de54127" },
            "links": { "self": "/permissions/c47f82396d09bfcd270343c5855b30a0" }
        },
        {
            "type": "io.cozy.permissions",
            "id": "c47f82396d09bfcd270343c5855b351a",
            "attributes": {
                "type": "share",
                "permissions": {
                    "rule0": {
                        "type": "io.cozy.events",
                        "verbs": ["GET"],
                        "values": ["c47f82396d09bfcd270343c5855b169b"]
                    }
                },
                "codes": {
                    "bob": "secret"
                },
                "shortcodes": {
                    "bob": "abcdeFGHIJ01"
                }
            },
            "meta": { "rev": "1-920af658575a56e9e84685f1b09e5c23" },
            "links": { "self": "/permissions/c47f82396d09bfcd270343c5855b351a" }
        }
    ]
}
```

Permissions required: GET on the whole doctype
