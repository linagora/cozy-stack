# Companion Research: OpenSearch Data Model, Permission Bitmaps & High-Fanout Defenses

## Overview

This document details the OpenSearch index architecture for `cozy-stack` server-side file search (companion to [ADR 008](./008-adr-server-side-file-search-v2.md)), addressing three core engineering questions:

1. **How do we check permissions in OpenSearch if we do not store `path` or `shared_with` on file documents?**
2. **What happens when a folder containing 30,000+ files is moved into or out of a shared folder?**
3. **How do we handle corner cases like `node_modules` or 50,000+ subdirectories without blowing up `dir_id` filters?**

---

## 1. Two-Index OpenSearch Data Model

### Index 1: `cozy_files_v1` (Normalized File & Folder Metadata)

Every file and folder is indexed once. File records store their immediate parent `dir_id` (`scoped_dir_key`), **never** their full path or sharing recipients:

```json
PUT /cozy_files_v1
{
  "settings": {
    "analysis": {
      "normalizer": {
        "LowercaseAscii": {
          "type": "custom",
          "filter": ["lowercase", "asciifolding"]
        }
      },
      "tokenizer": {
        "filename_ngram": {
          "type": "ngram",
          "min_gram": 3,
          "max_gram": 4,
          "token_chars": ["letter", "digit"]
        }
      },
      "analyzer": {
        "filename_index": {
          "type": "custom",
          "tokenizer": "filename_ngram",
          "filter": ["lowercase", "asciifolding"]
        },
        "filename_search": {
          "type": "custom",
          "tokenizer": "standard",
          "filter": ["lowercase", "asciifolding"]
        }
      }
    }
  },
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "resource_key":      { "type": "keyword" },
      "owner_instance_id": { "type": "keyword" },
      "doc_id":            { "type": "keyword" },
      "dir_id":            { "type": "keyword" },
      "scoped_dir_key":    { "type": "keyword" },
      "type":              { "type": "keyword" },
      "name": {
        "type": "text",
        "analyzer": "filename_index",
        "search_analyzer": "filename_search",
        "fields": {
          "prefix": { "type": "search_as_you_type" },
          "exact":  { "type": "keyword", "normalizer": "LowercaseAscii" }
        }
      },
      "mime":       { "type": "keyword" },
      "class":      { "type": "keyword" },
      "updated_at": { "type": "date" },
      "trashed":    { "type": "boolean" }
    }
  }
}
```

### Index 2: `cozy_share_scopes_v1` (1 Document per Active Share Scope)

Each active `io.cozy.sharings` document is represented by **1 scope document** storing its authorized members and the list of descendant directory keys (`subtree_dir_keys`) under its `RootID`:

```json
PUT /cozy_share_scopes_v1
{
  "mappings": {
    "dynamic": "strict",
    "properties": {
      "sharing_id":               { "type": "keyword" },
      "owner_instance_id":        { "type": "keyword" },
      "root_dir_id":              { "type": "keyword" },
      "active_member_principals": { "type": "keyword" },
      "subtree_dir_keys":         { "type": "keyword" },
      "direct_file_keys":         { "type": "keyword" },
      "updated_at":               { "type": "date" }
    }
  }
}
```

---

## 2. Query-Time Permission Enforcement via Native `terms` Lookup

When user `inst_alice` executes `POST /files/search`:

1. **Resolve Active Share IDs (~1 ms)**: Query `cozy_share_scopes_v1` for documents where `active_member_principals` contains `inst_alice` (or her group IDs). Suppose this returns `["sharing_hr_drive_01"]`.
2. **Execute Single OpenSearch Query on `cozy_files_v1` (~5–10 ms)**:
   Using OpenSearch's native `terms` lookup, OpenSearch reads `subtree_dir_keys` directly from `cozy_share_scopes_v1` inside the data node and caches the resulting **Lucene Roaring Bitmap**:

```json
POST /cozy_files_v1/_search
{
  "size": 20,
  "query": {
    "bool": {
      "must": [
        {
          "multi_match": {
            "query": "salaries",
            "fields": ["name^3", "name.prefix^2", "name.exact^4"],
            "type": "best_fields",
            "operator": "and"
          }
        }
      ],
      "filter": [
        { "term": { "trashed": false } },
        {
          "bool": {
            "should": [
              { "term": { "owner_instance_id": "inst_alice" } },
              {
                "terms": {
                  "scoped_dir_key": {
                    "index": "cozy_share_scopes_v1",
                    "id": "sharing_hr_drive_01",
                    "path": "subtree_dir_keys"
                  }
                }
              }
            ],
            "minimum_should_match": 1
          }
        }
      ]
    }
  },
  "sort": [
    { "_score": "desc" },
    { "resource_key": "asc" }
  ]
}
```

---

## 3. Moving a Folder Into or Out of a Shared Folder

Suppose `/Payroll-2026` (containing 30,000 files) is moved **out of** shared folder `/HR` and **into** private folder `/Board`:

- **File Index (`cozy_files_v1`)**: **0 writes**. Every file inside `/Payroll-2026` still has `"dir_id": "dir_payroll_2026"` and `"scoped_dir_key": "inst_corp_01:dir_payroll_2026"`.
- **Scope Index (`cozy_share_scopes_v1`)**: **1 document update**. `"inst_corp_01:dir_payroll_2026"` (and any child directories of `/Payroll-2026`) is removed from `sharing_hr_drive_01.subtree_dir_keys`.
- **Immediate Effect**: On the very next search, `"inst_corp_01:dir_payroll_2026"` is no longer in the `terms` bitmap for `sharing_hr_drive_01`. All 30,000 files become invisible to recipients immediately without touching a single file document.

---

## 4. Handling `node_modules`, `.git`, and 50,000+ Subdirectory Corner Cases

A single `node_modules` directory can contain 40,000 subdirectories and 200,000 files, violating the normal 1:50 folder-to-file ratio. We guard against this with three layers:

1. **Layer 1 — Shallow-Index Vendor & VCS Trees**:
   - Index the root folder document (`node_modules`, `.git`, `.venv`, `__pycache__`, `vendor`, `.next`) so users can locate the folder itself, but skip indexing its transitive child directories and files.
2. **Layer 2 — Bounded-Depth `scope_anchor_id` Coalescing**:
   - For legitimate business hierarchies with > 1,000 folders, directories deeper than depth 3 (or inside a parent exceeding 500 subfolders) inherit their ancestor's `scope_anchor_id` rather than adding thousands of leaf `dir_id`s to `subtree_dir_keys`.
3. **Layer 3 — Native OpenSearch `terms` Lookup**:
   - Avoids serializing thousands of directory IDs over HTTP between `cozy-stack` and OpenSearch and leverages Lucene's segment-level Roaring Bitmap cache.
