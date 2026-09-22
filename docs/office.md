[Table of contents](README.md#table-of-contents)

# Collaborative edition of Office and PDF documents

## Diagrams

### Opening a document with OnlyOffice

Reference: https://api.onlyoffice.com/editors/open

![Opening a document with OnlyOffice](diagrams/onlyoffice-open.png)

1. The browser makes a request `GET /office/:id/open` to know the address of the OnlyOffice server
2. The browser makes several HTTP requests to the Document Server
    1. Fetch the `api.js` script
    2. Open a websocket connection
    3. Send a command to load the office document
3. The document server makes a request to the [callback URL](https://api.onlyoffice.com/editors/callback#status-1) with `status=1`
4. The document server asks the file converter to load the document (via RabbitMQ?)
5. The file converter loads the file content from the stack

### Saving a document with OnlyOffice

Reference: https://api.onlyoffice.com/editors/save

![Saving a document with OnlyOffice](diagrams/onlyoffice-save.png)

1. The browser makes requests to edit the document and close the connection when done
2. The document server waits a bit after all clients have disconnected, and send a request to the callback URL
3. The stack downloads the file from the document server and saves it

## Routes

> **Note:** For local development setup, see [Docker - OnlyOffice Document Server](docker.md#onlyoffice-document-server).

### GET /office/:id/open

This route returns the parameters to open an office document or a PDF. There
are two cases:

1. The document is shared and should be opened on another instance (for
   collaborative edition)
2. The document can be opened locally.

In the first case, the response will contain the parameters of the other
instance. In the second case, the parameters are for the document server of
OnlyOffice.

If the identifier doesn't give an office document or PDF, or if there is no
OnlyOffice server configured, the response will be a `404 Page not found`.

#### Request

```http
GET /office/32e07d806f9b0139c541543d7eb8149c/open HTTP/1.1
Host: bob.cozy.example
```

#### Response (case 1)

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.office.url",
    "id": "32e07d806f9b0139c541543d7eb8149c",
    "attributes": {
      "document_id": "b05e7c306f9c0139c542543d7eb8149c",
      "subdomain": "flat",
      "protocol": "https",
      "instance": "alice.cozy.example",
      "sharecode": "543d7eb8149c",
      "public_name": "Bob"
    }
  }
}
```

#### Response (case 2)

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.office.url",
    "id": "32e07d806f9b0139c541543d7eb8149c",
    "attributes": {
      "document_id": "32e07d806f9b0139c541543d7eb8149c",
      "subdomain": "flat",
      "protocol": "https",
      "instance": "bob.cozy.example",
      "public_name": "Bob",
      "onlyoffice": {
        "url": "https://documentserver/",
        "token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.e30.t-IDcSemACt8x4iTMCda8Yhe3iZaWbvV5XKSTbuAn0M",
        "documentType": "word",
        "document": {
          "filetype": "docx",
          "key": "7c7ccc2e7137ba774b7e44de",
          "title": "Letter.docx",
          "url": "https://bob.cozy.example/files/downloads/735e6cf69af2db82/Letter.docx?Dl=1",
          "permissions": {
            "edit": true
          },
          "info": {
            "owner": "Bob",
            "uploaded": "2010-07-07 3:46 PM"
          }
        },
        "editorConfig": {
          "callbackUrl": "https://bob.cozy.example/office/callback",
          "lang": "en",
          "mode": "edit"
        }
      }
    }
  }
}
```

PDF files use `documentType: "pdf"` and `document.filetype: "pdf"`. For all
supported formats, `document.permissions.edit` is `true` only when the
`editorConfig.mode` is `"edit"`; it is `false` in read-only mode.

### POST /office/keys/:key

If a document is being edited while a new version is uploaded (via the desktop
for example), the OO webapp should call this endpoint if the user chooses to
continue editing the version on which they were working. A conflict file is
created, so that no work is lost.

#### Request

```http
POST /office/keys/7c7ccc2e7137ba774b7e44de HTTP/1.1
Host: bob.cozy.example
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
    "id": "32e07d806f9b0139c541543d7eb8149c",
    "meta": {
      "rev": "3-18c04daba326"
    },
    "attributes": {
      "type": "file",
      "name": "slideshow (2).pptx",
      "trashed": false,
      "md5sum": "ODZmYjI2OWQxOTBkMmM4NQo=",
      "created_at": "2023-09-30T21:42:05Z",
      "updated_at": "2023-09-30T22:38:04Z",
      "tags": [],
      "metadata": {},
      "size": 12345,
      "executable": false,
      "class": "slide",
      "mime": "application/vnd.ms-powerpoint",
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2023-09-30T21:42:05Z",
        "createdByApp": "drive",
        "createdOn": "https://bob.cozy.example/",
        "updatedAt": "2023-09-30T22:38:04Z",
        "uploadedAt": "2023-09-30T22:38:04Z",
        "uploadedOn": "https://bob.cozy.example/",
        "uploadedBy": {
          "slug": "onlyoffice-server"
        }
      }
    }
  }
}
```

### POST /office/callback

This is the callback handler for OnlyOffice. It is called when the document
server wants to save the file.

The same callback and VFS save path are used for Office documents and PDFs.

See https://api.onlyoffice.com/editors/callback

#### Request

```http
POST /office/callback HTTP/1.1
Host: bob.cozy.example
Content-Type: application/json
```

```json
{
    "actions": [{"type": 0, "userid": "78e1e841"}],
    "changesurl": "https://documentserver/url-to-changes.zip",
    "key": "7c7ccc2e7137ba774b7e44de",
    "status": 2,
    "url": "https://documentserver/url-to-edited-document.docx",
    "users": ["6d5a81d0"]
}
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{ "error": 0 }
```
