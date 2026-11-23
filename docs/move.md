[Table of contents](README.md#table-of-contents)

# Move, export and import

## Export

A Cozy's user can ask at any time to export a snapshot of all its data and
metadata. This export takes place asynchronously and is separated in two parts:

- a metadata tarball containing the in a JSON format all the doctypes
- multi-part files tarballs containing the files (or a subpart of the files).

The export process is part of a worker described in the
[workers section](./workers.md#export) of the documentation.

Endpoints described in this documentation require a permission on the
`io.cozy.exports` doctype.

### POST /move/exports

This endpoint can be used to create a new export job.

Exports options fields are:

-   `parts_size` (optional) (int): the size in bytes of a tarball files part.
-   `max_age` (optional) (duration / nanosecs): the maximum age of the export
    data.
-   `with_doctypes` (optional) (string array): the list of exported doctypes

#### Request

```http
POST /move/exports HTTP/1.1
Host: source.cozy.localhost
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "attributes": {
            "parts_size": 10240,
            "with_doctypes": []
        }
    }
}
```

### GET /move/exports/:opaque-identifier

This endpoint can be used to fetch the metadata of an export.

Exports fields are:

-   `parts_size` (int): the size in bytes of a tarball files part.
-   `parts_cursors` (string array): the list of cursors to access to the
    different files parts.
-   `with_doctypes` (string array): the list of exported doctypes
    (if empty of null, all doctypes are exported)
-   `state` (string): the state of the export (`"exporting"` / `"done"` /
    `"error"`).
-   `created_at` (string / time): the date of creation of the export
-   `expires_at` (string / time): the date of expiration of the export
-   `total_size` (int): the total size of the exported documents from CouchDB
-   `creation_duration` (int): the amount of nanoseconds taken for the creation
    of the export
-   `error` (string): an error string if the export is in an `"error"` state

#### Request

```http
GET /move/exports/XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX HTTP/1.1
Host: source.cozy.localhost
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "type": "io.cozy.exports",
        "id": "86dbb546ca49f0ed1ce0a1ff0d1b15e3",
        "meta": {
            "rev": "2-XXX"
        },
        "attributes": {
            "parts_size": 10240,
            "parts_cursors": ["AAA", "BBB", "CCC"],
            "with_doctypes": [],
            "state": "done",
            "created_at": "2018-05-04T08:59:37.530693972+02:00",
            "expires_at": "2018-05-11T08:59:37.530693972+02:00",
            "total_size": 1123,
            "creation_duration": 62978511,
            "error": ""
        }
    }
}
```

### GET /move/exports/data/:opaque-identifier?cursor=XXX

This endpoint will download an archive containing the metadata and files of the
user, as part of a multi-part download. The cursor given should be one of the
defined in the export document `parts_cursors`.

Only the first part of the archive contains the metadata.

To get all the parts, this endpoint must be called one time with no cursor, and
one time for each cursor in `parts_cursors`.

## Import

### POST /move/imports/precheck

This endpoint can be use to check that an export can be imported from the given
URL, before doing the real import. It is called from the settings application,
not from the cozy-move wizard.

#### Request

```http
POST /move/imports/precheck HTTP/1.1
Host: destination.cozy.localhost
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "attributes": {
            "url": "https://settings.source.cozy.localhost/#/exports/XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
        }
    }
}
```

#### Responses

- `204 No Content` if every thing is fine
- `412 Precondition Failed` if no archive can be found at the given URL
- `422 Entity Too Large` if the quota is too small to import the files

### POST /move/imports

This endpoint can be used to really start an import.

#### Request

```http
POST /move/imports HTTP/1.1
Host: destination.cozy.localhost
Authorization: Bearer ...
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "attributes": {
            "url": "https://settings.source.cozy.localhost/#/exports/XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
        }
    }
}
```

#### Responses

```http
HTTP/1.1 303 See Other
Location: https://destination.cozy.localhost/move/importing
```

### POST /move/importing

This endpoint is called on the target Cozy by the source Cozy to block the
instance during the move. A `source` parameter can be put in the query-string,
with the domain of the Cozy source (for information).

### GET /move/importing

This shows a page for the user to wait that the import finishes.

### GET /move/importing/realtime

This is a websocket endpoint that can be used to wait for the end of the
import. The server will send an event when it is done (or errored):

```
server> {"redirect": "http://cozy.localhost:8080/auth/login"}
```

### GET /move/authorize

This endpoint is used by cozy-move to select the cozy source. If the user is
already logged in, we don't ask its password again, as the delivered token will
still need a confirmation by mail to start moving the Cozy. The `redirect_uri`
must be on the move service of the instance (`move.url` in the config, or
`move_url` in the context), and the code can be used only once.

#### Request

```http
GET /move/authorize?state=8d560d60&redirect_uri=https://move.cozycloud.cc/callback/source HTTP/1.1
Server: source.cozy.localhost
```

#### Response

```http
HTTP/1.1 302 Found
Location: https://move.cozycloud.cc/callback/source?code=543d7eb8149c&used=123456&quota=5000000&state=8d560d60&vault=false
```

### POST /move/initialize

This endpoint is used by the settings application to open the move wizard.

#### Request

```http
POST /move/initialize HTTP/1.1
Host: source.cozy.localhost
```

#### Response

```http
HTTP/1.1 307 Temporary Redirect
Location: https://move.cozycloud.cc/initialize?code=834d7eb8149c&cozy_url=https://source.cozy.localhost&used=123456&quota=5000000&client_id=09136b00-1778-0139-f0a7-543d7eb8149c&client_secret=NDkyZTEzMDA&vault=false
```

### POST /move/request

This endpoint is used after the user has selected both instances on cozy-move
to prepare the export and send the confirmation mail.

#### Request

```http
POST /move/request HTTP/1.1
Content-Type: application/x-www-form-urlencoded

code=834d7eb8149c
&target_url=https://target.cozy.localhost/
&target_token=M2EwYjlhZjAtMTc3OC0wMTM5LWYwYWMtNTQzZDdlYjgxNDlj
&target_client_id=09136b00-1778-0139-f0a7-543d7eb8149c
&target_client_secret=NDkyZTEzMDA
```

**Note:** instead of `code`, we can have `token`, `client_id`, and
`client_secret` (depending if the user has started the workflow from the
settings app or from cozy-move).

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/html
```

### GET /move/go

This endpoint is used to confirm the move. It will ask the other Cozy to block
its-self during the move and pushs a job for the export.

#### Request

```http
GET /move/go?secret=tNTQzZDdlYjgxNDlj HTTP/1.1
Host: source.cozy.localhost
```

#### Reponse

```http
HTTP/1.1 303 See Other
Location: https://target.cozy.localhost/move/importing
```

### POST /move/finalize

When the move has finished successfully, the target Cozy calls this endpoint on
the source Cozy so that it can stop the konnectors and unblock the instance.

#### Request

```http
POST /move/finalize?subdomain=flat HTTP/1.1
Host: source.cozy.localhost
```
#### Reponse

```
HTTP/1.1 204 No Content
```

### POST /move/abort

If the export or the import fails during a move, the stack will call this
endpoint for the other instance to unblock it.

#### Request

```http
POST /move/abort HTTP/1.1
Host: source.cozy.localhost
```
#### Reponse

```
HTTP/1.1 204 No Content
```

### GET /move/vault

This shows a page for the user with instructions about how to import their vault.
