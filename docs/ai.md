[Table of contents](README.md#table-of-contents)

# AI for personal data

## Introduction

AI can be used for interacting with the personal data of a Cozy. This is
currently an experimental feature. Retrieval-Augmented Generation (RAG) is
a classical pattern in the AI world. Here, it is specific to each Cozy.

![Architecture with a RAG server](diagrams/ai.svg)

## Indexation

First of all, the RAG server must be installed with its dependencies. It is
not mandatory to install them on the same servers as the cozy-stack. And the
URL of RAG must be filled in cozy-stack configuration file (in `rag`).

The indexing is driven by `@event` triggers on the `rag-index` worker. The
app in charge of the AI assistants creates them; the worker is not reserved,
so an app token with the right permissions is enough:

```json
"rag-index": {
  "type": "io.cozy.triggers",
  "verbs": ["GET", "POST", "DELETE"],
  "selector": "worker",
  "values": ["rag-index"]
},
"rag-jobs": {
  "type": "io.cozy.jobs",
  "verbs": ["POST"],
  "selector": "worker",
  "values": ["rag-index"]
}
```

### Global indexing

A trigger whose message has no `dir_id` indexes every file of the instance:

```json
{
  "data": {
    "attributes": {
      "type": "@event",
      "arguments": "io.cozy.files",
      "debounce": "1m",
      "worker": "rag-index",
      "message": { "doctype": "io.cozy.files" }
    }
  }
}
```

### Folder-scoped indexing (knowledge bases)

A trigger whose message has a `dir_id` indexes only the files under that
folder, recursively, and attaches them to the openRAG workspace whose id is
the folder id. The workspace is created by the worker on its first run. The
assistant using the folder as its knowledge base has its retrieval scoped
to that workspace.

```json
{
  "data": {
    "attributes": {
      "type": "@event",
      "arguments": "io.cozy.files:CREATED,UPDATED,DELETED:<dirID>",
      "debounce": "1m",
      "worker": "rag-index",
      "message": { "doctype": "io.cozy.files", "dir_id": "<dirID>" }
    }
  }
}
```

The `<dirID>` filter in `arguments` is an optimization: the worker reads the
changes feed from its own checkpoint anyway, so a change missed by the
filter (for example one replicated by a sharing) is processed on the next
run. `"arguments": "io.cozy.files"` works too, at the cost of a run on
every file event.

Files that leave the folder are detached from the workspace, and deleted
from openRAG when no other workspace and no global trigger claims them.
Nested folders may each have their own trigger.

The app is expected to:

- on first launch, list `GET /jobs/triggers?Worker=rag-index` and create
  what is missing, then launch each new trigger with
  `POST /jobs/triggers/:trigger-id/launch` to start the initial indexing;
- on assistant creation, create and launch the scoped trigger of its
  knowledge base folder (unless one exists: several assistants may share a
  folder);
- when an assistant is deleted or its folder changes, push a `remove` job
  for the old folder (unless another assistant still uses it) and delete
  its trigger:

```http
POST /jobs/queue/rag-index
```

```json
{
  "data": {
    "attributes": {
      "arguments": { "doctype": "io.cozy.files", "dir_id": "<dirID>", "action": "remove" }
    }
  }
}
```

The `remove` job detaches the folder's files, deletes the unclaimed ones,
and drops the workspace.

By default, only text-based files are indexed. Images, videos, and audio files
can be indexed by enabling the following feature flags:

- `rag.index.image.enabled`
- `rag.index.video.enabled`
- `rag.index.audio.enabled`

### Operator tools

- `cozy-stack rag reset <domain> [--dir-id <id>]` deletes the checkpoint of
  the rag-index trigger(s) and launches them: the whole changes feed is
  scanned again.
- `cozy-stack rag purge <domain>` deletes from openRAG the files no trigger
  claims: files gone or trashed in the Cozy, and files outside every scoped
  folder when there is no global trigger. Use it after switching an
  instance from a global trigger to scoped ones, or when a `remove` job was
  never pushed.

A trigger can still be created by hand with a CLI token:

```sh
$ COZY=cozy.localhost:8080
$ TOKEN=$(cozy-stack instances token-cli $COZY io.cozy.triggers)
$ curl "http://${COZY}/jobs/triggers" -H "Authorization: Bearer $TOKEN" -d '{ "data": { "attributes": { "type": "@event", "arguments": "io.cozy.files", "debounce": "1m", "worker": "rag-index", "message": {"doctype": "io.cozy.files"} } } }'
```

### POST /ai/index/status

The RAG indexer reports the indexation status of a document on this route. The
status is saved on an `io.cozy.ai.chat.rag` document whose identifier is the
identifier of the document it describes. Only files are indexed for now.

This route has no authentication yet.

#### Request

```http
POST /ai/index/status HTTP/1.1
Host: alice.example.net
Content-Type: application/json
```

```json
{
  "partition": "alice.example.net",
  "file_id": "e21dce8058b9013d800a18c04daba326",
  "status": "success",
  "metadata": {
    "doc_rev": "3-6a1b0b8a51a4e0e0a3b7f0f1d2c3b4a5",
    "datetime": "2026-08-20T08:12:00.000Z",
    "doctype": "io.cozy.files"
  }
}
```

The `status` can be `success`, `error` or `notsupported`. The `doc_rev` is the
revision of the document the status is about, as it was given to the indexer. It
is mandatory: callbacks are ordered on it, and a callback that carries none
cannot be placed. It is saved on the status document as `docRev`, so that a
client can tell whether the current revision of the document is the one
described.

The indexer echoes back more than `doc_rev`, but the other fields are ignored.

#### Response

```http
HTTP/1.1 204 No Content
```

A callback is answered with a `400 Bad Request` when its payload is invalid or
when its partition is not this instance, and with a `500 Internal Server Error`
when the status could not be saved.

A callback about a revision older than the one already saved is accepted but not
saved, and answered with a `204`. One about the same revision describes the same
indexation and is saved.

## openRAG

Some openRAG API are directly exposed through cozy-stack.
Note the JSON-API format is not used here as we follow the openRAG format.

### POST /ai/v1/chat/completions

This route directly follows the [openAI chat completion AI](https://platform.openai.com/docs/api-reference/chat/create).

#### Request

```http
POST /ai/v1/chat/completions HTTP/1.1
Content-Type: application/json
```

```json
{
  "messages": [
    { "role": "user", "content": "Hello there, what's in your mind?" }
  ],
  "temperature": 0.3
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "id": "chatcmpl-43036e48fbac40fead606e8692a7b408",
  "created": 1763657211,
  "model": null,
  "object": "chat.completion",
  "system_fingerprint": null,
  "choices": [
    {
      "finish_reason": "stop",
      "index": 0,
      "message": {
        "content": "As an artificial intelligence language model, I don't have personal thoughts or emotions like humans do. My purpose is to assist and provide information to the best of my abilities based on the data I have been trained on. Is there something specific you would like to know or discuss?",
        "role": "assistant",
        "tool_calls": null,
        "function_call": null
      }
    }
  ],
  "usage": {
    "completion_tokens": 56,
    "prompt_tokens": 28,
    "total_tokens": 84,
    "completion_tokens_details": null,
    "prompt_tokens_details": null
  },
  "service_tier": null,
  "prompt_logprobs": null,
  "extra": "{\"sources\": []}"
}
```

### POST /ai/v1/tools/execute

This route directly calls the [openRAG](https://github.com/linagora/openrag) tools API.

#### Request

POST /ai/v1/tools/execute HTTP/1.1
Content-Type: multipart/form-data

```
file=<file content>
tool={"name": "extractText"}
metadata={"mime":"application/pdf","name":"myfile.pdf"}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "message": "Some file content"
}
```


## Assistant chat

When a user starts a chat from the assistant, their prompts are sent to the RAG that can use the
vector database to find relevant documents (technically, only some parts of
the documents called chunks). Those documents are added to the prompt, so
that the LLM can use them as a context when answering.

### POST /ai/chat/conversations/:id

This route can be used to ask AI for a chat completion. The id in the path
must be the identifier of a chat conversation. The client can generate a random
identifier for a new chat conversation.

The stack will respond after pushing a job for this task, but without the
response. The client must use the real-time websocket and subscribe to
`io.cozy.ai.chat.events`.

#### Request

```http
POST /ai/chat/conversations/e21dce8058b9013d800a18c04daba326 HTTP/1.1
Content-Type: application/json
```

```json
{
  "q": "Why the sky is blue?",
  "stream": true,
  "websearch": false,
  "assistantID": "abc123",
  "attachmentIDs": ["827f0fbb928b375cc457c732a4013aa7", "9a3b1c2d3e4f5a6b7c8d9e0f1a2b3c4d"]
}
```

- `q` is the user's message (required).
- `stream` enables streaming the response via SSE deltas (defaults to `true`).
- `websearch` enables web search for the query (defaults to `false`).
- `assistantID` (optional) associates the conversation with an `io.cozy.ai.chat.assistants`
  document. When set, the response includes a `relationships` block.
  When the assistant has a knowledge base folder, the retrieval is scoped to
  that folder's workspace. The folder must have a scoped `rag-index` trigger
  that has run at least once, otherwise the query fails with an error event
  saying the knowledge base is not indexed.
- `attachmentIDs` (optional) array of ids, specifying which documents should be leveraged by the RAG.
  

#### Response

```http
HTTP/1.1 202 Accepted
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.ai.chat.conversations",
    "id": "e21dce8058b9013d800a18c04daba326",
    "rev": "1-23456",
    "attributes": {
      "messages": [
        {
          "id": "eb17c3205bf1013ddea018c04daba326",
          "role": "user",
          "content": "Why the sky is blue?",
          "attachmentIDs": ["827f0fbb928b375cc457c732a4013aa7", "9a3b1c2d3e4f5a6b7c8d9e0f1a2b3c4d"],
          "createdAt": "2024-09-24T13:24:07.576Z"
        }
      ],
      "relationships": {
        "assistant": {
          "data": {
            "id":"b2e1a4144c123ec694697d996102983a",
            "type":"io.cozy.ai.chat.assistants"
          }
        }
      },
      "cozyMetadata": {
        "createdAt": "2024-09-24T13:24:07.576Z",
        "createdOn": "http://cozy.localhost:8080/",
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "updatedAt": "2024-09-24T13:24:07.576Z"
      }
    }
  }
}
```

### Real-time via websockets

#### Messages flow example

```
client > {"method": "AUTH", "payload": "token"}
client > {"method": "SUBSCRIBE",
          "payload": {"type": "io.cozy.ai.chat.events"}}
server > {"event": "CREATED",
          "payload": {"id": "eb17c3205bf1013ddea018c04daba326",
                      "type": "io.cozy.ai.chat.events",
                      "doc": {"object": "delta", "content": "The ", "position": 0}}}
server > {"event": "CREATED",
          "payload": {"id": "eb17c3205bf1013ddea018c04daba326",
                      "type": "io.cozy.ai.chat.events",
                      "doc": {"object": "delta", "content": "sky ", "position": 1}}}
[...]
server > {"event": "CREATED",
          "payload": {"id": "eb17c3205bf1013ddea018c04daba326",
                      "type": "io.cozy.ai.chat.events",
                      "doc": {"object": "sources", "content": [{"id": "827f0fbb928b375cc457c732a4013aa7", "doctype": "io.cozy.files"}]}}}
server > {"event": "CREATED",
          "payload": {"id": "eb17c3205bf1013ddea018c04daba326",
                      "type": "io.cozy.ai.chat.events",
                      "doc": {"object": "done"}}}
```

#### Error message

If an error occurs while processing the AI response (e.g. the LLM is
unavailable), an error event is sent instead:

```
server > {"event": "CREATED",
          "payload": {"id": "eb17c3205bf1013ddea018c04daba326",
                      "type": "io.cozy.ai.chat.events",
                      "doc": {"object": "error", "message": "I don't want to talk today"}}}
```
