[Table of contents](README.md#table-of-contents)

# Workers

This page list all the currently available workers on the cozy-stack. It
describes their input arguments object. See the [jobs document](jobs.md) to know
more about the API context in which you can see how to use these arguments.

## log worker

The `log` worker will just print in the log file the job sent to it. It can
useful for debugging for example.

## thumbnail worker

The `thumbnail` worker is used internally by the stack to generate thumbnails
from the image files of a cozy instance.

## konnector worker

The `konnector` worker is used to execute JS code that collects files and data
from external providers, and put them in the Cozy. Its execution is detailed on
[the Konnector Worker specs](https://docs.cozy.io/en/cozy-stack/konnectors-workflow/#konnector-worker-specs).

### Job arguments

To execute a konnector through a job, the expected `arguments` are the following:
```
"arguments": {
    "konnector": <konnector_slug>,
    "account": <account_id>
}
```

## service worker

The `service` worker is used to process background jobs, generally to compute
non-realtime user data.

```
"arguments": {
    "slug": <app slug>,
    "name": <service name>,
    "fields": <custom fields>
}
```

### Job arguments

To execute a service through a job, the expected `arguments` are the following:
```
"arguments": {
    "name": <service_name>,
    "slug": <application_slug>
}
```

## push worker

The `push` worker can be used to send push-notifications to a user's device. The
options are:

-   `client_id`: the ID of the oauth client to push a notification to.
-   `title`: the title of the notification
-   `message`: the content of the notification
-   `data`: key-value string map for additional metadata (optional)
-   `priority`: the notification priority: `high` or `normal` (optional)
-   `topic`: the topic identifier of the notification (optional)
-   `sound`: a sound associated with the notification (optional)

### Example

```json
{
    "client_id": "abcdef123123123",
    "title": "My Notification",
    "message": "My notification content.",
    "priority": "high"
}
```

### Permissions

To use this worker from a client-side application, you should use
[the notifications API](./notifications.md).

## sms worker

The `sms` worker can be used to send SMS notifications to a user, via
[the notifications API](./notifications.md). 

The phone number used is the `primary phone number` registered in 
the contact that has the flag `me: true` (see [contact doctype](https://github.com/cozy/cozy-doctypes/blob/master/docs/io.cozy.contacts.md) )

Note: If an error happens during the send of the sms (for instance no phone number on the cozy or SMS provider not available), the content will be send by email as fallback.

To use the sms worker, you need to configure your SMS provider:
- first enable sms worker in your [stack configuration](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml#L156)
- configure the [notification configuration](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml#L281-L285) by setting your provider's informations.

## unzip worker

The `unzip` worker can take a zip archive from the VFS, and will unzip the files
inside it to a directory of the VFS. The options are:

-   `zip`: the ID of the zip file
-   `destination`: the ID of the directory where the files will be unzipped.

### Example

```json
{
    "zip": "8737b5d6-51b6-11e7-9194-bf5b64b3bc9e",
    "destination": "88750a84-51b6-11e7-ba90-4f0b1cb62b7b"
}
```

### Permissions

To use this worker from a client-side application, you will need to ask the
permission. It is done by adding this to the manifest:

```json
{
    "permissions": {
        "unzip-to-a-directory": {
            "description": "Required to unzip a file inside the cozy",
            "type": "io.cozy.jobs",
            "verbs": ["POST"],
            "selector": "worker",
            "values": ["unzip"]
        }
    }
}
```

## zip worker

The `zip` worker does pretty much the opposite of the `unzip` worker: it
creates a zip archive from files in the VFS. The options are:

- `files`: a map with the the files to zip (their path in the zip as key, their
  VFS identifier as value)
- `dir_id`: the directory identifier where the zip archive will be put
- `filename`: the name of the zip archive.

**Note:** it is possible to include only a page for a PDF file, by using an
object with `id` and `page` instead of just the file identifier.

### Example

```json
{
    "files": {
        "selection/one.pdf": "36abc4c0-90fe-11e9-b05b-1fa43ca781ef",
        "selection/two.pdf": "36eb54c8-90fe-11e9-aeca-03ddc3acf91c",
        "selection/three.pdf": "37284586-90fe-11e9-be6d-179f72076e43",
        "selection/four.pdf": "37655462-90fe-11e9-9059-8739e3746720",
        "selection/five.pdf": "379fedfc-90fe-11e9-849f-0bbe172eba5f",
        "selection/front.pdf": { "id": "49ca9e50-1074-013d-361a-18c04daba326", "page": 1 },
        "selection/back.pdf":  { "id": "49ca9e50-1074-013d-361a-18c04daba326", "page": 2 }
    },
    "dir_id": "3657ce9c-90fe-11e9-b40b-33baf841bcb8",
    "filename": "selection.zip"
}
```

### Permissions

To use this worker from a client-side application, you will need to ask the
permission. It is done by adding this to the manifest:

```json
{
    "permissions": {
        "create-a-zip-archive": {
            "description": "Required to create a zip archive inside the cozy",
            "type": "io.cozy.jobs",
            "verbs": ["POST"],
            "selector": "worker",
            "values": ["zip"]
        }
    }
}
```

## sendmail worker

The `sendmail` worker can be used to send mail from the stack. It implies that
the stack has properly configured an access to an SMTP server. You can see an
example of configuration in the [cozy.example.yaml](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml)
file at the root of this repository.

`sendmail` options fields are the following:

-   `mode`: string specifying the mode of the send:
    -   `noreply` to send a notification mail to the user
    -   `pending` to send a mail to the user to confirm their new email address
    -   `from` to send a mail from the user
    -   `support` to send both an email to the support and a confirmation to
        the user
    -   `campaign` to send a non transactional email to the user via an SMTP
        server using the following configurations, in order of priority:
        1. `campaign_mail.contexts.<context name>` if defined
        2. `campaign_mail` otherwise
        3. `mail` as the final fallback
-   `to`: list of object `{name, email}` representing the addresses of the
    recipients. (should not be used in `noreply` mode)
-   `subject`: string specifying the subject of the mail
-   `parts`: list of part objects `{type, body}` listing representing the
    content parts of the
    -   `type` string of the content type: either `text/html` or `text/plain`
    -   `body` string of the actual body content of the part
-   `attachments`: list of objects `{filename, content}` that represent the
    files attached to the email, where the `content` is base64-encoded

### Examples

```js
// from mode sending mail from the user to a list of recipients
{
    "mode": "from",
    "to": [
        {"name": "John Doe 1", "email":"john1@doe"},
        {"name": "John Doe 2", "email":"john2@doe"}
    ],
    "subject": "Hey !",
    "parts": [
        {"type":"text/html", "body": "<h1>Hey !</h1>"},
        {"type":"text/plain", "body": "Hey !"}
    ]
}

// noreply mode, sending a notification mail to the user
{
    "mode": "noreply",
    "subject": "You've got a new file !",
    "parts": [
        {"type":"text/html", "body": "<h1>Hey !</h1>"},
        {"type":"text/plain", "body": "Hey !"}
    ]
}
```

### Permissions

To use this worker from a client-side application, you will need to ask the
permission. It is done by adding this to the manifest:

```json
{
    "permissions": {
        "mail-from-the-user": {
            "description": "Required to send mails from the user to his/her friends",
            "type": "io.cozy.jobs",
            "verbs": ["POST"],
            "selector": "worker",
            "values": ["sendmail"]
        }
    }
}
```

## export

The `export` worker can be used to generate allow the export of all data
contained in the cozy. At the end of the export, a mail is sent to the user
containing a link to access to its data.

The progress of the export process can be followed with realtime events on the
doctype `io.cozy.exports`.

Its options are:

-   `parts_size`: the size in bytes of the sizes index splitting done for
    multi-part download of files data
-   `max_age`: the maximum age duration of the archive before it expires
-   `with_doctypes`: the list of exported doctypes (exports all doctypes if empty)

### Example

```json
{
    "parts_size": 52428800,
    "max_age": 60000000000, // 1 minute
    "with_doctypes": ["io.cozy.accounts", "io.cozy.files"] // empty or null means all doctypes
}
```

## import

The `import` worker can be used to import the data from an export. The instance
will be reset before importing data to avoid complex logic of reconciliation.
The instance is blocked during the import, and a mail is sent at the end of the
import, when the instance can be accessed again.

Its options are:

- `manifest_url`: the URL of the manifest for the exported data.

### Example

```json
{
  "manifest_url": "http://cozy.localhost:8080/move/exports/QUFBQUFGLVg5b3c0WTJNNU9HRmpPR0V4WlRnd01XVTJZMlU0T0RjeE5UaGpNVEF3TWpKbVplblFfRWZWUVAtRGJXU0lnV2tIZ3NsVHN5dUR6V0ZIdVdSeERLb196X3A0"
}
```

### Permissions

To use this worker from a client-side application, you will need to ask the
permission. It is done by adding this to the manifest:

```json
{
    "permissions": {
        "mail-from-the-user": {
            "description": "Required to create a export of the user's data",
            "type": "io.cozy.jobs",
            "verbs": ["POST"],
            "selector": "worker",
            "values": ["export"]
        }
    }
}
```

## trash-files worker

This worker is used only by the stack: when the user asks to clean the trash,
the stack will delete the files from CouchDB and put a job for this worker. The
deletion of files in Swift is slow, and can be done via this worker
asynchronously. The behavior is not the same for all the VFS:

- afero: all the work is done during the HTTP request, no job is pushed
- Swift layout v1: all the work is done during the HTTP request, no job is pushed
- Swift layout v2: the files are deleted in CouchDB during the HTTP request,
  and they are deleted in Swift via the job
- Swift layout v3: the files are deleted in CouchDB during the HTTP request,
  the file versions are deleted in CouchDB via the job, and the files and their
  versions are deleted in Swift via the job.

## clean-old-trashed worker

This worker is used to automatically delete files and directories that are in
the trash for too long. The threshold for deletion is configurable per context
in the config file, via the `fs.auto_clean_trashed_after` parameter.

## share workers

The stack have 5 workers to power the sharings (internal usage only):

1. `share-group`, to add/remove members to a sharing
2. `share-track`, to update the `io.cozy.shared` database
3. `share-replicate`, to start a replicator for most documents
4. `share-upload`, to upload files
5. `share-autoaccept`, to automatically accept a sharing for trusted members

### Share-group

When a contact is added to or removed from a group, the change should be
reflected in the group's sharings' recipients. The message is composed of the
contact ID, the list of groups added and the list of groups removed.

### Share-track

The message is composed of 3 fields: the sharing ID, the rule index, and the
doctype. The event is similar to a realtime event: a verb, a document, and
optionaly the old version of this document.

### Share-replicate and share-upload

The message is composed of a sharing ID and a count of the number of errors
(i.e. the number of times this job was retried).

### Share-autoaccept

The message is composed of the sharing ID and the OAuth state assigned to the
recipient. When the job runs on the recipient Cozy it calls the owner's
`/sharings/:sharing-id/answer` endpoint (the state is provided in the Drive
sharing credentials payload in
[PUT /sharings/:sharing-id](sharing.md#put-sharingssharing-id)) to perform the
acceptance flow without user interaction.

## notes-save

This is another worker for the interal usage of the stack. It allows to write
to the VFS a note in an asynchronous way. Writing to the VFS after each tiny
change would kill the performance for realtime collaboration, so the stack
writes the note to a cache, and has a trigger with debounce to persist the note
to the VFS later.

## clean-clients

This internal worker will delete unused OAuth clients. When an OAuth client is
created, it has the pending flag set to true. When an access code is generated
for a client, the flag is set to false. If 1 hour after the client has been
created, the flag is still true, this worker will delete the client. It will
help to clean unused clients which can be misleading for the user when the list
of clients in settings is displayed.

## migrations

The `migrations` worker can be used to migrate a cozy instance. Currently, it
has a single option, `type`, with two supported values:

* `remove-unwanted-folders`: remove the administrative and/or photos folders
  for contexts where `init_administrative_folder` or `init_photos_folder` is
  set to `false`
* `to-swift-v3`: migrate a cozy instance that has files in swift from a V1 or V2 layout to a V3 layout
* `accounts-to-organization`: create [ciphers](https://docs.cozy.io/en/cozy-doctypes/docs/com.bitwarden.ciphers/)
  from [accounts](https://docs.cozy.io/en/cozy-doctypes/docs/io.cozy.accounts/),
  re-encrypted with the organization key
* `notes-mime-type`: update the notes mime-type to
  `text/vnd.cozy.note+markdown` to allow them to be listed in the cozy-notes
  application.

### Example

It can be launched from command-line with:

```sh
$ cozy-stack jobs run migrations --domain example.mycozy.cloud --json '{"type": "to-swift-v3"}'
```

## index

This worker is used for sending data to a RAG. It looks at the changes feed for
the given doctype, send the changes to an external indexer that will generate
embeddings for the data and put them in a vector database.

## antivirus

The `antivirus` worker scans files for malware using ClamAV. It is automatically
triggered when a file is created or updated (when the content changes). The
worker sends the file content to a ClamAV daemon for scanning and updates the
file document with the scan result.

### How it works

* When a file is created or its content is updated, the stack automatically
   schedules an antivirus scan job
* The file's `antivirus_scan.status` is immediately set to `pending`
* The worker retrieves the file, sends it to ClamAV for scanning
* The scan result is stored in the file document's `antivirus_scan` field
* If the file is infected, skipped, or an error occurs, a notification is sent
   to the user (if enabled)

### Job arguments

The worker expects a single argument:

```json
{
    "file_id": "8737b5d6-51b6-11e7-9194-bf5b64b3bc9e"
}
```

### Configuration

Antivirus scanning must be enabled per-context in the stack configuration. See
[cozy.example.yaml](https://github.com/cozy/cozy-stack/blob/master/cozy.example.yaml)
for a complete example.

### Requirements

- ClamAV daemon (`clamd`) must be running and accessible at the configured address
- The daemon should be configured with appropriate scan limits
