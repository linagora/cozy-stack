[Table of contents](README.md#table-of-contents)

# Intents

-   [Overview](#overview)
-   [Glossary](#glossary)
-   [Proposal](#proposal)
    -   [1. Manifest](#1-manifest)
    -   [2. Intent Start](##2-intent-start)
    -   [3. Service Resolution](#3-service-resolution)
    -   [4. Handshake](#4-handshake)
    -   [5. Processing & Terminating](#5-processing--terminating)
-   [Routes](#routes)
-   [Annexes](#annexes)
    -   [Use cases](#use-cases)
    -   [Bibliography & Prior Art](#bibliography--prior-art)
    -   [Discarded Ideas](#discarded-ideas)

## Overview

A typical Twake Workplace runs multiple applications, but most of these applications
are focused on one task and interact with one particular type of data.

However, Twake Workplace especially shines when data is combined across apps to
provide an integrated experience. This is made difficult by the fact that apps
have no dedicated back-end and that they have restricted access to documents.

This document outlines a proposal for apps to rely on each other to accomplish
certain tasks and gain access to new documents, in a way that hopefully is
neither painful for the users nor the developers.

## Glossary

-   **Intent**: Intents, sometimes also called Activities, is a pattern used in
    environments where multiple apps with different purposes coexist. The idea
    is that any app can express the need to do _something_ that it can't do
    itself, and an app that _can_ do it will take over from there.
-   **Stack**: refers to [cozy-stack](https://github.com/cozy/cozy-stack/), the
    server-side part of the Cozy infrastructure.
-   **Client**: the client is the application that _starts_ an intent.
-   **Service**: the service is the application that _handles_ an intent started
    by a client.
-   **Available apps**: Uninstalled applications that _handles_ the intent.

## Proposal

In this proposal, services declare themselves through their manifest. When a
clients starts an intent, the stack finds the appropriate service and help the
two apps to create a communication channel between them. After the channel is
ready, the intent is processed. These steps are detailed below.

### 1. Manifest

Every app can register itself as a potential handler for one or more intents. To
do so, it must provide the following information for each intent it wishes to
handle:

-   `action`: A verb that describes what the service should do. The most common
    actions are `CREATE`, `EDIT`, `OPEN`, `PICK`, and `SHARE`. While we
    recommend using one of these verbs, the list is not exhaustive and may be
    extended by any app.
-   `type`: One or more types of data on which the service knows how to operate
    the `action`. A `type` can be expressed as a
    [MIME type](https://en.wikipedia.org/wiki/Media_type) or a
    [Cozy Document Type](https://github.com/cozy/cozy-stack/blob/master/docs/data-system.md#typing).
    The application must have permissions for any Cozy Document Type listed
    here. You can also think of the `type` as the intent's subject.
-   `href`: the relative URL of the route designed to handle this intent. A
    query-string with the intent id will be added to this URL.

These informations must be provided in the manifest of the application, inside
the `intents` key.

Here is a very simple example:

```json
"intents": [
    {
        "action": "PICK",
        "type": ["io.cozy.files"],
        "href": "/pick"
    }
]
```

When this service is called, it will load the page
`https://service.domain.example.com/pick?intent=123abc`.

Here is an example of an app that supports multiple data types:

```json
"intents": [
    {
        "action": "PICK",
        "type": ["io.cozy.files", "image/*"],
        "href": "/pick"
    }
]
```

Finally, here is an example of an app that supports several intent types:

```json
"intents": [
    {
        "action": "PICK",
        "type": ["io.cozy.files", "image/*"],
        "href": "/pick"
    },
    {
        "action": "VIEW",
        "type": ["image/gif"],
        "href": "/viewer"
    }
]
```

This information is stored by the stack when the application is installed.

### 2. Intent Start

Any app can start a new intent whenever it wants. When it does, the app becomes
the _client_.

To start an intent, it must specify the following information:

-   `action`: an action verb, which will be matched against the actions
    declared in services manifest files.
-   `type`: a **single** data type, which will be matched against the types
    declared in services manifest files.

There are also two optional fields that can be defined at the start of the
intent:

-   `data`: Any data that the client wants to make available to the service.
    `data` must be a JSON object but its structure is left to the discretion of
    the client. The only exception is when `type` is a MIME type and the client
    wishes to send a file to the service. In that case, the file should be
    represented as a base-64 encoded [Data URL](http://dataurl.net/#about) and
    must be named `content`. This convention is also recomended when dealing
    with other intent `type`s. See the examples below for an example of this.
-   `permissions`: When `type` is a Cozy Document Type and the client expects
    to receive one or more documents as part of the reply from the service, the
    `permissions` field allows the client to request permissions for these
    documents. `permissions` is a list of HTTP Verbs. Refer
    [to this section](https://github.com/cozy/cozy-stack/blob/master/docs/permissions.md#verbs)
    of the permission documentation for more information.

**Note**: if the intent's subject is a Cozy Doctype that holds references to
other Cozy Documents (such as an album referencing photos or a playlist
referencing music files), the permissions should be granted for the referenced
documents too, whenever possible.

Here is an example of what the API could look like:

```js
// "Let the user pick a file"
cozy.intents.start('PICK', 'io.cozy.files')
.then(document => {
    // document is a JSON representation of the picked file
});

// "Create a contact, with some information already filled out"
cozy.intents.start('CREATE', 'io.cozy.contacts', {
    name: 'John Johnsons',
    tel: '+12345678'
    email: 'john@johnsons.com'
})
.then(document => {
    // document is a JSON representation of the contact that was created
});

// "Save this file somewhere"
cozy.intents.start('CREATE', 'io.cozy.files', {
    content: 'data:application/zip;base64,UEsDB...',
    name: 'photos.zip'
});

// "Create a new note, and give me read-only access to it"
cozy.intents.start('CREATE', 'io.cozy.notes', {}, ['GET'])
.then(document => {
    // document is a JSON representation of the note that was created.
    // Additionally, this note can now be retrieved through the API since we have read access on it.
});

// "Create an event based on the provided data, and give me full access to it"
cozy.intents.start('CREATE', 'io.cozy.events', {
    date: '2017/06/24',
    title: 'Beach day'
}, ['ALL'])
.then(document => {
    // document is a JSON representation of the note that was created.
    // Additionally, this note can now be retrieved through the API since we have read access on it.
});

// "Crop this picture"
cozy.intents.start('EDIT', 'image/png', {
    content: 'data:image/png;base64,iVBORw...',
    width: 50,
    height: 50
})
.then(image => {
    //image is the edited version of the image provided above.
})
```

### 3. Service Resolution

The service resolution is the phase where a service is chosen to handle an
intent. This phase is handled by the stack.

After the client has started it's intent, it sends the `action`, the `type` and
the `permissions` to the stack. The stack will then traverse the list of
installed apps and find all the apps that can handle that specific combination.
Note that if the intent request `GET` permissions on a certain Cozy Document
Type, but the service does not have this permission itself, it can not handle
the intent.

The stack then stores the information for that intent:

-   A unique id for that intent
-   Client URL
-   Service URL
-   `action`
-   `type`
-   `permissions`

Finally, the service URL is suffixed with `?intent=` followed by the intent's
id, and then sent to the client.

### 4. Available apps

In addition to the services that manage intents, a list of available (but not
installed) applications of the instance that handles the intent is returned.
This object list is called `availableApps` and contains:
- The application `slug`
- The application `name`

#### Service choice

If more than one service match the intent's criteria, the stack returns the list
of all matching service URLs to the client (and stores it in the service URL
version of the intent it keeps in memory). The client is then free to pick one
arbitrarily.

The client may also decide to let the user choose one of the services. To do
this, it should start another intent with a `PICK` action and a `io.cozy.apps`
`type`. This intent should be resolved by the stack to a special page, in order
to avoid having multiple services trying to handle it and ending up in a loop.

This special page is a service like any other; it expects the list of services
to pick from as input data, and will return the one that has been picked to the
client. The client can then proceed with the first intent.

The user may decide to abort the intent before picking a service. If that is the
case, the choice page will need to inform the client that the intent was
aborted.

#### No available service

If no service is available to handle an intent, the stack returns an error to
the client. However, some non-installed applications may handle the intent, and
are listed in the `availableApps` field.

### 4. Handshake

The next phase consist of the client and the service establishing a
communication channel between them. The communication will be done using the
[window.postMessage](https://developer.mozilla.org/fr/docs/Web/API/Window/postMessage)
API.

#### Service Initialization

When the client receives the service URL from the stack, it starts to listen for
messages coming from that URL. Once the listener is started, it opens an iframe
that points to the service's URL.

#### Service to Client

At this point, the service app is opened on the route that it provided in the
`href` part of it's manifest. This route now also contains the intent's id.

The service queries the stack to find out information about the intent, passing
along the intent id. In response, the stack sends the client's URL, the
`action`, and the `type`. If the intent includes `permissions`, the stack sends
them too, as well as the client's permission id.

It then starts to listen for messages coming from the client's URL. Eventually,
it sends a message to the client, as a mean to inform it that the service is now
ready.

#### Client to Service

After the client receives the "ready" message from the service, it sends a
message to the service acknowledging the "ready" state.

Along with this message, it should send the `data` that was provided at the
start of the intent, if any.

### 5. Processing & Terminating

After this handshake, there is a confirmed communication channel between the
client and the service, and the service knows what it has to do. This is the
phase where the user interacts with the service.

If the service is going to grant extra permissions to the client app, it is
strongly recommended to make this clear to the user.

When the service has finished his task, it sends a "completed" message to the
client. Permissions extensions should have been done before that. Along with the
completed message, the service should send any data it deems relevant. This data
should be a JSON object. Again, the structure of that object is left to the
discretion of the service, except when `type` is a MIME type. In that case, the
file should be represented as a base-64 encoded
[Data URL](http://dataurl.net/#about) and must be named `content`. This
convention is also recomended when dealing with other intent `type`s.

After the client receives a "completed" message, it can close the service's
iframe and resume operations as usual.

If, for whatever reason, the service can not fulfill the intent, it can send an
"error" message to the client. When the client receives an "error" message, the
intent is aborted and the iframe can be closed.

## Routes

### POST /intents

The client app can ask to start an intent via this route.

Any client-side app can call this route, no permission is needed.

#### Request

```
POST /intents HTTP/1.1
Host: cozy.example.net
Authorization: Bearer eyJhbG...
Content-Type: application/vnd.api+json
Accept: application/vnd.api+json
```

```json
{
    "data": {
        "type": "io.cozy.intents",
        "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"]
        }
    }
}
```

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/vnd.api+json
```

```json
{
    "data": {
        "id": "77bcc42c-0fd8-11e7-ac95-8f605f6e8338",
        "type": "io.cozy.intents",
        "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"],
            "client": "https://contacts.cozy.example.net",
            "services": [
                {
                    "slug": "files",
                    "href": "https://files.cozy.example.net/pick?intent=77bcc42c-0fd8-11e7-ac95-8f605f6e8338"
                }
            ],
            "availableApps": [
                {
                    "slug": "myapp",
                    "name": "My App"
                }
            ]
        },
        "links": {
            "self": "/intents/77bcc42c-0fd8-11e7-ac95-8f605f6e8338",
            "permissions": "/permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6"
        }
    }
}
```

### GET /intents/:id

Get all the informations about the intent

**Note**: only the service can access this route (no permission involved).

#### Request

```http
GET /intents/77bcc42c-0fd8-11e7-ac95-8f605f6e8338 HTTP/1.1
Host: cozy.example.net
Authorization: Bearer J9l-ZhwP...
Content-Type: application/vnd.api+json
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
        "id": "77bcc42c-0fd8-11e7-ac95-8f605f6e8338",
        "type": "io.cozy.intents",
        "attributes": {
            "action": "PICK",
            "type": "io.cozy.files",
            "permissions": ["GET"],
            "client": "https://contacts.cozy.example.net",
            "services": [
                {
                    "slug": "files",
                    "href": "https://files.cozy.example.net/pick?intent=77bcc42c-0fd8-11e7-ac95-8f605f6e8338"
                }
            ],
            "availableApps": [
                {
                    "slug": "myapp",
                    "name": "My App"
                }
            ]
        },
        "links": {
            "self": "/intents/77bcc42c-0fd8-11e7-ac95-8f605f6e8338",
            "permissions": "/permissions/a340d5e0-d647-11e6-b66c-5fc9ce1e17c6"
        }
    }
}
```

## Mobile + WebView flow

When the client app is a native mobile app and the service app must be loaded in a
WebView, the browser-shared cookie is not available. The mobile app can authenticate
the WebView by reusing the existing `session_code` mechanism.

### Flow

```
Mobile app                cozy-stack                WebView (service app)
----------  --------------  ----------------------   ----------------------
[token_exchange]  ──POST /auth/token_exchange──────▶
                   ◀── app-scoped access_token ────
[create intent]    ──POST /intents  (Bearer app token)────▶
                   ◀── intent doc: id + service href ────
[mint session_code]──POST /auth/session_code (Bearer app token)──▶
                   ◀── {"session_code": "<code>"} ────
[open WebView]     ── load https://<service>.<inst>/?intent=<id>&session_code=<code> ────▶
                                                    ServeAppFile:
                                                    1. CheckAndClearSessionCode → ok
                                                    2. SetCookieForNewSession(NormalRun)
                                                    3. redirect to ?intent=<id> (code stripped)
                                                    4. on reload: serve app with cozyData.token
                   ◀── index.html with cozyData.token = <app token> ────
                                                    cozy-client in WebView uses app token
                                                    for all API calls (scoped to service slug)
```

### Prerequisites

- The instance must have OIDC configured with `allow_app_token_exchange: true` and
  at least one entry under `app_token_exchange` in the context configuration.
- The caller app must be a linked app listed in that configuration.
- The service app must be installed on the instance.

### API calls

1. **Token exchange** — `POST /auth/token_exchange` with `exchange_type: "app"` and
   a valid OIDC `id_token`. Returns an OAuth access token scoped to the linked app.
2. **Create intent** — `POST /intents` with the app-scoped token. Returns the intent
   document including the service app's href with `?intent=<id>`.
3. **Mint session_code** — `POST /auth/session_code` with the same app-scoped token
   as Bearer. Returns `{"session_code": "<code>"}`.
4. **Open WebView** — load `https://<serviceSlug>.<instance>/...?intent=<id>&session_code=<code>`.
   The stack consumes the code, creates a session cookie, and redirects (code
   stripped). The WebView now has a full user session for the instance domain.

### Security

- The `session_code` is single-use (atomic delete in Redis on read) with a 7-day TTL.
- The cookie is a `NormalRun` session (browser-session, 30-day max age) cleared
  when the WebView is destroyed.
- The service page only receives a service-app-scoped token via `BuildAppToken`;
  the underlying cookie grants a full user session, but the page code never
  accesses it directly.
- Only tokens minted by `token_exchange` for a currently configured linked app
  can mint session codes. Revoking the app from `app_token_exchange` immediately
  prevents new session code minting.

## Annexes

### Use Cases

Here is a non exhaustive list of situations that _may_ use intents:

-   Configure a new connector account
-   Share a photo album via email
-   Add an attachment to an email
-   Attach a note to a contact
-   Create a contact based on an email
-   Save an attachment received in an email
-   Create a birthday event in a calendar, based on a contact
-   Create an event based on an email's content
-   Create an event based on an ICS file
-   Chose an avatar for a contact
-   Open a file from the file browser (music, PDF, image, ...)
-   Attach a receipt to an expense
-   Provide a tip for another application

### Bibliography & Prior Art

-   [Prior art](https://forum.cozy.io/t/cozy-tech-topic-inter-app-communication-architecture/2287)
-   [Web intents](http://webintents.org/)
-   [WebActivities](https://wiki.mozilla.org/WebAPI/WebActivities) and [WebActivities on FirefoxOS (archive)](https://developer.mozilla.org/en-US/docs/Archive/Firefox_OS/API/Web_Activities)
-   [Siri Intents](https://developer.apple.com/documentation/sirikit#//apple_ref/doc/uid/TP40016875-CH5-SW1)
-   [Android Intents](https://developer.android.com/reference/android/content/Intent.html)
-   [iOS extensions](https://developer.apple.com/library/content/documentation/General/Conceptual/ExtensibilityPG/index.html)

### Discarded Ideas

#### Disposition

Some specifications include a `disposition` field in the manifests, that gives a
hint to the client about how to display the service (inlined or in a new
window). Since we were unable to find a use case where a new window is required,
we decided not to include the `disposition` in this specification. It might be
added later if the need arises.

#### Client / Server architecture

Instead of letting the two applications communicate with each other, they could
be made to talk through the stack. This would be useful as the stack could act
as a middleware, transforming data on the fly or granting permissions where
appropriate.

However, this approach has also severe drawbacks, notably the fact that the
stack must hold a copy of all the data that the apps want to transfer (which can
include large files). It also makes it significantly harder for the client to
know when the intent has been processed.

#### Data representation

The client could explicitly request a data format to be used in the
communication. However, this idea was abandoned because the format is _always_
json, except when then intent's `type` is a MIME type, in which case the data
format _also_ uses this type.
