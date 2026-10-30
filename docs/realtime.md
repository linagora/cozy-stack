[Table of contents](README.md#table-of-contents)

# Realtime

The stack offers a way for applications to be notified in real-time of what
happens on the server via a websocket connection.

We start with a normal websocket handshake.

Websockets include a protocol description in handshake, the protocol described
below is hereby named `io.cozy.websocket`.

Changes to the websocket protocol should be given versions, support for older
version should be maintained when reasonable.

```http
GET /realtime/ HTTP/1.1
Host: mycozy.example.com
Upgrade: websocket
Connection: Upgrade
Origin: http://calendar.mycozy.example.com
Sec-WebSocket-Key: x3JrandomLkh9GBhXDw==
Sec-WebSocket-Protocol: io.cozy.websocket
Sec-WebSocket-Version: 13
```

Then messages are sent using json:

```
client > {"method": "AUTH",
          "payload": "xxAppOrAuthTokenxx="}
client > {"method": "SUBSCRIBE",
          "payload": {"type": "io.cozy.files"}}
client > {"method": "SUBSCRIBE",
          "payload": {"type": "io.cozy.contacts"}}
server > {"event": "UPDATED",
          "payload": {"id": "idA", "rev": "2-705...", "type": "io.cozy.contacts", "doc": {embeded doc ...}}}
server > {"event": "DELETED",
          "payload": {"id": "idA", "rev": "3-541...", "type": "io.cozy.contacts"}}
client > {"method": "UNSUBSCRIBE",
          "payload": {"type": "io.cozy.contacts"}}
server > {"event": "UPDATED",
          "payload": {"id": "idB", "rev": "6-457...", "type": "io.cozy.files", "doc": {embeded doc ...}}}
```

## AUTH

It must be the first command to be sent. The client gives its token with this
command, and the stack will use it to know which are the permissions of the app.

```
{"method": "AUTH", "payload": "eyJhbGciOiJIUzUxMiIsInR5cCI6IkpXVCJ9.eyJhdWQiOiJhcHAiLCJpYXQiOjE0OTg4MTY1OTEsImlzcyI6ImNvenkudG9vbHM6ODA4MCIsInN1YiI6Im1pbmkifQ.eH9DhoHz7rg8gR7noAiKfeo8eL3Q_PzyuskO_x3T8Hlh9q_IV-4zqoGtjTiO7luD6_VcLboEU-6o3XBek84VTg"}
```

## SUBSCRIBE

A client can send a SUBSCRIBE request to be notified of changes. The payload is
a selector for the events it wishes to receive For now the only possible
selector is on type & optionally id.

```
{"method": "SUBSCRIBE", "payload": {"type": "[desired doctype]"}}
{"method": "SUBSCRIBE", "payload": {"type": "[desired doctype]", "id": "idA"}}
```

In order to subscribe, a client must have permission `GET` on the passed
selector. Otherwise an error is passed in the message feed.

```
server > {"event": "error",
          "payload": {
            "status": "403 Forbidden"
            "code": "forbidden"
            "title":"The Application can't subscribe to io.cozy.files"
            "source": {"method": "SUBSCRIBE", "payload": {"type":"io.cozy.files"} }
          }}
```

## UNSUBSCRIBE

A client can send an UNSUBSCRIBE request to no longer be notified of changes
from a previous request.

```
{"method": "UNSUBSCRIBE", "payload": {"type": "[desired doctype]"}}
{"method": "UNSUBSCRIBE", "payload": {"type": "[desired doctype]", "id": "idA"}}
```

## Response messages

A message sent by the server after a subscribe will be a JSON object with two
keys at root: `event` and `payload`. `event` will be one of `CREATED`,
`UPDATED`, `DELETED` (when a document is written in CouchDB), `NOTIFIED` (see
below), or `error`. The `payload` will be a map with `type`, `id`, and `doc`.
The `payload` can also contain an optional `old` with the old values for the
document in case of `UPDATED` or `DELETED`.

## Synthetic types

The stack an inject some synthetic events for documents that are not persisted
in CouchDB like classical doctypes:

- [Auth confirmations](https://docs.cozy.io/en/cozy-stack/auth/#real-time-via-websockets)
- [Initial sync for sharings](https://docs.cozy.io/en/cozy-stack/sharing/#real-time-via-websockets)
- [Thumbnails for files](https://docs.cozy.io/en/cozy-stack/files/#real-time-via-websockets)
- [Telepointers for notes](https://docs.cozy.io/en/cozy-stack/notes/#real-time-via-websockets)

## `POST /realtime/:doctype/:id`

This route can be used to send documents in the real-time without having to
persist them in CouchDB (and they can't be used for triggers).

A permission on POST for the document `:doctype/:id` is required to use this
endpoint. For `io.cozy.files`, the rules are checked against the file and its
parent directories, not against the request body.

### Request

```http
POST /realtime/io.cozy.jobs/2c577f00-145a-0138-f569-543d7eb8149c HTTP/1.1
Content-Type: application/json
```

```json
{
  "subtype": "progress",
  "imported": 10,
  "total": 42
}
```

### Response

```http
HTTP/1.1 204 No Content
```

### Websocket

```
server > {"event": "NOTIFIED",
          "payload": {"id": "2c577f00-145a-0138-f569-543d7eb8149c",
                      "type": "io.cozy.jobs",
                      "doc": {"subtype": "progress",
                              "imported": 10,
                              "total": 42}}}
```
