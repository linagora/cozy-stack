[Table of contents](README.md#table-of-contents)

# Shared drives

A shared drive is a federated sharing in `io.cozy.files` between several Cozy
instances. The root can currently be either:

- a directory
- a single file

A member doesn't have the files in their Cozy, but can access them via the
stack playing a proxy role.

The root kind is exposed on the sharing payload with `drive_root_type`:

- `directory`
- `file`

For drives created on an organization instance, the sharing payload also
exposes `org_drive: true`. This is an additive classification flag for clients;
it does not change the underlying drive-sharing behavior.

## Creating a shared drive

There are two ways to create a shared drive:

### Simple method: Use `POST /sharings/drives`

Use the [`POST /sharings/drives`](#post-sharingsdrives) endpoint either to:

- convert an existing file or directory into a shared drive with `folder_id`
  or `file_id`
- create a brand new directory-root shared drive with `name`

This is the recommended approach as it handles validation and setup
automatically.

### Manual method

To create a shared drive manually (typically on the organization Cozy), follow
these steps:

1. Ensure that the `/Drives` folder exists in the cozy instance with the
   [`POST /files/shared-drives`](https://docs.cozy.io/en/cozy-stack/files/#post-filesshared-drives)
   route.
2. Create the root resource to share:
   - either a folder inside it, with the name of shared drive
   - or a file that will become the root of the shared drive
3. Create a sharing with the `drive: true` attribute, and one rule for
   the shared root (with `none` for `add`, `update` and `remove` attributes).

## Managing shared drives

### GET /sharings/drives

The `GET /sharings/drives` route returns the list of shared drives.

When a drive was created on an organization instance, its attributes include
`org_drive: true`.

#### Request

```http
GET /sharings/drives HTTP/1.1
Host: acme.example.net
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
      "id": "aae62886e79611ef8381fb83ff72e425",
      "attributes": {
        "drive": true,
        "drive_root_type": "directory",
        "org_drive": true,
        "owner": true,
        "description": "Drive for the product team",
        "app_slug": "drive",
        "created_at": "2025-02-10T11:08:08Z",
        "updated_at": "2025-02-10T12:10:43Z",
        "members": [
          {
            "status": "owner",
            "public_name": "ACME",
            "email": "admin@acme.example.net",
            "instance": "acme.example.net"
          },
          {
            "status": "pending",
            "name": "Alice",
            "email": "alice@example.net"
          },
          {
            "status": "pending",
            "name": "Bob",
            "email": "bob@example.net"
          }
        ],
        "rules": [
          {
            "title": "Product team",
            "doctype": "io.cozy.files",
            "values": [
              "357665ec-e797-11ef-94fb-f3d08ccb3ff5"
            ],
            "add": "none",
            "update": "none",
            "remove": "none"
          }
        ]
      },
      "meta": {
        "rev": "1-272ba74b868f"
      },
      "links": {
        "self": "/sharings/aae62886e79611ef8381fb83ff72e425"
      }
    }
  ]
}
```

### POST /sharings/drives

Creates a new shared drive. The endpoint supports three mutually exclusive
modes:

- pass `folder_id` to convert an existing file or directory into a shared drive
- pass `file_id` to convert an existing file or directory into a shared drive
- pass `name` to create a new directory under the Shared Drives root and share
  it

If the target Cozy is an organization instance, the created sharing is also
marked with `org_drive: true`.

`folder_id` is the legacy root identifier alias. `file_id` is the preferred
identifier alias for clients migrating to the new API. In both cases, the
actual root kind is determined from the referenced `io.cozy.files` document and
is exposed back as `drive_root_type`.

When an existing root is used through `folder_id` or `file_id`, that root must:

- Exist
- Not already have a sharing itself
- Not be inside another shared folder

Additional rules for directory roots:

- Must not be a system folder (`root`, `trash`, `shared-with-me`,
  `shared-drives`, `no-longer-shared`)
- Must not be inside the trash
- Must not contain any subfolder that already has a sharing

Additional rules for file roots:

- Must not be trashed

#### Request

```http
POST /sharings/drives HTTP/1.1
Host: acme.example.net
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "attributes": {
      "description": "Project Documents",
      "folder_id": "357665ec-e797-11ef-94fb-f3d08ccb3ff5"
    },
    "relationships": {
      "recipients": {
        "data": [
          {"id": "contact-id-1", "type": "io.cozy.contacts"},
          {"id": "group-id-1", "type": "io.cozy.contacts.groups"}
        ]
      },
      "read_only_recipients": {
        "data": [
          {"id": "contact-id-2", "type": "io.cozy.contacts"}
        ]
      }
    }
  }
}
```

Or create a brand new shared drive directly:

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "attributes": {
      "name": "Product Team"
    }
  }
}
```

Or create a file-root shared drive explicitly:

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "attributes": {
      "description": "Quarterly report",
      "file_id": "357665ec-e797-11ef-94fb-f3d08ccb3ff5"
    }
  }
}
```

**Attributes:**

| Attribute     | Required | Description |
|---------------|----------|-------------|
| `folder_id`   | No       | Legacy root ID alias. Can reference an existing file or directory to convert into a shared drive |
| `file_id`     | No       | Preferred root ID alias. Can reference an existing file or directory to convert into a shared drive |
| `name`        | No       | The name of the directory to create under Shared Drives for a new directory-root shared drive |
| `description` | No       | A description for the shared drive. If not provided, defaults to the root resource name |

Exactly one of `folder_id`, `file_id`, or `name` must be provided.

**Relationships:**

| Relationship           | Description |
|------------------------|-------------|
| `recipients`           | Contacts or groups to add as read-write members |
| `read_only_recipients` | Contacts or groups to add as read-only members |

Both relationships are optional - you can create a shared drive without any recipients
and add them later.

#### Response (Success)

```http
HTTP/1.1 201 Created
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.sharings",
    "id": "aae62886e79611ef8381fb83ff72e425",
    "attributes": {
      "drive": true,
      "drive_root_type": "directory",
      "owner": true,
      "description": "Project Documents",
      "app_slug": "drive",
      "created_at": "2025-02-10T11:08:08Z",
      "updated_at": "2025-02-10T11:08:08Z",
      "members": [
        {
          "status": "owner",
          "public_name": "ACME",
          "email": "admin@acme.example.net",
          "instance": "acme.example.net"
        },
        {
          "status": "pending",
          "name": "Alice",
          "email": "alice@example.net"
        }
      ],
      "rules": [
        {
          "title": "Project Documents",
          "doctype": "io.cozy.files",
          "values": [
            "357665ec-e797-11ef-94fb-f3d08ccb3ff5"
          ],
          "add": "none",
          "update": "none",
          "remove": "none"
        }
      ]
    },
    "meta": {
      "rev": "1-272ba74b868f"
    },
    "links": {
      "self": "/sharings/aae62886e79611ef8381fb83ff72e425"
    }
  }
}
```

#### Error Responses

| Status | Error | Description |
|--------|-------|-------------|
| 400    | Bad Request | Invalid JSON body |
| 403    | Forbidden | Insufficient permissions to create a sharing |
| 404    | Not Found | The file or directory with the given `folder_id` or `file_id` does not exist |
| 409    | Conflict | The root already has a sharing, is inside a shared folder, contains a shared subfolder, or the new `name` already exists in Shared Drives |
| 400    | Bad Request | Invalid request: missing all of `folder_id`, `file_id`, and `name`, conflicting attributes, invalid root type, system folder, trashed root, or invalid `name` |

**Example error (root already shared):**

```http
HTTP/1.1 409 Conflict
Content-Type: application/vnd.api+json
```

```json
{
  "errors": [
    {
      "status": "409",
      "title": "Conflict",
      "detail": "Folder already has an existing sharing"
    }
  ]
}
```

## Files and directories

Unless stated otherwise, a permission on the whole `io.cozy.files` doctype is
required to use the following routes.

For file-root shared drives (`drive_root_type = file`), iteration 1 currently
supports only file-shaped routes. Directory-only routes return
`422 Unprocessable Entity`. `_changes` and realtime are available with exact
root-file semantics for file-root shared drives.

### GET /sharings/drives/:id/download/:file-id

Download a file via a drive share.

Identical call to [`GET /files/download/:file-id`](files.md#get-filesdownloadfile-id) but over a shared drive.
See there for request and response examples

### POST /sharings/drives/:id/downloads

Two-step download of a single file: creates a short-lived secret link (valid 10 minutes) pointing back to the shared drive download endpoint. Useful when the client cannot set an `Authorization` header on the eventual GET (e.g. a plain `<a>` tag).

Identical call to [`POST /files/downloads`](files.md#post-filesdownloads) but over a shared drive. The `links.related` in the response will point to `/sharings/drives/:id/downloads/:secret/:filename` instead of `/files/downloads/...`.

### GET /sharings/drives/:id/downloads/:secret/:fake-name

Download the file prepared by the `POST /sharings/drives/:id/downloads` call above. The `:fake-name` segment is ignored by the server — it exists solely so browsers use it as the suggested save-as filename.

Does not require an `Authorization` header (the secret acts as the credential).

### POST /sharings/drives/:id/archive

Create a temporary zip archive of multiple files and/or folders inside a shared drive. Works for both owners and recipients (including read-only recipients).

This route is supported only for directory-root shared drives. File-root shared
drives return `422 Unprocessable Entity`.

The request body follows the same format as [`POST /files/archive`](files.md#post-filesarchive). The `links.related` in the response points to `/sharings/drives/:id/archive/:secret/:name.zip` instead of `/files/archive/...`.

#### Request

```http
POST /sharings/drives/aae62886e79611ef8381fb83ff72e425/archive HTTP/1.1
Authorization: Bearer token
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "attributes": {
      "name": "project-docs",
      "ids": ["af1e1b66e92111ef8ddd5fbac4938703", "be2f2c77f03222fg9eee6gcbd5049814"]
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
  "links": {
    "related": "/sharings/drives/aae62886e79611ef8381fb83ff72e425/archive/4521DC87/project-docs.zip"
  },
  "data": {
    "type": "io.cozy.archives",
    "id": "4521DC87",
    "attributes": {
      "name": "project-docs",
      "ids": ["af1e1b66e92111ef8ddd5fbac4938703", "be2f2c77f03222fg9eee6gcbd5049814"]
    }
  }
}
```

### GET /sharings/drives/:id/archive/:secret/:fake-name

Download the zip archive prepared by `POST /sharings/drives/:id/archive`. The archive is built on-the-fly and streamed directly — it is not stored on disk.

Does not require an `Authorization` header (the secret acts as the credential, valid 10 minutes).

### GET /sharings/drives/:id/_changes

Get the change feed for a drive.

Identical call to [`GET /files/_changes`](files.md#get-files_changes) but over a shared drive.
See there for request and response examples, differences are the URL and:

- Any item that changed for that owner but isn't under that shared drive
  is presented as a deletion.
- Paths are truncated to the shared drive, and formatted accordingly:

  eg: `//io.cozy.files.shared-drives-dir/1/ba3b516812f636fc022f3968f991357a/Meetings/Checklist.txt`

  Schema and it's version, followed by the shared drive ID, and the path within

For file-root shared drives, this route returns only changes for the root file.
Unrelated file changes are filtered out.

### GET /sharings/drives/:id/:file-id

Get a directory or a file informations inside a shared drive. In the case of a
directory, it contains the list of files and sub-directories inside it. For a
note, its images are included.

#### Request

```http
GET /sharings/drives/aae62886e79611ef8381fb83ff72e425/af1e1b66e92111ef8ddd5fbac4938703 HTTP/1.1
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
    "type": "io.cozy.files",
    "id": "af1e1b66e92111ef8ddd5fbac4938703",
    "meta": {
      "rev": "1-e36ab092"
    },
    "attributes": {
      "type": "directory",
      "name": "Streams",
      "path": "/Product team/Streams",
      "created_at": "2016-09-19T12:35:00Z",
      "updated_at": "2016-09-19T12:35:00Z",
      "tags": [],
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2016-09-20T18:32:47Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2016-09-20T18:32:47Z"
      },
      "driveId": "aae62886e79611ef8381fb83ff72e425"
    },
    "relationships": {
      "contents": {
        "data": [
          {
            "type": "io.cozy.files",
            "id": "a2843318f52411ef8e7ab79eae2f09ab"
          },
          {
            "type": "io.cozy.files",
            "id": "b1db1642f52411efbe0b3bfc5fc0b437"
          }
        ]
      }
    },
    "links": {
      "self": "/files/af1e1b66e92111ef8ddd5fbac4938703"
    }
  },
  "included": [
    {
      "type": "io.cozy.files",
      "id": "a2843318f52411ef8e7ab79eae2f09ab",
      "meta": {
        "rev": "1-ff3beeb456eb"
      },
      "attributes": {
        "type": "directory",
        "name": "Authentication",
        "path": "/Product team/Streams/Authentication",
        "created_at": "2016-09-19T12:35:08Z",
        "updated_at": "2016-09-19T12:35:08Z",
        "cozyMetadata": {
          "doctypeVersion": "1",
          "metadataVersion": 1,
          "createdAt": "2016-09-20T18:32:47Z",
          "createdByApp": "drive",
          "createdOn": "https://cozy.example.com/",
          "updatedAt": "2016-09-20T18:32:47Z"
        }
      }
    },
    {
      "type": "io.cozy.files",
      "id": "b1db1642f52411efbe0b3bfc5fc0b437",
      "meta": {
        "rev": "1-0e6d5b72"
      },
      "attributes": {
        "type": "file",
        "name": "REAMDE.md",
        "trashed": false,
        "md5sum": "ODZmYjI2OWQxOTBkMmM4NQo=",
        "created_at": "2016-09-19T12:38:04Z",
        "updated_at": "2016-09-19T12:38:04Z",
        "tags": [],
        "size": 12,
        "executable": false,
        "class": "document",
        "mime": "text/plain",
        "cozyMetadata": {
          "doctypeVersion": "1",
          "metadataVersion": 1,
          "createdAt": "2016-09-20T18:32:49Z",
          "createdByApp": "drive",
          "createdOn": "https://cozy.example.com/",
          "updatedAt": "2016-09-20T18:32:49Z",
          "uploadedAt": "2016-09-20T18:32:49Z",
          "uploadedOn": "https://cozy.example.com/",
          "uploadedBy": {
            "slug": "drive"
          }
        }
      }
    }
  ]
}
```

### GET /sharings/drives/:id/:file-id/size

This endpoint returns the size taken by the files in a directory inside a shared
drive, including those in subdirectories.

This route is supported only for directory-root shared drives. File-root shared
drives return `422 Unprocessable Entity`.

#### Request

```http
GET /sharings/drives/aae62886e79611ef8381fb83ff72e425/af1e1b66e92111ef8ddd5fbac4938703/size HTTP/1.1
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
    "type": "io.cozy.files.sizes",
    "id": "af1e1b66e92111ef8ddd5fbac4938703",
    "attributes": {
      "size": "1234567890"
    },
    "meta": {}
  }
}
```

### POST /sharings/drives/:id/:file-id/copy

Duplicates a file.

Identical call to [`POST /files/:file-id/copy`](files.md#post-filesfile-idcopy) but over a shared drive.
See there for request and response examples, the only difference is the URL.

This route is supported only for directory-root shared drives. File-root shared
drives return `422 Unprocessable Entity`.

### POST /sharings/drives/move

Move or copy a file or a directory between locations (personal drive and/or shared drives).

Supported flows:

- From a shared drive to another shared drive (same stack or cross-stack)
- From a shared drive to a personal drive
- From a personal drive to a shared drive

At least one side (source or destination) must be a shared drive.

The `copy` parameter controls whether the operation is a move (default) or copy:
- When `copy: false` (default): The source file/directory is moved to the destination and deleted from the source
- When `copy: true`: The source file/directory is copied to the destination but remains in the source location

#### Request

Body (preferred):

```json
{
  "source": {
    "instance": "https://alice.localhost:8080", // omit for personal drive
    "sharing_id": "share_src_id",               // required when instance is set
    "file_id": "file123",                       // exactly one of file_id or dir_id
    "dir_id": ""
  },
  "dest": {
    "instance": "https://bob.localhost:8080",   // omit for personal drive
    "sharing_id": "share_dst_id",               // required when instance is set
    "dir_id": "destDir456"                      // destination directory id
  },
  "copy": false                                 // optional, defaults to false
}
```

Validation rules:

- Exactly one of `source.file_id` or `source.dir_id` must be provided.
- `dest.dir_id` is required.
- If `source.instance` is provided, `source.sharing_id` is required.
- If `dest.instance` is provided, `dest.sharing_id` is required.
- At least one of `source.sharing_id` or `dest.sharing_id` must be provided.
- When one side is a personal drive (no `instance`), whole-type permission on `io.cozy.files` is required.
- `copy` is optional and defaults to `false` (move operation).

Notes on behavior:

- Name conflicts at destination are resolved automatically by appending a conflict suffix.
- For directory operations, the subtree is recreated top-down and files are copied, then sources are deleted (only for move operations).
- Cross-stack operations perform a remote download/upload and delete the remote source upon success (only for move operations).
- When `copy: true`, source files and directories are preserved in their original location.
- When `copy: false` (default), source files and directories are deleted after successful copy to destination.

Additional rules for file-root shared drives:

- A file-root shared drive can be used only as a copy source.
- `move` is rejected when the source shared drive is file-root.
- A file-root shared drive cannot be used as a destination for either move or
  copy.
- When a file-root shared drive is the source, only its root file can be
  copied.

#### Responses

- 201 Created, with a JSON object describing the created file or directory on the destination.
- 400 Bad Request, when required inputs are missing or invalid.
- 403 Forbidden, on permission errors or missing credentials for a shared drive.
- 404 Not Found, when referenced files or drives do not exist.

Example response (file, abbreviated):

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "new-file-id",
    "attributes": {
      "name": "example.txt",
      "dir_id": "destDir456",
      "type": "file",
      "size": 123,
      "mime": "text/plain",
      "class": "document",
      "executable": false,
      "tags": [],
      "driveId": "aae62886e7..." // present when destination is a shared drive
    }
  }
}
```

When the move occurs locally (same stack), the response matches the standard local creation response. For cross-stack moves, the response is built from the remote upload result and includes equivalent attributes.

#### Copy Example

To copy a file instead of moving it, set the `copy` parameter to `true`:

```json
{
  "source": {
    "instance": "https://alice.localhost:8080",
    "sharing_id": "share_src_id",
    "file_id": "file123"
  },
  "dest": {
    "instance": "https://bob.localhost:8080",
    "sharing_id": "share_dst_id",
    "dir_id": "destDir456"
  },
  "copy": true
}
```

This will create a copy of the file in the destination while preserving the original file in the source location.

### PATCH /sharings/drives/:id/:file-id

This endpoint can be used to update the metadata of a file or directory, to
rename it or to move it within the same shared drive.

Some specific attributes of the patch can be used:

- `dir_id` attribute can be updated to move a file or directory (the new
  directory needs to be in the same shared drive as the old one)

For file-root shared drives, metadata updates and rename are supported, but
changing `dir_id` is rejected with `422 Unprocessable Entity`.

#### HTTP headers

It's possible to send the `If-Match` header, with the previous revision of the
file/directory (optional).

#### Request

```http
PATCH /sharings/drives/aae62886e79611ef8381fb83ff72e425/9152d568-7e7c-11e6-a377-37cbfb190b4b HTTP/1.1
Accept: application/vnd.api+json
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
    "attributes": {
      "type": "file",
      "name": "hi.txt",
      "dir_id": "f2f36fec-8018-11e6-abd8-8b3814d9a465",
      "tags": ["poem"]
    }
  }
}
```

#### Status codes

- 200 OK, when the file or directory metadata has been successfully updated
- 400 Bad Request, when a the destination directory does not exist
- 403 Forbidden, when the file or directory cannot be modified or the
- destination directory is not accessible
- 404 Not Found, when the file/directory does not exist
- 412 Precondition Failed, when the `If-Match` header is set and doesn't match
  the last revision of the file/directory
- 422 Unprocessable Entity, when the sent data is invalid (for example, the
  parent doesn't exist)

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
Location: https://cozy.example.com/sharings/drives/aae62886e79611ef8381fb83ff72e425/9152d568-7e7c-11e6-a377-37cbfb190b4b
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
    "meta": {
      "rev": "1-0e6d5b72"
    },
    "attributes": {
      "type": "file",
      "name": "hi.txt",
      "trashed": false,
      "md5sum": "ODZmYjI2OWQxOTBkMmM4NQo=",
      "created_at": "2016-09-19T12:38:04Z",
      "updated_at": "2016-09-19T12:38:04Z",
      "tags": ["poem"],
      "size": 12,
      "executable": false,
      "class": "document",
      "mime": "text/plain",
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2016-09-20T18:32:49Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2016-09-22T13:32:51Z",
        "uploadedAt": "2016-09-21T04:27:50Z",
        "uploadedOn": "https://cozy.example.com/",
        "uploadedBy": {
          "slug": "drive"
        }
      }
    },
    "relationships": {
      "parent": {
        "links": {
          "related": "/files/f2f36fec-8018-11e6-abd8-8b3814d9a465"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "f2f36fec-8018-11e6-abd8-8b3814d9a465"
        }
      }
    },
    "links": {
      "self": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b"
    }
  }
}
```

### Similar to /files

The following routes are similar to [`/files`](files.md), but for a shared
drive.

#### GET /sharings/drives/:id/metadata
#### POST /sharings/drives/upload/metadata
#### DELETE /sharings/drives/:file-id
#### POST /sharings/drives/trash/:file-id
#### DELETE /sharings/drives/trash/:file-id

For file-root shared drives:

- `GET /sharings/drives/:id/metadata` is not supported and returns `422`
- `POST /sharings/drives/upload/metadata` is not supported and returns `422`
- `DELETE /sharings/drives/:file-id` is supported for the root file
- trash and restore routes are supported for the root file

Trash operations keep the same file metadata shape as `/files`: when an item is
moved to the trash, `cozyMetadata.trashedAt` and `cozyMetadata.trashedBy` are
exposed on both the caller side and the replicated shared-drive copies. On
shared-drive requests, `trashedBy.kind` is `member` and
`trashedBy.displayName` / `trashedBy.domain` are derived from the sharing
member linked to the `DriveToken`.

## Share-by-link permissions

The following routes manage share-by-link permissions scoped to files inside a
shared drive:

- `GET /sharings/drives/:id/permissions?ids=...`
- `POST /sharings/drives/:id/permissions`
- `PATCH /sharings/drives/:id/permissions/:perm-id`
- `DELETE /sharings/drives/:id/permissions/:perm-id`

### GET /sharings/drives/:id/permissions?ids=...

Lists the share-by-link permissions for the requested file or folder IDs inside
the shared drive.

For file-root shared drives, the only valid target is the root file.

Authorization rules:

- The shared-drive owner can list all matching links.
- A write-capable recipient can list writable and read-only links.
- A read-only recipient only sees read-only links.

Validation:

- `ids` is required and must be a comma-separated list of file or folder IDs.
- Every requested ID must belong to the shared drive.

Status codes:

- `200 OK` listed
- `403 Forbidden` caller cannot access shared-drive permissions
- `422 Unprocessable Entity` missing or invalid `ids`

### POST /sharings/drives/:id/permissions

Creates a share-by-link permission for one file or folder in the shared drive.
The request body uses the same JSON:API shape as [`POST /permissions`](permissions.md#post-permissions).

For file-root shared drives, the only valid target is the root file.

Authorization rules:

- The shared-drive owner can create a link.
- A write-capable recipient can create a link.
- A read-only recipient can create a read-only link.

Validation:

- The permission set must target exactly one file or folder.
- The target type must be `io.cozy.files`.
- Selectors are not supported.
- The target must belong to the shared drive and must be readable by the
  caller.
- A read-only recipient cannot request writable verbs.
- Only one share-by-link permission can exist per target. A second creation
  attempt on the same target returns a conflict, regardless of which member
  created the existing link.

Status codes:

- `200 OK` created
- `400 Bad Request` invalid permission set or invalid target
- `403 Forbidden` caller lacks access to the target or requests a permission
  set larger than their shared-drive access
- `409 Conflict` a share-by-link permission already exists for this target

### PATCH /sharings/drives/:id/permissions/:perm-id

Updates an existing share-by-link permission.

Authorization rules:

- The shared-drive owner can patch any share-by-link permission.
- The creator of a share-by-link permission can patch the permission they
  created.
- Creator resolution works for same-stack and cross-stack recipients.
- A read-only shared-drive recipient can patch only the read-only permission
  they created.
- Public share tokens (`share`, `share-preview`) cannot patch permissions.

Allowed updates:

- `password`
- `expires_at`
- `permissions` (same target only, owner or write-capable creator only)

Validation:

- `password` must be a string (empty string clears the password).
- `expires_at` must be a string (empty string clears expiration, otherwise
  RFC3339 date-time).
- `permissions`, when provided, must still target the same file or folder
  inside the shared drive.
- for file-root shared drives, that target must remain the root file
- A write-capable creator or the owner can promote a read-only link to a
  writable link if their current token grants those verbs.
- A read-only shared-drive recipient can only update `password` and
  `expires_at`.
- A read-only shared-drive recipient cannot patch `permissions`, even if the
  result would stay read-only.

Status codes:

- `200 OK` updated
- `400 Bad Request` invalid payload (for example trying to update `codes`),
  invalid `password` / `expires_at` attribute format
- `403 Forbidden` caller is not owner/creator, caller identity cannot be
  verified for a shared-drive token, or a read-only recipient attempts to
  patch `permissions`
- `404 Not Found` permission ID does not exist

### DELETE /sharings/drives/:id/permissions/:perm-id

Revokes an existing share-by-link permission.

Authorization rules:

- The shared-drive owner can revoke any share-by-link permission.
- The creator of a share-by-link permission can revoke the permission they
  created.
- A read-only shared-drive recipient can revoke the read-only permission they
  created.
- Public share tokens (`share`, `share-preview`) cannot revoke permissions.

Status codes:

- `204 No Content` revoked
- `400 Bad Request` permission is not a share-by-link permission
- `403 Forbidden` caller is not owner/creator, or permission does not belong
  to this shared drive
- `404 Not Found` permission ID does not exist

## Delegated email sharing

Members of a shared drive add new recipients through the sharing API, not
through a drive-specific route:

- `POST /sharings/:sharing-id/recipients`

When that request is sent from a recipient Cozy, the stack delegates the
operation to the owner Cozy internally.

Authorization rules:

- The shared-drive owner can add recipients as for any other sharing.
- A write-capable recipient can invite read-write or read-only recipients.
- A read-only recipient can invite only read-only recipients.
- A read-only recipient receives `403 Forbidden` for a read-write invite.


## Versions

The identifier of the `io.cozy.files.versions` is composed of the `file-id` and
another string called the `version-id`, separated by a `/`. So, when a route
makes reference to `/something/:file-id/:version-id`, you can use the identifier
of the version document (without having to prepend the file identifier).

### POST /sharings/drives/:id/:file-id/versions

Create a new version of a file, with the same content but new metadata.

Identical call to [`POST /files/:file-id/versions`](files.md#post-filesfile-idversions)
but over a shared drive. See there for request and response examples, the only
difference is the URL.

For file-root shared drives, this route is supported for the root file.

### GET /sharings/drives/:id/download/:file-id/:version-id

Downloads an old version of the file content.

Identical call to [`GET /files/download/:file-id/:version-id`](files.md#get-filesdownloadfile-idversion-id)
but over a shared drive. See there for request and response examples, the only
difference is the URL.

## Notes

### POST /sharings/drives/:id/notes

Create a note inside a shared drive. Identical to [`POST /notes`](notes.md#post-notes).

This route is supported only for directory-root shared drives. File-root shared
drives return `422 Unprocessable Entity`.

### GET /sharings/drives/:id/notes/:file-id/open

Return the parameters to build the URL where the note can be opened.
Identical to [`GET /notes/:file-id/open`](notes.md#get-notesidopen).

## Office

### GET /sharings/drives/:id/office/:file-id/open

Returns the parameters to open an office document. Identical to
[`GET /office/:file-id/open`](office.md#get-officeidopen).

## Editors

### GET /sharings/drives/:id/editor/:file-id/open

Return the parameters to open a file from a shared drive with an editor.
Identical to [`GET /editor/:file-id/open`](files.md#get-editorfile-idopen), but
scoped to the shared drive.

Recipients are resolved through the shared-drive owner instance, so the returned
`instance`, `file_id`, and `sharecode` are the values to use for opening the
file on that owner instance.

## Shortcuts

### GET /sharings/drives/:id/shortcuts/:file-id

Get information about a shortcut inside a shared drive. Identical to
[`GET /shortcuts/:id`](shortcuts.md#get-shortcutsid), with the same two
variants depending on the `Accept` header (JSON-API response or redirect).

Recipients are transparently proxied to the owner's instance.

## Realtime

### GET /sharings/drives/:id/realtime

Get the changes inside a shared drive in real-time from a websocket.
Identical to [`GET /realtime`](realtime.md), except subscribing to the shared drive is automatically done.

On the recipient side, connecting marks the sharing as seen (its shortcut
leaves the `new` status counted by `GET /sharings/news`). Background
consumers that watch a drive without the user looking at it, like the
dataproxy indexer, must pass the query parameter `background=true` to keep
the sharing unseen.

This route is currently available only for directory-root shared drives.
For file-root shared drives, the websocket stream emits only events whose
target is the root file of the drive. Unrelated file events are filtered out.

```
client > {"method": "AUTH",
          "payload": "xxAppOrAuthTokenxx="}
server > {"event": "UPDATED",
          "payload": {"id": "idB", "rev": "6-457...", "type": "io.cozy.files", "doc": {embeded doc ...}}}
```
