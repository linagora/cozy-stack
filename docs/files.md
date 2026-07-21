[Table of contents](README.md#table-of-contents)

# Virtual File System

Cozy applications can use files for storing binary content, like photos or bills
in PDF. This service offers a REST API to manipulate easily without having to
know the underlying storage layer. The metadata are kept in CouchDB, but the
binaries can go to the local system, or a Swift instance.

## Directories

A directory is a container for files and sub-directories.

Its path is the path of its parent, a slash (`/`), and its name. It's case
sensitive.

### Root directory

The root of the virtual file system is a special directory with id
`io.cozy.files.root-dir`.

You can use it in any request where you would use a directory, except you cannot
delete it.

### POST /files/:dir-id

Create a new directory. The `dir-id` parameter is optional. When it's not given,
the directory is created at the root of the virtual file system.

#### Query-String

| Parameter  | Description                         |
| ---------- | ----------------------------------- |
| Type       | `directory`                         |
| Name       | the directory name                  |
| Tags       | an array of tags                    |
| CreatedAt  | the creation date                   |
| MetadataID | the identifier of a metadata object |

#### HTTP headers

| Parameter | Description                            |
| --------- | -------------------------------------- |
| Date      | The modification date of the directory |

#### Request

```http
POST /files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81?Type=directory&Name=phone&Tags=bills,konnectors HTTP/1.1
Accept: application/vnd.api+json
Date: Mon, 19 Sep 2016 12:35:08 GMT
```

#### Status codes

- 201 Created, when the directory has been successfully created
- 404 Not Found, when the parent directory does not exist
- 409 Conflict, when a directory with the same name already exists
- 422 Unprocessable Entity, when the `Type`, `Name`, or `MetadataID` parameter
  is missing or invalid

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/vnd.api+json
Location: https://cozy.example.com/files/6494e0ac-dfcb-11e5-88c1-472e84a9cbee
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "6494e0ac-dfcb-11e5-88c1-472e84a9cbee",
    "meta": {
      "rev": "1-ff3beeb456eb"
    },
    "attributes": {
      "type": "directory",
      "name": "phone",
      "path": "/Documents/phone",
      "created_at": "2016-09-19T12:35:08Z",
      "updated_at": "2016-09-19T12:35:08Z",
      "tags": ["bills", "konnectors"],
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2016-09-20T18:32:48Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2016-09-20T18:32:48Z"
      }
    },
    "relationships": {
      "parent": {
        "links": {
          "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        }
      },
      "not_synchronized_on": {
        "links": {
          "self": "/files/6494e0ac-dfcb-11e5-88c1-472e84a9cbee/relationships/not_synchronized_on"
        },
        "data": [
          {
            "type": "io.cozy.oauth.clients",
            "id": "653dfdb0-0595-0139-92df-543d7eb8149c"
          }
        ]
      }
    },
    "links": {
      "self": "/files/6494e0ac-dfcb-11e5-88c1-472e84a9cbee"
    }
  }
}
```

**Note**: see [not synchronized directories](not-synchronized-vfs.md) for
more informations about the `not_synchronized_on` field.

### POST /files/shared-drives

This endpoint returns the information about the Shared Drives directory. If the
directory does not exist, it is created.

#### Request

```http
POST /files/shared-drives
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "io.cozy.files.shared-drives-dir",
    "meta": {
      "rev": "1-e4abdb5a"
    },
    "attributes": {
      "type": "directory",
      "name": "Drives",
      "path": "/Drives",
      "created_at": "2024-03-25T15:22:00Z",
      "updated_at": "2024-03-25T15:22:00Z",
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2024-03-25T15:22:00Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2024-03-25T15:22:00Z"
      }
    },
    "relationships": {
      "parent": {
        "links": {
          "related": "/files/io.cozy.files.root-dir"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "io.cozy.files.root-dir"
        }
      }
    },
    "links": {
      "self": "/files/io.cozy.files.shared-drives-dir"
    }
  }
}
```

### GET /files/:file-id

Get a directory or a file informations. In the case of a directory, it contains
the list of files and sub-directories inside it. For a note, its images are
included.

Contents is paginated following [jsonapi conventions](./http-api.md#pagination).
The default limit is 30 entries.

#### Request

```http
GET /files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81 HTTP/1.1
Accept: application/vnd.api+json
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "links": {
    "next": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81?page[cursor]=9152d568-7e7c-11e6-a377-37cbfb190b4b"
  },
  "data": {
    "type": "io.cozy.files",
    "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81",
    "meta": {
      "rev": "1-e36ab092"
    },
    "attributes": {
      "type": "directory",
      "name": "Documents",
      "path": "/Documents",
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
      }
    },
    "relationships": {
      "contents": {
        "data": [
          {
            "type": "io.cozy.files",
            "id": "6494e0ac-dfcb-11e5-88c1-472e84a9cbee"
          },
          {
            "type": "io.cozy.files",
            "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b"
          }
        ]
      }
    },
    "links": {
      "self": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
    }
  },
  "included": [
    {
      "type": "io.cozy.files",
      "id": "6494e0ac-dfcb-11e5-88c1-472e84a9cbee",
      "meta": {
        "rev": "1-ff3beeb456eb"
      },
      "attributes": {
        "type": "directory",
        "name": "phone",
        "path": "/Documents/phone",
        "created_at": "2016-09-19T12:35:08Z",
        "updated_at": "2016-09-19T12:35:08Z",
        "tags": ["bills", "konnectors"],
        "cozyMetadata": {
          "doctypeVersion": "1",
          "metadataVersion": 1,
          "createdAt": "2016-09-20T18:32:47Z",
          "createdByApp": "drive",
          "createdOn": "https://cozy.example.com/",
          "updatedAt": "2016-09-20T18:32:47Z"
        }
      },
      "relationships": {
        "parent": {
          "links": {
            "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
          },
          "data": {
            "type": "io.cozy.files",
            "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
          }
        }
      },
      "links": {
        "self": "/files/6494e0ac-dfcb-11e5-88c1-472e84a9cbee"
      }
    },
    {
      "type": "io.cozy.files",
      "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
      "meta": {
        "rev": "1-0e6d5b72"
      },
      "attributes": {
        "type": "file",
        "name": "hello.txt",
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
      },
      "relationships": {
        "parent": {
          "links": {
            "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
          },
          "data": {
            "type": "io.cozy.files",
            "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
          }
        }
      },
      "links": {
        "self": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b"
      }
    }
  ]
}
```

### GET /files/:file-id/size

This endpoint returns the size taken by the files in a directory, including
those in subdirectories.

#### Request

```http
GET /files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81/size HTTP/1.1
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
    "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81",
    "attributes": {
      "size": "1234567890"
    },
    "meta": {}
  }
}
```

### POST `/files/_all_docs`

This route allows to fetch several files in one request. It is the same as the
`_all_docs` request for CouchDB, except the response is in the JSON-API format,
(with thumbnails and path for the files).

### Request

```http
POST /files/_all_docs HTTP/1.1
```

```json
{
  "keys": ["e8c1561846c730428180a5f6c6107914", "e8c1561846c730428180a5f6c6109007"]
}
```

### Response

```http
HTTP/1.1 200 OK
Date: Mon, 27 Sept 2016 12:28:53 GMT
Content-Length: ...
Content-Type: application/json
```

```json
{
  "data": [
    {
      "type": "io.cozy.files",
      "id": "e8c1561846c730428180a5f6c6107914",
      "attributes": {
        "type": "file",
        "name": "nicepic1.jpg",
        "dir_id": "f49b4087cbf946dfc759214394009a6c",
        "created_at": "2020-02-13T16:35:47.568155477+01:00",
        "updated_at": "2020-02-13T16:35:47.568155477+01:00",
        "size": "345385",
        "md5sum": "12cGYwT+RiNjFxf4f7AmzQ==",
        "mime": "image/jpeg",
        "class": "image",
        "executable": false,
        "trashed": false,
        "tags": [],
        "path": "/Pictures/nicepic1.jpg",
        "metadata": {
          "datetime": "2020-02-13T16:35:47.568155477+01:00",
          "extractor_version": 2,
          "height": 1080,
          "width": 1920
        }
      },
      "meta": {
        "rev": "2-235e715b1d82a93285be1b0bd691b779"
      },
      "links": {
        "self": "/files/e8c1561846c730428180a5f6c6107914",
        "tiny": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/tiny",
        "small": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/small",
        "medium": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/medium",
        "large": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/large"
      },
      "relationships": {
        "parent": {
          "links": {
            "related": "/files/f49b4087cbf946dfc759214394009a6c"
          },
          "data": {
            "id": "f49b4087cbf946dfc759214394009a6c",
            "type": "io.cozy.files"
          }
        }
      }
    },
    {
      "type": "io.cozy.files",
      "id": "e8c1561846c730428180a5f6c6109007",
      "attributes": {
        "type": "file",
        "name": "nicepic2.jpg",
        "dir_id": "f49b4087cbf946dfc759214394009a6c",
        "created_at": "2020-02-13T16:35:47.845049743+01:00",
        "updated_at": "2020-02-13T16:35:47.845049743+01:00",
        "size": "323009",
        "md5sum": "Fla3ucNXuW2Xw/TK8pfsPA==",
        "mime": "image/jpeg",
        "class": "image",
        "executable": false,
        "trashed": false,
        "tags": [],
        "path": "/Pictures/nicepic2.jpg",
        "metadata": {
          "datetime": "2020-02-13T16:35:47.845049743+01:00",
          "extractor_version": 2,
          "height": 1080,
          "width": 1920
        }
      },
      "meta": {
        "rev": "2-4883d6b8ccad32f8fb056af9b7f8b37f"
      },
      "links": {
        "self": "/files/e8c1561846c730428180a5f6c6109007",
        "tiny": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/tiny",
        "small": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/small",
        "medium": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/medium",
        "large": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/large"
      },
      "relationships": {
        "parent": {
          "links": {
            "related": "/files/f49b4087cbf946dfc759214394009a6c"
          },
          "data": {
            "id": "f49b4087cbf946dfc759214394009a6c",
            "type": "io.cozy.files"
          }
        }
      }
    }
  ]
}
```



### GET `/files/_changes`

This endpoint is similar to the changes feed of CouchDB for io.cozy.files.
There are a few specificities:

- it is wrapped in the VFS lock to avoid seeing inconsistent results
- it automatically skips design docs
- if the requests is coming from an OAuth client, it excludes the files and directories that are in a not synchronized folder
- it only supports the options `since`, `limit`, and `include_docs` of CouchDB
- it has an option `fields` to only include those fields in the results
- it has an option `include_file_path` to add the `path` for files
- it has two options `skip_deleted` and `skip_trashed` to not send to the client the deleted/trashed files and directories.

### Request

```http
GET /files/_changes?include_docs=true&fields=type,name,dir_id&skip_deleted=true&limit=3 HTTP/1.1
```

### Response

```http
HTTP/1.1 200 OK
Content-Type: application/json
```

```json
{
  "last_seq": "13-g1AAAABHeJzLYWBgYMxgTmHgzcvPy09JdcjLz8gvLskBCjMlMuSxMPwHgqwM5kTeXKAQe5Jpmrl5ojG68iwA2MMV1A",
  "pending": 24,
  "results": [
    {
      "id": "io.cozy.files.root-dir",
      "seq": "11-g1AAAABHeJzLYWBgYMxgTmHgzcvPy09JdcjLz8gvLskBCjMlMuSxMPwHgqwM5kTuXKAQe5Jpmrl5ojG68iwA2IcV0g",
      "doc": {
        "type": "directory"
      },
      "changes": [
        {
          "rev": "1-077f26acac6f0a5e1022f109e8d1dfc4"
        }
      ]
    },
    {
      "id": "io.cozy.files.trash-dir",
      "seq": "12-g1AAAABHeJzLYWBgYMxgTmHgzcvPy09JdcjLz8gvLskBCjMlMuSxMPwHgqwM5kSeXKAQe5Jpmrl5ojG68iwA2KUV0w",
      "doc": {
        "dir_id": "io.cozy.files.root-dir",
        "name": ".cozy_trash",
        "type": "directory"
      },
      "changes": [
        {
          "rev": "1-de9c11dbc386b5f91adbe2aee4a3e754"
        }
      ]
    },
    {
      "id": "d30b0dd6e0e8fefdac2a94cb2c00249f",
      "seq": "13-g1AAAABHeJzLYWBgYMxgTmHgzcvPy09JdcjLz8gvLskBCjMlMuSxMPwHgqwM5kTeXKAQe5Jpmrl5ojG68iwA2MMV1A",
      "doc": {
        "dir_id": "io.cozy.files.root-dir",
        "name": "Administratif",
        "type": "directory"
      },
      "changes": [
        {
          "rev": "1-24f2828e8dbe64135913072a4c92d846"
        }
      ]
    }
  ]
}
```

### POST `/files/_find`

Find allows to find documents using a mango selector. You can read more about mango selectors [here](http://docs.couchdb.org/en/stable/api/database/find.html#selector-syntax).

Note that it returns a
[bookmark](https://github.com/cozy/cozy-stack/blob/master/docs/mango.md#pagination-cookbook)
in the `links`, useful to paginate (following the [JSON-API
pagination](./http-api.md#pagination)). On the last page, there won't be a
`links.next`.

It is possible to pass a `execution_stats` parameter to get some information about the query execution. See [here](https://docs.couchdb.org/en/stable/api/database/find.html#execution-statistics) for more details.

### Request

```http
POST /files/_find HTTP/1.1
```


```json
{
    "selector": {
        "class": "image",
        "trashed": false
    },
    "limit": 2,
    "bookmark": "g1AAAABjeJzLYWBgYMpgSmHgKy5JLCrJTq2MT8lPzkzJBYorpFokG5qaGVqYmCWbGxuYGFkYWhgkmqaZJZsZGpibWhiA9HHA9OWATAJpY83MTUxPTWFgTUvMKU7NygIA7IYZzA",
    "use_index": "_design/a5f4711fc9448864a13c81dc71e660b524d7410c",
    "execution_stats": true
}
```

### Response

```http
HTTP/1.1 200 OK
Date: Mon, 27 Sept 2016 12:28:53 GMT
Content-Length: ...
Content-Type: application/json
```

```json
{
  "data": [
    {
      "type": "io.cozy.files",
      "id": "e8c1561846c730428180a5f6c6107914",
      "attributes": {
        "type": "file",
        "name": "nicepic1.jpg",
        "dir_id": "f49b4087cbf946dfc759214394009a6c",
        "created_at": "2020-02-13T16:35:47.568155477+01:00",
        "updated_at": "2020-02-13T16:35:47.568155477+01:00",
        "size": "345385",
        "md5sum": "12cGYwT+RiNjFxf4f7AmzQ==",
        "mime": "image/jpeg",
        "class": "image",
        "executable": false,
        "trashed": false,
        "tags": [],
        "path": "/Pictures/nicepic1.jpg",
        "metadata": {
          "datetime": "2020-02-13T16:35:47.568155477+01:00",
          "extractor_version": 2,
          "height": 1080,
          "width": 1920
        }
      },
      "meta": {
        "rev": "2-235e715b1d82a93285be1b0bd691b779"
      },
      "links": {
        "self": "/files/e8c1561846c730428180a5f6c6107914",
        "tiny": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/tiny",
        "small": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/small",
        "medium": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/medium",
        "large": "/files/e8c1561846c730428180a5f6c6107914/thumbnails/377327a8e20d6a50/large"
      },
      "relationships": {
        "parent": {
          "links": {
            "related": "/files/f49b4087cbf946dfc759214394009a6c"
          },
          "data": {
            "id": "f49b4087cbf946dfc759214394009a6c",
            "type": "io.cozy.files"
          }
        }
      }
    },
    {
      "type": "io.cozy.files",
      "id": "e8c1561846c730428180a5f6c6109007",
      "attributes": {
        "type": "file",
        "name": "nicepic2.jpg",
        "dir_id": "f49b4087cbf946dfc759214394009a6c",
        "created_at": "2020-02-13T16:35:47.845049743+01:00",
        "updated_at": "2020-02-13T16:35:47.845049743+01:00",
        "size": "323009",
        "md5sum": "Fla3ucNXuW2Xw/TK8pfsPA==",
        "mime": "image/jpeg",
        "class": "image",
        "executable": false,
        "trashed": false,
        "tags": [],
        "path": "/Pictures/nicepic2.jpg",
        "metadata": {
          "datetime": "2020-02-13T16:35:47.845049743+01:00",
          "extractor_version": 2,
          "height": 1080,
          "width": 1920
        }
      },
      "meta": {
        "rev": "2-4883d6b8ccad32f8fb056af9b7f8b37f"
      },
      "links": {
        "self": "/files/e8c1561846c730428180a5f6c6109007",
        "tiny": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/tiny",
        "small": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/small",
        "medium": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/medium",
        "large": "/files/e8c1561846c730428180a5f6c6109007/thumbnails/58a4aea31b00c99d/large"
      },
      "relationships": {
        "parent": {
          "links": {
            "related": "/files/f49b4087cbf946dfc759214394009a6c"
          },
          "data": {
            "id": "f49b4087cbf946dfc759214394009a6c",
            "type": "io.cozy.files"
          }
        }
      }
    }
  ],
  "links": {
    "next": "/files/_find?page[cursor]=g1AAAABjeJzLYWBgYMpgSmHgKy5JLCrJTq2MT8lPzkzJBYorpFokG5qaGVqYmCWbGxuYGFkYWhgkmqaZJZsZGlgaGJiD9HHA9OWATAJpY83MTUxPTWFgTUvMKU7NygIA694ZyA"
  },
  "meta": {
    "count": 2,
    "execution_stats": {
      "total_docs_examined": 11,
      "results_returned": 2,
      "execution_time_ms": 8.833
    }
  }
}
```


### DELETE /files/:dir-id

Put a directory and its subtree in the trash. It requires the permissions on
`io.cozy.files` for `PATCH`.

#### HTTP headers

It's possible to send the `If-Match` header, with the previous revision of the
file/directory (optional).

## Files

A file is a binary content with some metadata.

### POST /files/:dir-id

Upload a file in the directory identified by `:dir-id`.

The `created_at` field will be the first valid value in this list:

- the datetime extracted from the EXIF for a photo
- the `CreatedAt` parameter from the query-string
- the `Date` HTTP header
- the current time from the server.

The `updated_at` field will be the first value in this list:

- the datetime extracted from the EXIF for a photo if it is greater than the other values
- the `UpdatedAt` parameter from the query-string
- the `Date` HTTP header
- the current time from the server.

/!\ If the `updated_at` field is older than the `created_at` one,
then the `updated_at` will be set with the value of the `created_at`.

#### Query-String

| Parameter               | Description                                                    |
| ----------------------- | -------------------------------------------------------------- |
| Type                    | `file`                                                         |
| Name                    | the file name                                                  |
| Size                    | the file size (when `Content-Length` can't be used)            |
| Tags                    | an array of tags                                               |
| Executable              | `true` if the file is executable (UNIX permission)             |
| Encrypted               | `true` if the file is client-side encrypted                    |
| Metadata                | a JSON with metadata on this file (_not recommended_)          |
| MetadataID              | the identifier of a metadata object                            |
| CreatedAt               | the creation date of the file                                  |
| UpdatedAt               | the modification date of the file                              |
| SourceAccount           | the id of the source account used by a konnector               |
| SourceAccountIdentifier | the unique identifier of the account targeted by the connector |

#### HTTP headers

| Parameter      | Description                                 |
| -------------- | ------------------------------------------- |
| Content-Length | The file size                               |
| Content-MD5    | A Base64-encoded binary MD5 sum of the file |
| Content-Type   | The mime-type of the file                   |
| Date           | The modification date of the file           |

#### Request

```http
POST /files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81?Type=file&Name=hello.txt&CreatedAt=2016-09-18T01:23:45Z HTTP/1.1
Accept: application/vnd.api+json
Content-Length: 12
Content-MD5: hvsmnRkNLIX24EaM7KQqIA==
Content-Type: text/plain
Date: Mon, 19 Sep 2016 12:38:04 GMT
Host: cozy.example.com

Hello world!
```

#### Status codes

- 201 Created, when the file has been successfully created
- 404 Not Found, when the parent directory does not exist
- 409 Conflict, when a file with the same name already exists
- 412 Precondition Failed, when the md5sum is `Content-MD5` is not equal to
  the md5sum computed by the server
- 413 Payload Too Large, when there is not enough available space on the cozy
  to upload the file or the file is larger than the server's filesystem maximum
  file size
- 422 Unprocessable Entity, when the sent data is invalid (for example, the
  parent doesn't exist, `Type`, `Name`, or `MetadataID` parameter is missing or
  invalid, etc.)

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/vnd.api+json
Location: https://cozy.example.com/files/9152d568-7e7c-11e6-a377-37cbfb190b4b
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
    "meta": {
      "rev": "4-1482b88a"
    },
    "attributes": {
      "type": "file",
      "name": "sunset.jpg",
      "trashed": false,
      "md5sum": "ODZmYjI2OWQxOTBkMmM4NQo=",
      "created_at": "2016-09-18T20:38:04Z",
      "updated_at": "2016-09-21T12:38:04Z",
      "tags": [],
      "metadata": {
        "datetime": "2016-09-18T20:38:04Z",
        "height": 1080,
        "width": 1920
      },
      "size": 12,
      "executable": false,
      "class": "image",
      "mime": "image/jpg",
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2016-09-20T18:32:49Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2016-09-21T14:46:37Z",
        "uploadedAt": "2016-09-20T18:37:52Z",
        "uploadedOn": "https://cozy.example.com/",
        "uploadedBy": {
          "slug": "drive"
        }
      }
    },
    "relationships": {
      "parent": {
        "links": {
          "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        }
      },
      "referenced_by": {
        "links": {
          "self": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81/relationships/references"
        },
        "data": [
          {
            "type": "io.cozy.albums",
            "id": "94375086-e2e2-11e6-81b9-5bc0b9dd4aa4"
          }
        ]
      },
      "old_versions": {
        "data": [
          {
            "type": "io.cozy.files.versions",
            "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b/2-fa3a3bec"
          },
          {
            "type": "io.cozy.files.versions",
            "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b/1-0e6d5b72"
          }
        ]
      }
    },
    "links": {
      "self": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b",
      "tiny": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/0f9cda56674282ac/tiny",
      "small": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/0f9cda56674282ac/small",
      "medium": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/0f9cda56674282ac/medium",
      "large": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/0f9cda56674282ac/large"
    }
  },
  "included": [
    {
      "type": "io.cozy.files.versions",
      "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b/2-fa3a3bec",
      "meta": {
        "rev": "1-26a331"
      },
      "attributes": {
        "file_id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
        "updated_at": "2016-09-21T10:11:12Z",
        "md5sum": "a2lth5syMW+4r7jwNhdk3A==",
        "size": 123456,
        "tags": [],
        "cozyMetadata": {
          "doctypeVersion": "1",
          "metadataVersion": 1,
          "createdAt": "2016-09-20T18:37:52Z",
          "createdByApp": "drive",
          "createdOn": "https://cozy.example.com/",
          "updatedAt": "2016-09-20T18:37:52Z",
          "uploadedAt": "2016-09-20T18:37:52Z",
          "uploadedOn": "https://cozy.example.com/",
          "uploadedBy": {
            "slug": "drive"
          }
        }
      }
    },
    {
      "type": "io.cozy.files.versions",
      "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b/1-0e6d5b72",
      "meta": {
        "rev": "1-57b3e2"
      },
      "attributes": {
        "file_id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
        "updated_at": "2016-09-18T20:38:04Z",
        "md5sum": "FBA89XXOZKFhdv37iILb2Q==",
        "size": 159753,
        "tags": [],
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

**Note**: see [references of documents in VFS](references-docs-in-vfs.md) for
more informations about the references field.

### POST /files/upload/metadata

Send a metadata object that can be associated to a file (or directory) uploaded
after that, via the `MetadataID` query parameter. The `MetadataID` parameter is
only valid for 10 minutes, and will expire after that.

**Note:** a special permission on `io.cozy.certified.carbon_copy` is required
to send a request with `carbonCopy` as key in the `attributes` map. Same for
`electronicSafe`.

See the [metadata doctype](https://docs.cozy.io/en/cozy-doctypes/docs/io.cozy.files_metadata/) to know more about metadata attributes that can be set, notably for images and document qualification.

#### Request

```http
POST /files/upload/metadata HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files.metadata",
    "attributes": {
      "qualification": {
        "label": "telecom_invoice",
        "purpose": "invoice",
        "sourceCategory": "telecom"
      },
      "datetime": "2017-04-22T01:00:00-05:00"
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
    "type": "io.cozy.files.metadata",
    "id": "42E6BD48",
    "attributes": {
      "qualification": {
        "label": "telecom_invoice",
        "purpose": "invoice",
        "sourceCategory": "telecom"
      },
      "datetime": "2017-04-22T01:00:00-05:00"
    }
  }
}
```

### GET /files/download/:file-id

Download the file content.

By default the `content-disposition` will be `inline`, but it will be
`attachment` if the query string contains the parameter `Dl=1`

For a PDF file, it's possible to get only a single page by using the `Page`
parameter in the query-string (1 is the first page).

#### Request

```http
GET /files/download/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81 HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Length: 12
Content-Disposition: inline; filename="hello.txt"
Content-Type: text/plain

Hello world!
```

### GET /files/download

Download the file content from its path.

By default the `content-disposition` will be `inline`, but it will be
`attachment` if the query string contains the parameter `Dl=1`

For a PDF file, it's possible to get only a single page by using the `Page`
parameter in the query-string (1 is the first page).

#### Request

```http
GET /files/download?Path=/Documents/hello.txt&Dl=1 HTTP/1.1
```

### GET /editor/:file-id/open

Return the parameters to build the URL where a file can be opened by an editor
application. This route is generic for files stored as `io.cozy.files`; it does
not require a dedicated editor doctype.

The caller must have `GET` access to the file. Share-by-link, share-preview,
and share-interact tokens are also supported when the token gives access to the
file or to a shared directory that contains it.

If the file is part of a sharing, the response can point to another instance
where collaborative edition should happen. In that case, the JSON:API `id`
remains the file id known by the caller, while `file_id` is the id to use on the
instance returned in the response.

#### Query-String

| Parameter   | Description |
| ----------- | ----------- |
| SharingID   | Identifier of the Cozy-to-Cozy sharing, used when the stack forwards an open request |
| MemberIndex | Index of the sharing member, used when the stack forwards an open request |
| ReadOnly    | Set to `true` when the returned sharecode must only grant read access |

#### Request

```http
GET /editor/32e07d806f9b0139c541543d7eb8149c/open HTTP/1.1
Host: bob.cozy.example
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
    "id": "32e07d806f9b0139c541543d7eb8149c",
    "attributes": {
      "file_id": "b05e7c306f9c0139c542543d7eb8149c",
      "subdomain": "flat",
      "protocol": "https",
      "instance": "alice.cozy.example",
      "sharecode": "543d7eb8149c",
      "public_name": "Bob"
    }
  }
}
```

### GET /files/:file-id/thumbnails/:secret/:format

Get a thumbnail of a file (for an image & pdf only). `:format` can be `tiny` (96x96)
`small` (640x480), `medium` (1280x720), or `large` (1920x1080).

This API does not require authentication because the secret acts as a token.
This secret is valid for 10 minutes, after which the link will return an error.
To retrieve a new functional link, you must query the files API again to obtain
a new secret.

### PUT /files/:file-id

Overwrite a file

The `updated_at` field will be the first value in this list:

- the datetime extracted from the EXIF for a photo if it is greater than the other values
- the `UpdatedAt` parameter from the query-string
- the `Date` HTTP header
- the current time from the server.

/!\ If the `updated_at` field is older than the `created_at` one, then the
`updated_at` will be set with the value of the `created_at`.

#### Query-String

| Parameter               | Description                                                    |
| ----------------------- | -------------------------------------------------------------- |
| Size                    | the file size (when `Content-Length` can't be used)            |
| Tags                    | an array of tags                                               |
| Executable              | `true` if the file is executable (UNIX permission)             |
| Encrypted               | `true` if the file is client-side encrypted                    |
| MetadataID              | the identifier of a metadata object                            |
| UpdatedAt               | the modification date of the file                              |
| SourceAccount           | the id of the source account used by a konnector               |
| SourceAccountIdentifier | the unique identifier of the account targeted by the connector |

#### HTTP headers

The HTTP headers are the same than for uploading a file. There is one additional
header, `If-Match`, with the previous revision of the file (optional).

#### Request

```http
PUT /files/9152d568-7e7c-11e6-a377-37cbfb190b4b HTTP/1.1
Accept: application/vnd.api+json
Content-Length: 12
Content-MD5: hvsmnRkNLIX24EaM7KQqIA==
Content-Type: text/plain
If-Match: 1-0e6d5b72

HELLO WORLD!
```

#### Status codes

- 200 OK, when the file has been successfully overwritten
- 404 Not Found, when the file wasn't existing
- 412 Precondition Failed, when the `If-Match` header is set and doesn't match
  the last revision of the file
- 413 Payload Too Large, when there is not enough available space on the cozy
  to upload the file or the file is larger than the server's filesystem maximum
  file size
- 422 Unprocessable Entity, when the sent data is invalid (for example, the
  `MetadataID` parameter has expired)

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
    "meta": {
      "rev": "2-d903b54c"
    },
    "attributes": {
      "type": "file",
      "name": "hello.pdf",
      "trashed": false,
      "md5sum": "YjU5YmMzN2Q2NDQxZDk2Nwo=",
      "created_at": "2016-09-19T12:38:04Z",
      "updated_at": "2016-09-19T12:38:04Z",
      "tags": [],
      "size": 12,
      "executable": false,
      "class": "pdf",
      "mime": "application/pdf",
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2016-09-20T18:32:49Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2016-09-21T04:27:50Z",
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
          "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        }
      }
    },
    "links": {
      "self": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b",
      "icon": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/icon/543d7eb8",
      "preview": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/preview/77e117e0",
      "tiny": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/93fd0a5d96b22e9d/tiny",
      "small": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/93fd0a5d96b22e9d/small",
      "medium": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/93fd0a5d96b22e9d/medium",
      "large": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b/thumbnails/93fd0a5d96b22e9d/large"
    }
  }
}
```

### POST /files/:file-id/copy

Create a copy of a file in the same directory with a copy suffix in its name

#### Query-String

| Parameter  | Description                                        |
| ---------- | -------------------------------------------------- |
| Name       | the file copy name (optional)                      |
| DirID      | the destination directory id (optional)            |

#### Request

```http
POST /files/9152d568-7e7c-11e6-a377-37cbfb190b4b/copy HTTP/1.1
Accept: application/vnd.api+json
```

#### Status codes

- 201 Created, when the file has been successfully copied
- 404 Not Found, when the file does not exist
- 409 Conflict, when a file with the same name as the copy would have already exists
- 413 Payload Too Large, when there is not enough available space on the cozy
  to copy the file

#### Response

```http
HTTP/1.1 201 Created
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files",
    "id": "7382f28a-21d4-12d9-4438-3fd53e98a219",
    "meta": {
      "rev": "1-83a82e9"
    },
    "attributes": {
      "type": "file",
      "name": "hello (copy).pdf",
      "trashed": false,
      "md5sum": "YjU5YmMzN2Q2NDQxZDk2Nwo=",
      "created_at": "2022-10-18T18:33:24Z",
      "updated_at": "2022-10-18T18:33:24Z",
      "tags": [],
      "size": 12,
      "executable": false,
      "class": "pdf",
      "mime": "application/pdf",
      "cozyMetadata": {
        "doctypeVersion": "1",
        "metadataVersion": 1,
        "createdAt": "2022-10-18T18:33:24Z",
        "createdByApp": "drive",
        "createdOn": "https://cozy.example.com/",
        "updatedAt": "2022-10-18T18:33:24Z"
      }
    },
    "relationships": {
      "parent": {
        "links": {
          "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        }
      }
    },
    "links": {
      "self": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219",
      "icon": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219/icon/543d7eb8",
      "preview": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219/preview/77e117e0",
      "tiny": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219/thumbnails/93fd0a5d96b22e9d/tiny",
      "small": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219/thumbnails/93fd0a5d96b22e9d/small",
      "medium": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219/thumbnails/93fd0a5d96b22e9d/medium",
      "large": "/files/7382f28a-21d4-12d9-4438-3fd53e98a219/thumbnails/93fd0a5d96b22e9d/large"
    }
  }
}
```

### DELETE /files/:file-id

Put a file in the trash.

## Common

### GET /files/metadata

Same as `/files/:file-id` but to retrieve informations from a path.

#### Request

```http
GET /files/metadata?Path=/Documents/hello.txt HTTP/1.1
```

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
Location: https://cozy.example.com/files/9152d568-7e7c-11e6-a377-37cbfb190b4b
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
      "name": "hello.txt",
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
    },
    "relationships": {
      "parent": {
        "links": {
          "related": "/files/fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        },
        "data": {
          "type": "io.cozy.files",
          "id": "fce1a6c0-dfc5-11e5-8d1a-1f854d4aaf81"
        }
      }
    },
    "links": {
      "self": "/files/9152d568-7e7c-11e6-a377-37cbfb190b4b"
    }
  }
}
```

### PATCH /files/:file-id and PATCH /files/metadata

Both endpoints can be used to update the metadata of a file or directory, or to
rename/move it. The difference is the first one uses an id to identify the
file/directory to update, and the second one uses the path.

Some specific attributes of the patch can be used:

- `dir_id` attribute can be updated to move a file or directory
- `move_to_trash` boolean to specify that the file needs to be moved to the
  trash
- `permanent_delete` boolean to specify that the files needs to be deleted
  (after being trashed)
- `cozyMetadata.favorite` boolean attribute to put/remove a file from the
  favorites

#### HTTP headers

It's possible to send the `If-Match` header, with the previous revision of the
file/directory (optional).

#### Request

```http
PATCH /files/9152d568-7e7c-11e6-a377-37cbfb190b4b HTTP/1.1
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
      "tags": ["poem"],
      "cozyMetadata": {
        "favorite": true
      }
    }
  }
}
```

#### Status codes

- 200 OK, when the file or directory metadata has been successfully updated
- 400 Bad Request, when a the directory is asked to move to one of its
  sub-directories
- 404 Not Found, when the file/directory wasn't existing
- 412 Precondition Failed, when the `If-Match` header is set and doesn't match
  the last revision of the file/directory
- 422 Unprocessable Entity, when the sent data is invalid (for example, the
  parent doesn't exist)

#### Response

```http
HTTP/1.1 200 OK
Content-Type: application/vnd.api+json
Location: https://cozy.example.com/files/9152d568-7e7c-11e6-a377-37cbfb190b4b
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

### PATCH /files/

Endpoint to update the metadata of files and directories in batch. It can be
used, for instance, to move many files in a single request.

#### Request

```http
PATCH /files/ HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": [
    {
      "type": "io.cozy.files",
      "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
      "meta": {"rev": "1-0e6d5b72"},
      "attributes": {"dir_id": "f2f36fec-8018-11e6-abd8-8b3814d9a465"}
    },
    {
      "type": "io.cozy.files",
      "id": "9152d568-7e7c-11e6-a377-37cbfb190b4c",
      "meta": {"rev": "2-123123"},
      "attributes": {"move_to_trash": true}
    }
  ]
}
```

#### Status codes

The same status codes can be encountered as the `PATCH /files/:file-id` route.


### POST /files/archive

Create an archive. The body of the request lists the files and directories that
will be included in the archive. For directories, it includes all the files and
sub-directories in the archive.

It's possible to give a file by its id (in the `ids` array) or by its path (in
the `files` array).

For PDF files, it's possible to put in the archive a single page, with the
`pages` argument: it's an array of objects, with `id` the file identifier of
the PDF file, and `page` the page number (1 is the first page).

The generated archive is temporary and is not persisted.

#### Request

```http
POST /files/archive HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files.archives",
    "attributes": {
      "name": "project-X",
      "ids": ["a51aeeea-4f79-11e7-9dc4-83f67e9494ab"],
      "files": [
        "/Documents/bills",
        "/Documents/images/sunset.jpg",
        "/Documents/images/eiffel-tower.jpg"
      ],
      "pages": [
        { "id": "3780caf0-104f-013d-3619-18c04daba326", "page": 1 }
      ]
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
    "related": "/files/archive/4521DC87/project-X.zip"
  },
  "data": {
    "type": "io.cozy.files.archives",
    "id": "4521DC87",
    "attributes": {
      "href": "/files/archive/4521DC87/project-X.zip"
    }
  }
}
```

### GET /files/archive/:key/:name

Download a previously created archive. The name parameter is not used in the
stack but aims to allow setting a name even for browser / downloader that do not
support Content-Disposition filename.

**This route does not require Basic Authentification**

```http
GET /files/archive/4521DC87/project-X.zip HTTP/1.1
Accept: application/zip
Content-Length: 12345
Content-Disposition: attachment; filename="project-X.zip"
Content-Type: application/zip
```

### POST /files/downloads?Path=file_path

Create a file download. The Path query parameter specifies the file to download.
The response json API links contains a `related` link for downloading the file,
see below.

### POST /files/downloads?Id=file_id

Also create a file download. But it takes the id of the file and not its path.

### POST /files/downloads?VersionId=file_id/version_id

This is a third way to create a file download. But, this time, it is for
downloading an old version of a file.

These 3 routes also accept a `Filename=...` parameter in the query-string to
change the filename that will be used for the downloaded file.

### GET /files/downloads/:secret/:name

Allows to download a file with a secret created from the route above.

The name parameter is not used in the stack but aims to allow setting a name
even for browser / downloader that do not support Content-Disposition filename.

By default the `content-disposition` will be `inline`, but it will be
`attachment` if the query string contains the parameter `Dl=1`

**This route does not require Basic Authentification**

## Versions

The identifier of the `io.cozy.files.versions` is composed of the `file-id` and
another string called the `version-id`, separated by a `/`. So, when a route
makes reference to `/something/:file-id/:version-id`, you can use the identifier
of the version document (without having to prepend the file identifier).

### GET /files/download/:file-id/:version-id

Download an old version of the file content

By default the `content-disposition` will be `inline`, but it will be
`attachment` if the query string contains the parameter `Dl=1`

#### Request

```http
GET /files/download/9152d568-7e7c-11e6-a377-37cbfb190b4b/1-0e6d5b72 HTTP/1.1
```

### POST /files/:file-id/versions

Create a new version of a file, with the same content but new metadata. It
requires a permission for PUT on the file, as it is equivalent to upload the
same content of the file.

#### Query-String

| Parameter | Description        |
| --------- | ------------------ |
| Tags      | an array of tags   |

#### Request

```http
POST /files/9152d568-7e7c-11e6-a377-37cbfb190b4b/metadata HTTP/1.1
Content-Type: application/vnd.api+json
```

```json
{
  "data": {
    "type": "io.cozy.files.metadata",
    "attributes": {
      "qualification": {
        "label": "telecom_invoice",
        "purpose": "invoice",
        "sourceCategory": "telecom"
      },
      "datetime": "2017-04-22T01:00:00-05:00",
    }
  }
}
```

### POST /files/revert/:file-id/:version-id

This endpoint can be used to revert to an old version of the content for a
file.

#### Request

```http
POST /files/revert/9152d568-7e7c-11e6-a377-37cbfb190b4b/2-fa3a3bec HTTP/1.1
```

### PATCH /files/:file-id/:version-id

This endpoint can be used to edit the tags of a previous version of the file.

#### Request

```http
PATCH /files/9152d568-7e7c-11e6-a377-37cbfb190b4b/1-0e6d5b72 HTTP/1.1
Accept: application/vnd.api+json
Content-Type: application/vnd.api+json
```


```json
{
  "data": {
    "type": "io.cozy.files.versions",
    "id": "9152d568-7e7c-11e6-a377-37cbfb190b4b/1-0e6d5b72",
    "attributes": {
      "tags": ["poem"]
    }
  }
}
```

### DELETE /files/:file-id/:version-id

This endpoint can be used to delete an old version of the content for a file.

#### Request

```http
DELETE /files/9152d568-7e7c-11e6-a377-37cbfb190b4b/2-fa3a3bec HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 No Content
```

### DELETE /files/versions

Deletes all the old versions of all files to make space for new files.

#### Request

```http
DELETE /files/versions HTTP/1.1
```

#### Response

```http
HTTP/1.1 204 No Content
```


## Trash

When a file is deleted, it is first moved to the trash. In the trash, it can be
restored. Or, after some time, it will be removed from the trash and permanently
destroyed.

The file `trashed` attribute will be set to true.
When a file or folder is moved to the trash, `cozyMetadata` also records:

- `trashedAt`: the server timestamp of the trash action
- `trashedBy`: the request actor that triggered the trash action, with:
  - `kind`: `member` or `anonymous-share`
  - `displayName` and `domain` for `member`

### GET /files/trash

List the files inside the trash. It's paginated.

### Query-String

| Parameter    | Description                           |
| ------------ | ------------------------------------- |
| page[cursor] | the last id of the results            |
| page[limit]  | the number of entries (30 by default) |

#### Request

```http
GET /files/trash HTTP/1.1
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
      "type": "io.cozy.files",
      "id": "df24aac0-7f3d-11e6-81c0-d38812bfa0a8",
      "meta": {
        "rev": "1-3b75377c"
      },
      "attributes": {
        "type": "file",
        "dir_id": "io.cozy.files.trash-dir",
        "name": "foo.txt",
        "trashed": true,
        "restore_path": "/previous_parent",
        "md5sum": "YjAxMzQxZTc4MDNjODAwYwo=",
        "created_at": "2016-09-19T12:38:04Z",
        "updated_at": "2016-09-19T12:38:04Z",
        "tags": [],
        "size": 123,
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
          "trashedAt": "2016-09-20T18:32:49Z",
          "trashedBy": {
            "kind": "member",
            "displayName": "Alice",
            "domain": "cozy.example.com"
          },
          "uploadedAt": "2016-09-20T18:32:49Z",
          "uploadedOn": "https://cozy.example.com/",
          "uploadedBy": {
            "slug": "drive"
          }
        }
      },
      "links": {
        "self": "/files/trash/df24aac0-7f3d-11e6-81c0-d38812bfa0a8"
      }
    },
    {
      "type": "io.cozy.files",
      "id": "4a4fc582-7f3e-11e6-b9ca-278406b6ddd4",
      "meta": {
        "rev": "1-4a09030e"
      },
      "attributes": {
        "type": "file",
        "dir_id": "io.cozy.files.trash-dir",
        "name": "bar.txt",
        "trashed": true,
        "restore_path": "/other_parent",
        "md5sum": "YWVhYjg3ZWI0OWQzZjRlMAo=",
        "created_at": "2016-09-19T12:38:04Z",
        "updated_at": "2016-09-19T12:38:04Z",
        "tags": [],
        "size": 456,
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
          "trashedAt": "2016-09-20T18:32:49Z",
          "trashedBy": {
            "kind": "member",
            "displayName": "Alice",
            "domain": "cozy.example.com"
          },
          "uploadedAt": "2016-09-20T18:32:49Z",
          "uploadedOn": "https://cozy.example.com/",
          "uploadedBy": {
            "slug": "drive"
          }
        }
      },
      "links": {
        "self": "/files/trash/4a4fc582-7f3e-11e6-b9ca-278406b6ddd4"
      }
    }
  ]
}
```

### POST /files/trash/:file-id

Restore the file with the `file-id` identifiant into its `restore_path`. If a file already exists at the restore path with the same name, a suffix will be added to avoid any conflict.

The file's `trashed` attributes will be set to false.

### DELETE /files/trash/:file-id

Destroy the file and make it unrecoverable (it will still be available in
backups).

### DELETE /files/trash

Clear out the trash.

## Trashed attribute

All files that are inside the trash will have a `trashed: true` attribute. This
attribute can be used in mango queries to only get "interesting" files.

## Real-time via websockets

In addition to the normal events for files, the stack also injects some events
when a thumbnail is generated. A permission on `io.cozy.files` is required to
subscribe to those events on `io.cozy.files.thumbnails`.

### Example

```
client > {"method": "AUTH",
          "payload": "xxAppOrAuthTokenxx="}
client > {"method": "SUBSCRIBE",
          "payload": {"type": "io.cozy.files.thumbnails"}}
server > {"event": "CREATED",
          "payload": {"id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
                      "type": "io.cozy.files.thumbnails",
                      "doc": {"format": "large"}}}
server > {"event": "CREATED",
          "payload": {"id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
                      "type": "io.cozy.files.thumbnails",
                      "doc": {"format": "medium"}}}
server > {"event": "CREATED",
          "payload": {"id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
                      "type": "io.cozy.files.thumbnails",
                      "doc": {"format": "small"}}}
server > {"event": "CREATED",
          "payload": {"id": "9152d568-7e7c-11e6-a377-37cbfb190b4b",
                      "type": "io.cozy.files.thumbnails",
                      "doc": {"format": "tiny"}}}
```
