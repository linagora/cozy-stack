[Table of contents](README.md#table-of-contents)

# Admin

## Introduction

An admin API is available on the stack. It offers several endpoints to interact
with your cozy-stack installation (E.g. interacting with instances, generating tokens, ...).

:warning: Use the admin API only if you know what you are doing. The admin API
provides a basic authentication, you **must** protect these endpoints as they
are very powerful.

The default port for the admin endpoints is `6060`. If you want to customize the parameters, please see the [config file documentation page](config.md).

## Instance

### GET /instances

Returns the list of all instances. By default, there is no pagination, but it
is possible to add a `page[limit]` parameter in the query-string to paginate (
cf [JSON-API pagination](./http-api.md#pagination)). A `page[skip]` parameter in
the query-string is also supported, but CouchDB may be slow on requests with a
skip on large collections.

#### Request

```http
GET /instances HTTP/1.1
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
      "type": "instances",
      "id": "3af6ed68a6d9146b3529d2584a001d98",
      "attributes": {
        "domain": "alice.cozy.localhost:8080",
        "prefix": "cozy7d3d5947e7f3b0c674d1b8644646348e",
        "locale": "fr",
        "context": "dev",
        "onboarding_finished": true,
        "indexes_version": 30
      },
      "meta": {
        "rev": "1-32c855c989e8f6def0bc0cc417d8b3b4"
      },
      "links": {
        "self": "/instances/3af6ed68a6d9146b3529d2584a001d98"
      }
    },
    {
      "type": "instances",
      "id": "3af6ed68a6d9146b3529d2584a01d557",
      "attributes": {
        "domain": "bob.cozy.localhost:8080",
        "prefix": "cozybf682065ca3c7d64f2dafc6cc12fe702",
        "locale": "fr",
        "context": "dev",
        "onboarding_finished": true,
        "indexes_version": 30
      },
      "meta": {
        "rev": "1-ab6f77dbfdb3aab5b70b022e37fe231f"
      },
      "links": {
        "self": "/instances/3af6ed68a6d9146b3529d2584a01d557"
      }
    }
  ],
  "meta": {
    "count": 2
  }
}
```

### GET /instances/count

Returns the count of all instances.

#### Request

```http
GET /instances/count HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{ "count": 259 }
```

### GET /instances/:domain/last-activity

It returns an approximate date of when the instance was last used by their
owner (automatic jobs like connectors don't count). It looks at the sessions
and OAuth tokens.

#### Request

```http
GET /instances/john.mycozy.cloud/last-activity HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "last-activity": "2022-12-31"
}
```

### PATCH /instances/:domain

Updates an instance's properties. All parameters are optional and passed as
query string; only the ones provided are updated.

#### Parameters (query string)

| Parameter            | Type    | Description                                                                 |
| -------------------- | ------- | --------------------------------------------------------------------------- |
| `OldDomain`          | string  | Previous domain of the instance (migration)                                 |
| `Locale`             | string  | Locale (e.g. `fr`, `en`)                                                    |
| `UUID`               | string  | Instance UUID identifier                                                    |
| `OIDCID`             | string  | Identifier for OIDC authentication                                          |
| `FranceConnectID`    | string  | FranceConnect identifier                                                    |
| `TOSSigned`          | string  | Signed TOS version                                                          |
| `TOSLatest`          | string  | Latest TOS version                                                          |
| `Timezone`           | string  | Timezone (e.g. `Europe/Paris`)                                              |
| `ContextName`        | string  | Context name                                                                |
| `Email`              | string  | Owner email address                                                         |
| `PublicName`         | string  | Display name                                                                |
| `Phone`              | string  | Phone number                                                                |
| `Settings`           | string  | Settings (e.g. `offer:premium`)                                             |
| `BlockingReason`     | string  | Blocking reason code (e.g. `PAYMENT_FAILED`, `LOGIN_FAILED`)                |
| `DomainAliases`      | string  | Domain aliases, comma-separated                                             |
| `Sponsorships`       | string  | Sponsorships, comma-separated                                               |
| `DiskQuota`          | integer | Disk quota in bytes                                                         |
| `OnboardingFinished` | bool    | `true` or `false` — whether onboarding is finished                          |
| `MagicLink`          | bool    | `true` or `false` — enable magic links via email                            |
| `Blocked`            | bool    | `true` or `false` — block or unblock the instance                           |
| `FromCloudery`       | bool    | `true` to indicate the update comes from Cloudery (skips Cloudery callback) |
| `Deleting`           | bool    | `true` or `false` — mark the instance as being deleted                      |
| `Debug`              | bool    | _Deprecated_ — kept for compatibility                                       |

#### Request example

```http
PATCH /instances/john.mycozy.cloud?Email=john@example.com&FromCloudery=true HTTP/1.1
```

To block an instance with a reason:

```http
PATCH /instances/john.mycozy.cloud?Blocked=true&BlockingReason=PAYMENT_FAILED HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
```

```json
{
  "data": {
    "type": "instances",
    "id": "0dc76ad9b1cf3a979b916b3155001830",
    "attributes": {
      "domain": "john.mycozy.cloud",
      "prefix": "cozy1a4e1aabf424a194d7daf946d7b1337d",
      "locale": "fr",
      "context": "cozy",
      "onboarding_finished": true,
      "indexes_version": 31,
      "passphrase_hash": "c2NyeXB0JDMyNzY4JDgkMSQyYjMzMTU1YTY5OTdjYzM2ZjQyYjk1MWM0MWU4ZWVkYSRlODA4NTY2ODQ5OTdkOWNmNzc1ZGEzMjdlYWMwOTgyNTMwMTM3NTJjYTMxMTdhYTIyYTIxODI0NzBmODhjYjdl",
      "session_secret": "eyG2l+G1xO38WyD1GfqYkgSU/T4rnti+JzOwj6haHpM8PSMvzkGu/CSH0mpXUuuCNVbjEXc+hRwGMJ8lTKqs+w==",
      "oauth_secret": "tnr6V8jDK27CDVpzNiOOAJZs+5wrvGyNyJxIc/BJ6O87i2eJX4LCzblDFyDbVv/B7qV7HA9/Fc+Agon2gHQg8x0E0zzfGizbFeWt+KPk7UrZNd4sZJ81oWNNd9BrJ2+eKXDmZeYBI0AwUSykyr7iOIpB5jXaIvfOQvH7EYwtKLg=",
      "cli_secret": "dcLa1VqcoI4eNE7nBrFzhJ9w6rRLlAMESl3PAEqr+IDE29OeN3uyhzLhxPlk0b9rkc0yvozQc/AFttZxyqH/DDYa6rrJyrf91gddtwSfka1pJVss+/DiFaghyWJzEbffBs78X3swA2gSJNu0eGDKdFVY7q8iLT4JpfXy+GPzdLk="
    },
    "meta": {
      "rev": "1-1c4efe4196191469a65b2a3e0898db61"
    },
    "links": {
      "self": "/instances/0dc76ad9b1cf3a979b916b3155001830"
    }
  }
}
```

### GET /instances/with-app-version/:slug/:version

Returns all the instances using slug/version pair

#### Request

```http
GET /instances/with-app-version/drive/1.0.0 HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "instances": [
    "alice.cozy.localhost",
    "bob.cozy.localhost",
    "zoe.cozy.localhost"
  ]
}
```

### POST /instances/:domain/magic_link

Creates a code that can be used in a magic link (if this feature is enabled on
the Cozy).

#### Request

```http
POST /instances/alice.cozy.localhost/magic_link HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "code": "YmU5NmEzMzAtYjZiYy0wMTNiLTE1YzUtMThjMDRkYWJhMzI2"
}
```

### POST /instances/:domain/session_code

Creates a session_code that can be used to login on the given instance.

#### Request

```http
POST /instances/alice.cozy.localhost/session_code HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "session_code": "L7oJ6BDQtdbLR5Vr5vTxTXLJ1pQzMXcD"
}
```

### POST /instances/:domain/session_code/check

Checks that a session_code is valid for the given instance. Note that the
session_code will be invalidated after that.

#### Request

```http
POST /instances/alice.cozy.localhost/session_code/check HTTP/1.1
Content-Type: application/json
```

```json
{
  "session_code": "L7oJ6BDQtdbLR5Vr5vTxTXLJ1pQzMXcD"
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "valid": true
}
```

### POST /instances/:domain/email_verified_code

Creates an email_verified_code that can be used on the given instance to avoid
the 2FA by email.

#### Request

```http
POST /instances/alice.cozy.localhost/email_verified_code HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "email_verified_code": "jBPF5Kvpv1oztdaSgdA2315hVpAf6BCd"
}
```

Note: if the two factor authentication by email is not enabled on this
instance, it will return a 400 Bad Request error.

### DELETE /instances/:domain/sessions

Delete the databases for io.cozy.sessions and io.cozy.sessions.logins.

#### Request

```http
DELETE /instances/alice.cozy.localhost/sessions HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /instances/:domain/fixers/content-mismatch

Fixes the 64k (or multiple) content mismatch files of an instance

#### Request

```http
POST /instances/alice.cozy.localhost/fixers/content-mismatch HTTP/1.1
Content-Type: application/json
```

```json
{
  "dry_run": true
}
```

The `dry_run` (default to `true`) body parameter tells if the request is a
dry-run or not.

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "dry_run": true,
  "updated": [
    {
      "filepath": "/file64.txt",
      "id": "3c79846513e81aee78ab30849d006550",
      "created_at": "2019-07-30 15:05:27.268876334 +0200 CEST",
      "updated_at": "2019-07-30 15:05:27.268876334 +0200 CEST"
    }
  ],
  "removed": [
    {
      "filepath": "/.cozy_trash/file64.txt-corrupted",
      "id": "3c79846513e81aee78ab30849d001f98",
      "created_at": "2019-07-30 10:18:28.826400117 +0200 CEST",
      "updated_at": "2019-07-30 14:32:29.862882247 +0200 CEST"
    }
  ],
  "domain": "alice.cozy.localhost"
}
```

### POST /instances/:domain/fixers/password-defined

Fill the `password_defined` field of the io.cozy.settings.instance if it was
missing.

#### Request

```http
POST /instances/alice.cozy.localhost/fixers/password-defined HTTP/1.1
```

### POST /instances/:domain/fixers/orphan-account

Delete the accounts which are not linked to a konnector

#### Request

```http
POST /instances/alice.cozy.localhost/fixers/orphan-account HTTP/1.1
```

### POST /instances/:domain/export

Starts an export for the given instance. The CouchDB documents will be saved in
an intermediary archive while the files won't be added until the data is
actually downloaded.

The response contains the details of the scheduled export job.

#### Query-String

| Parameter | Description                                                                                 |
| --------- | ------------------------------------------------------------------------------------------- |
| admin-req | Boolean indicating when the request is made by an admin and the user should not be notified |

The admin-req parameter is optional: by default, the instance's owner will be
notified via e-mail, whether the export is successful or not. If it's
successful, the e-mail will contain a link to the Settings app allowing the user
to download the data archives.
When this parameter is `true`, no e-mails will be sent and the admin will be
able to get the export document via realtime events.

#### Request

```http
POST /instances/alice.cozy.localhost/export?admin-req=true HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 Accepted
Content-Type: application/json
```

```json
{
  "_id": "123123",
  "_rev": "1-58d2b368da0a1b336bcd18ced210a8a1",
  "domain": "alice.cozy.localhost",
  "prefix": "cozyfdd8fd8eb825ad98821b11871abf58c9",
  "worker": "export",
  "message": {
    "parts_size": 0,
    "max_age": 0,
    "contextual_domain": "alice.cozy.localhost",
    "admin_req": true
  },
  "event": null,
  "state": "queued",
  "queued_at": "2023-02-01T11:50:59.286530525+01:00",
  "started_at": "0001-01-01T00:00:00Z",
  "finished_at": "0001-01-01T00:00:00Z"
}
```

### GET /instances/:domain/exports/:export-id/data

This endpoint will return an archive containing the metadata and files of the
user, as part of a multi-part response.

Only the first part of the archive contains the metadata.

#### Query-String

| Parameter | Description                                                         |
| --------- | ------------------------------------------------------------------- |
| cursor    | String reprentation of the export cursor to start the download from |

The cursor parameter is optional but any given cursor should be one of the
defined `parts_cursors` in the export document.
To get all the parts, this endpoint must be called one time with no cursors, and
one time for each cursor in `parts_cursors`.

#### Request

```http
GET /instances/alice.cozy.localhost/exports/123123/data?cursor=io.cozy.files%2Fa27b3bae83160774a74525de670d5d8e HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/zip
Content-Disposition: attachment; filename="alice.cozy.localhost - part001.zip"
```

### POST /instances/:domain/notifications

This endpoint allows to send a notification via the notification center. Both
the notification declaration and its properties need to be passed in the body.
These notifications cannot use templates defined in cozy-stack though so their
e-mail content must be provided directly (at least in HTML).

When the request is successful, the generated notification object is returned.

```http
POST /instances/alice.cozy.localhost/notifications HTTP/1.1
Authorization: Bearer ...
Content-Type: application/json
```

```json
{
  "notification": {
    "category": "account-balance",
    "category_id": "my-bank",
    "title": "Your account balance is not OK",
    "message": "Warning: we have detected a negative balance in your my-bank",
    "priority": "high",
    "state": "-1",
    "preferred_channels": ["mobile"],
    "content": "Hello,\r\nWe have detected a negative balance in your my-bank account.",
    "content_html": "<html>\r\n\t<body>\r\n\t<p>Hello,<br/>We have detected a negative balance in your my-bank account.</p>\r\n\t</body>\r\n\t</html>"
  },
  "properties": {
    "description": "Alert the user when its account balance is negative",
    "collapsible": true,
    "multiple": true,
    "stateful": true,
    "default_priority": "high"
  }
}
```

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/json
```

```json
{
  "_id": "c57a548c-7602-11e7-933b-6f27603d27da",
  "_rev": "1-1f2903f9a867",
  "source_id": "cozy/cli//account-balance/my-bank",
  "originator": "cli",
  "category": "account-balance",
  "category_id": "my-bank",
  "created_at": "2024-01-04T15:23:01.832Z",
  "last_sent": "2024-01-04T15:23:01.832Z",
  "title": "Your account balance is not OK",
  "message": "Warning: we have detected a negative balance in your my-bank",
  "priority": "high",
  "state": "-1",
  "content": "Hello,\r\nWe have detected a negative balance in your my-bank account.",
  "contentHTML": "<html>\r\n\t<body>\r\n\t<p>Hello,<br/>We have detected a negative balance in your my-bank account.</p>\r\n\t</body>\r\n\t</html>"
}
```

## RAG indexing

The RAG indexing of an instance follows the knowledge base folders of its
assistants (see [ai.md](ai.md)): there is one checkpoint for the instance,
and no per-folder trigger any more. These routes are operator tools.

Only `reset` runs under the lock the `rag-index` jobs take (`purge` takes it
just to drop the checkpoint, after having emptied openRAG). Run `prune` and
`purge` when no `rag-index` job is running, or they may fight with it.

### POST /instances/:domain/rag/reset

Deletes the `rag-index` checkpoint of the instance and launches a `rag-index`
job, so the whole changes feed is scanned again by the RAG indexer.

#### Request

```http
POST /instances/alice.cozy.localhost/rag/reset HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 No Content
```

### POST /instances/:domain/rag/reconcile

Pushes a reconcile job per knowledge base folder: the job walks the subtree
of the folder and indexes every file it holds. With the `dir_id` query
parameter, only that folder is reconciled. It is how an initial indexing that
did not finish is restarted. `dir_id` is the folder id, the one the
assistant's `knowledgeBase` carries (`io.cozy.files.root-dir` for the whole
Drive), not the id of its openRAG workspace.

#### Request

```http
POST /instances/alice.cozy.localhost/rag/reconcile?dir_id=6c36a9ee HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "jobs": 1
}
```

A `404 Not Found` is returned when the given folder is in no assistant's
knowledge base.

### POST /instances/:domain/rag/prune

Deletes from openRAG what no knowledge base folder claims any more: the files
gone or trashed in the Cozy, the files outside every knowledge base folder,
and the workspaces of the folders no assistant uses. The index status of a
deleted file is dropped too. A workspace is named after its folder id, except
for the well-known folders: openRAG refuses an id with dots, so the root
folder (`io.cozy.files.root-dir`) is indexed in the workspace
`io-cozy-files-root-dir`, and likewise for the trash, "Shared with me", "No
longer shared" and "Shared drives".

#### Request

```http
POST /instances/alice.cozy.localhost/rag/prune HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "files_scanned": 12,
  "files_deleted": 3,
  "workspaces_deleted": 1
}
```

### POST /instances/:domain/rag/purge

Deletes everything openRAG holds for the instance: its workspaces, its files
and its partition. The index statuses and the `rag-index` checkpoint are
dropped too, so the next run indexes the whole instance again (use the reset
route above to launch it right away).

#### Request

```http
POST /instances/alice.cozy.localhost/rag/purge HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 No Content
```

## Contexts

### GET /instances/contexts

This endpoint returns the list of the contexts, with their name, config,
registries, office server, cloudery endpoints, and OIDC data.

#### Request

```
GET /instances/contexts HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
```

```json
[
  {
    "config": {
      "claudy_actions": ["desktop", "mobile", "support"],
      "debug": true,
      "features": [
        {
          "foo": "bar"
        },
        {
          "baz": ["qux", "quux"]
        }
      ],
      "help_link": "https://forum.cozy.io/",
      "noreply_address": "noreply@cozy.beta",
      "noreply_name": "My Cozy Beta",
      "sharing_domain": "cozy.localhost"
    },
    "context": "dev",
    "registries": ["https://apps-registry.cozycloud.cc/"],
    "office": {
      "OnlyOfficeURL": "https://documentserver.cozycloud.cc/"
    },
    "cloudery_endpoint": "",
    "oidc": {
      "allow_oauth_token": false,
      "authorize_url": "https://identity-prodiver/path/to/authorize",
      "client_id": "aClientID",
      "id_token_jwk_url": "https://identity-prodiver/path/to/jwk",
      "login_domain": "login.mycozy.cloud",
      "redirect_uri": "https://oauthcallback.mycozy.cloud/oidc/redirect",
      "scope": "openid profile",
      "token_url": "https://identity-prodiver/path/to/token",
      "userinfo_instance_field": "cozy_number",
      "userinfo_instance_prefix": "name",
      "userinfo_instance_suffix": ".mycozy.cloud",
      "userinfo_url": "https://identity-prodiver/path/to/userinfo"
    }
  }
]
```

### GET /instances/contexts/:name

This endpoint returns the config of a given context.

#### Request

```
GET /instances/contexts/dev HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
```

```json
{
  "config": {
    "claudy_actions": ["desktop", "mobile", "support"],
    "debug": true,
    "features": [
      {
        "foo": "bar"
      },
      {
        "baz": ["qux", "quux"]
      }
    ],
    "help_link": "https://forum.cozy.io/",
    "noreply_address": "noreply@cozy.beta",
    "noreply_name": "My Cozy Beta",
    "sharing_domain": "cozy.localhost"
  },
  "context": "dev",
  "registries": ["https://apps-registry.cozycloud.cc/"],
  "office": {
    "OnlyOfficeURL": "https://documentserver.cozycloud.cc/",
    "InboxSecret": "inbox_secret",
    "OutboxSecret": "outbox_secret"
  },
  "cloudery_endpoint": ""
}
```

## Checkers

### GET /instances/:domain/fsck

This endpoint can be use to check the VFS of a given instance. It accepts three
possible parameters in the query-string:

- `IndexIntegrity=true` to check only the integrity of the data in CouchDB
- `FilesConsistency` to check the consistency between CouchDB and Swift
- `FailFast` to abort on the first error.

It will returns a `200 OK`, except if the instance is not found where the code
will be `404 Not Found` (a `5xx` can also happen in case of server errors like
CouchDB not available). The format of the response will be one JSON per line,
and each JSON represents an error.

#### Request

```http
GET /instances/alice.cozy.localhost/fsck HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
```

```json
{"type":"index_orphan_tree","dir_doc":{"type":"directory","_id":"34a61c6ceb38075fe971cc6a3263659f","_rev":"2-94ca3acfebf927cb231d125c57f85bd7","name":"Photos","dir_id":"45496c5c442dabecae87de3d73008ec4","created_at":"2020-12-15T18:23:21.498323965+01:00","updated_at":"2020-12-15T18:23:21.498323965+01:00","tags":[],"path":"/Photos","cozyMetadata":{"doctypeVersion":"1","metadataVersion":1,"createdAt":"2020-12-15T18:23:21.498327603+01:00","updatedAt":"2020-12-15T18:23:21.498327603+01:00","createdOn":"http://alice.cozy.localhost:8080/"},"size":"0","is_dir":true,"is_orphan":true,"has_cycle":false},"is_file":false,"is_version":false}
{"type":"index_missing","file_doc":{"type":"file","name":"Photos","dir_id":"","created_at":"2020-12-15T18:23:21.527308795+01:00","updated_at":"2020-12-15T18:23:21.527308795+01:00","tags":null,"path":"/Photos","size":"4096","mime":"application/octet-stream","class":"files","executable":true,"is_dir":false,"is_orphan":false,"has_cycle":false},"is_file":true,"is_version":false}
```

### POST /instances/:domain/checks/triggers

This endpoint will check if no trigger has been installed twice (or more).

#### Request

```http
POST /instances/alice.cozy.localhost/checks/triggers HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
[
  {
    "_id": "45496c5c442dabecae87de3d7300666f",
    "arguments": "io.cozy.files:CREATED,UPDATED,DELETED:image:class",
    "debounce": "",
    "other_id": "34a61c6ceb38075fe971cc6a3263895f",
    "trigger": "@event",
    "type": "duplicate",
    "worker": "thumbnail"
  }
]
```

### POST /instances/:domain/checks/shared

This endpoint will check that the io.cozy.shared documents have a correct
revision tree (no generation smaller for a children than its parent).

#### Request

```http
POST /instances/alice.cozy.localhost/checks/shared HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
[
  {
    "_id": "io.cozy.files/fd1706de234d17d1ac2fe560051a2aae",
    "child_rev": "1-e19947b4f9bfb273bc8958ae932ae4c7",
    "parent_rev": "2-4f82af35577dbc9b686dd447719e4835",
    "type": "invalid_revs_suite"
  },
  {
    "_id": "io.cozy.files/fd9bef5df406f5b150f302b8c5b3f5f0",
    "child_rev": "7-05bb459e0ac5450c17df79ed1f13afa1",
    "parent_rev": "8-07b4cafef3c2e74e698ee4a04d1874c2",
    "type": "invalid_revs_suite"
  }
]
```

### POST /instances/:domain/checks/sharings

This endpoint can be used to check the setup of sharings owned by a given
instance and the consistency of the shared files and folders with their
counterparts on the Cozy they're shared with. It accepts one parameter in the
query-string:

- `Fast` to skip the files and folders consistency check as it can be quite long

It will return a `200 OK`, except if the instance is not found where the code
will be `404 Not Found` (a `5xx` can also happen in case of server errors like
CouchDB not available). The format of the response will be a JSON array of
objects, each object representing an error.

#### Possible error types

##### invalid_rules

This will be raised when the owner's sharing rules are invalid.
The validation result will be returned in the `error` attribute.

##### sharing_in_sharing

This will be raised when the root of the sharing being checked is part of
another sharing (i.e. one of its parent folders is shared). The parent
sharing can be found either on the owner's instance or on one of its members'
instance.
The `parent_sharing` attribute will contain the parent sharing ID.

##### missing_matching_docs_for_owner

This will be raised if the shared files and folders associated with the sharing
could not be fetched on the owner's instance.
The request error will be returned in the `error` attribute.
No further consistency checks will be run on this sharing.

##### missing_sharing_for_member

This will be raised when the associated `io.cozy.sharing` document cannot be
found on a sharing member's instance.
The request error will be returned in the `error` attribute.
No further consistency checks will be run for this member.

##### missing_files_rule_for_member

This will be raised if a member's sharing doesn't have any sharing rule for
`io.cozy.files` documents (while the owner's sharing has been determined to be
of this type).
The member's domain will be available in the `member` attribute.
No further consistency checks will be run for this member.

##### missing_matching_docs_for_member

This will be raised if the shared files and folders associated with the sharing
could not be fetched on a member's instance.
The request error will be returned in the `error` attribute and the member's
domain in the `member` attribute. No further consistency checks will be run for
this member.

##### disk_quota_exceeded

This will be raised if a file is outdated or missing on an instance and its size
is larger than the available space on this instance. If the file was created or
modified in the last 5 minutes, the check is skipped as we expect the
synchronization to happen later.
The instance's domain will be available in the `instance` attribute.

##### read_only_member

This will be raised if a file or folder is outdated or missing on the owner's
instance and the member being checked has only read-only access to the sharing.
This is not really an error as this behavior is expected but since it can be
confusing for users we log it for debugging purposes.
The member's domain will be available in the `member` attribute.

##### invalid_doc_rev

This will be raised if a file or folder's revisions don't match on the owner's
and member's instances while the member has write access to the sharing and the
file size is not greater than the outdated instance's available disk space. The
revisions generations can be the same too. If the document was modified in the
last 5 minutes, the check is skipped as we expect the synchronization to happen
later.
The member's domain will be available in the `member` attribute, as well as the
revision of the member document in `memberRev` and the complete owner document
in `ownerDoc`.

##### invalid_doc_name

This will be raised if a file or folder's revisions match on the owner's and
member's instances but their names don't.
The member's domain will be available in the `member` attribute, as well as the
name of the member document in `memberName` and the complete owner document
in `ownerDoc`.

##### invalid_doc_checksum

This will be raised if a file's revisions match on the owner's and member's
instances but their checksums don't.
The member's domain will be available in the `member` attribute, as well as the
checksum of the member file in `memberChecksum` and the complete owner document
in `ownerDoc`.

##### invalid_doc_parent

This will be raised if a file or folder's parent directories don't match on the
owner's and member's instances but their names don't. Since it is expected for
the sharing root to not have different parents on each instance, the check does
not apply to this document.
The member's domain will be available in the `member` attribute, as well as the
id of the member's parent directory in `memberParent` and the complete owner
document in `ownerDoc`.

##### missing_matching_doc_for_owner

This will be raised if a file or directory is missing on the owner's instance
while the member being check has write access to the sharing and the file size
is not greater than the instance's available disk space. If the missing document
was created or modified in the last 5 minutes, the check is skipped as we expect
the synchronization to happen later.
The member's domain will be available in the `member` attribute and the complete
missing member document in `memberDoc`.

##### missing_matching_doc_for_member

This will be raised if a file or directory is missing on a member's instance
while the file size is not greater than the instance's available disk space. If
the missing document was created or modified in the last 5 minutes, the check is
skipped as we expect the synchronization to happen later.
The member's domain will be available in the `member` attribute and the complete
missing owner document in `ownerDoc`.

===

Other error types include `missing_trigger_on_active_sharing`,
`trigger_on_inactive_sharing`, `not_enough_members`, `mail_not_sent`,
`invalid_member_status`, `invalid_instance_for_member`,
`missing_instance_for_member`, `missing_oauth_client`, `missing_access_token`,
`invalid_number_of_credentials` and `missing_inbound_client_id`.

When checking for files and folders inconsistencies, sharings will be skipped
when inactive, not initialized, read-only or not about `io.cozy.files`
documents.
Also, for each instance, only the sharings owned by said instance will be
checked. Other sharings will be checked via their owner instance.

#### Request

```http
POST /instances/alice.cozy.localhost/checks/sharings HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
[
  {
    "id": "314d69d7ebaed0a1870cca67f4433390",
    "trigger": "track",
    "trigger_id": "314d69d7ebaed0a1870cca67f4d75e41",
    "type": "trigger_on_inactive_sharing"
  },
  {
    "id": "314d69d7ebaed0a1870cca67f4433390",
    "trigger": "replicate",
    "trigger_id": "314d69d7ebaed0a1870cca67f4d75e41",
    "type": "trigger_on_inactive_sharing"
  },
  {
    "id": "314d69d7ebaed0a1870cca67f4433390",
    "trigger": "upload",
    "trigger_id": "314d69d7ebaed0a1870cca67f4d75e41",
    "type": "trigger_on_inactive_sharing"
  },
  {
    "id": "314d69d7ebaed0a1870cca67f4433390",
    "member": 0,
    "status": "revoked",
    "type": "invalid_member_status"
  },
  {
    "id": "314d69d7ebaed0a1870cca67f4433390",
    "nb_members": 0,
    "owner": false,
    "type": "invalid_number_of_credentials"
  }
]
```

## Konnectors

### GET /konnectors/maintenance

#### Request

```http
GET /konnectors/maintenance HTTP/1.1
```

A parameter `Context` can be given on the query string to also includes the
konnectors that are in maintenance on the apps registry.

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "meta": {
    "count": 1
  },
  "data": [
    {
      "type": "io.cozy.konnectors.maintenance",
      "attributes": {
        "level": "stack",
        "maintenance_activated": true,
        "maintenance_options": {
          "flag_disallow_manual_exec": false,
          "flag_infra_maintenance": true,
          "flag_short_maintenance": true,
          "messages": {
            "fr": {
              "long_message": "Bla bla bla",
              "short_message": "Bla"
            }
          }
        },
        "slug": "ameli",
        "type": "konnector"
      }
    }
  ]
}
```

**Note:** `level` can be `stack` or `registry`.

### PUT /konnectors/maintenance/:slug

#### Request

```http
PUT /konnectors/maintenance/ameli HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "flag_short_maintenance": true,
      "flag_disallow_manual_exec": false,
      "messages": {
        "fr": {
          "long_message": "Bla bla bla",
          "short_message": "Bla"
        },
        "en": {
          "long_message": "Yadi yadi yada",
          "short_message": "Yada"
        }
      }
    }
  }
}
```

**Note:** the `flag_infra_maintenance` will always be set to true with this
endpoint.

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /konnectors/maintenance/:slug

#### Request

```http
DELETE /konnectors/maintenance/ameli HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 No Content
```

## OIDC

### POST /oidc/:context/:provider/code

This endpoint is used by the cloudery to create a delegated code, which will be
then used by the flagship app to obtain its access_token and refresh_token. The
`:provider` parameter can be `generic` or `franceconnect`.

The cloudery sends its access_token for the OIDC provider, the stack can use it
to make a request to the userinfo endpoint of the OIDC provider. With the
response, the stack can create a delegated code associated to the sub.

If an `id_token` is also provided, the stack extracts the session ID (`sid`)
claim and stores it with the delegated code. This session ID is later used for
OIDC logout: when the user signs out of the flagship app, the stack calls the
OpenID provider's `end_session_endpoint` to terminate the upstream SSO session.

```http
POST /oidc/dev/franceconnect/code HTTP/1.1
Accept: application/json
Content-Type: application/json
Authorization: Bearer ZmE2ZTFmN
```

```json
{
  "access_token": "ZmE2ZTFmN",
  "id_token": "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "delegated_code": "wMTNiLTNmZWItMThY",
  "sub": "DIzYWE2MjA",
  "email": "jerome@example.org"
}
```

## OAuth clients

### DELETE /oauth/:domain/clients

Delete all the OAuth clients for the given instance. It can be limited to a
specific kind of clients (`desktop`, `mobile`, `sharing`, etc.) by using the
`Kind` parameter of the query-string. It returns the number of clients that
have been deleted.

#### Request

```http
DELETE /oauth/cozy.example/clients HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
```

```json
{ "count": 42 }
```

## Swift

### GET /swift/layouts

Count swift layouts by type

#### Request

```http
GET /swift/layouts HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "total": 3,
  "unknown": {
    "counter": 0
  },
  "v1": {
    "counter": 1
  },
  "v2a": {
    "counter": 0
  },
  "v2b": {
    "counter": 0
  },
  "v3a": {
    "counter": 2
  },
  "v3b": {
    "counter": 4
  }
}
```

The `show_domains=true` query parameter provides the domain names if needed

#### Request

```http
GET /swift/layouts?show_domains=true HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "total": 3,
  "unknown": {
    "counter": 0
  },
  "v1": {
    "counter": 1,
    "domains": ["bob.cozy.localhost:8081"]
  },
  "v2a": {
    "counter": 0
  },
  "v2b": {
    "counter": 0
  },
  "v3a": {
    "counter": 2,
    "domains": ["alice.cozy.localhost:8081", "ru.cozy.localhost:8081"]
  },
  "v3b": {
    "counter": 4,
    "domains": [
      "foo.cozy.localhost:8081",
      "bar.cozy.localhost:8081",
      "baz.cozy.localhost:8081",
      "foobar.cozy.localhost:8081"
    ]
  }
}
```

### GET /swift/vfs/:object

Retrieves a Swift object

#### Request

```http
GET /swift/vfs/67a88b22520680b1fae840%2F9a8a0%2F18d02%2FiYbkfuCDEMaVoIXg HTTP/1.1
Host: alice.cozy.localhost
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: text/plain
```

```text
"foobar"
```

### PUT /swift/vfs/:object

Put an object in Swift

#### Request

```http
PUT /swift/vfs/67a88b22520680b1fae840%2F9a8a0%2F18d02%2FiYbkfuCDEMaVoIXg HTTP/1.1
Host: alice.cozy.localhost
Content-Type: text/plain
```

```text
"this is my content"
```

### DELETE /swift/vfs/:object

Removes an object from Swift

#### Request

```http
DELETE /swift/vfs/67a88b22520680b1fae840%2F9a8a0%2F18d02%2FiYbkfuCDEMaVoIXg HTTP/1.1
Host: alice.cozy.localhost
```

### GET /swift/vfs

List Swift objects of an instance

#### Request

```http
GET /swift/vfs HTTP/1.1
Host: alice.cozy.localhost
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "objects_names": [
    "67a88b22520680b1fae840/9a8a0/17264/AxfGhAiWVRhPufKK",
    "67a88b22520680b1fae840/9a8a0/18d02/iYbkfuCDEMaVoIXg",
    "thumbs/67a88b22520680b1fae840/9a8a0/17264-large",
    "thumbs/67a88b22520680b1fae840/9a8a0/17264-medium",
    "thumbs/67a88b22520680b1fae840/9a8a0/17264-small",
    "thumbs/67a88b22520680b1fae840/9a8a0/17264-tiny"
  ]
}
```

## Tools

### GET /tools/pprof/heap

Return a sampling of memory allocations as pprof format.

#### Request

```http
GET /tools/pprof/heap HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
```
