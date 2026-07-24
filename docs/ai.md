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

For the moment, the feature is experimental, and a trigger must be created
manually on the Cozy:

```sh
$ COZY=cozy.localhost:8080
$ TOKEN=$(cozy-stack instances token-cli $COZY io.cozy.triggers)
$ curl "http://${COZY}/jobs/triggers" -H "Authorization: Bearer $TOKEN" -d '{ "data": { "attributes": { "type": "@event", "arguments": "io.cozy.files", "debounce": "1m", "worker": "rag-index", "message": {"doctype": "io.cozy.files"} } } }'
```

It can also be a good idea to start a first indexation with:

```sh
$ cozy-stack triggers launch --domain $COZY $TRIGGER_ID
```

In practice, when files are uploaded/modified/deleted, the trigger will create
a job for the index worker (with debounce). The index worker will look at the
changed feed, and will call the RAG for each entry in the changes feed.

By default, only text-based files are indexed. Images, videos, and audio files
can be indexed by enabling the following feature flags:

- `rag.index.image.enabled`
- `rag.index.video.enabled`
- `rag.index.audio.enabled`

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
  document. When set, the response includes a `relationships` block. If the assistant has a
  non-empty `prompt` field, a `role: "system"` message carrying that prompt is prepended to
  `messages` when the conversation is created (before the user's own message). Clients should
  not render `system` messages as regular conversation turns.
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
