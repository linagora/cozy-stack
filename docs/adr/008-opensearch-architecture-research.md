# Companion Research: OpenSearch Data Model, Permission Bitmaps & High-Fanout Defenses

## Overview

This document details the OpenSearch index architecture for `cozy-stack` server-side file search (companion to [ADR 008](./008-adr-server-side-file-search-v2.md)), addressing core engineering questions:

1. **How do we check permissions in OpenSearch if we do not store `path` or `shared_with` on file documents?**
2. **What happens when a folder containing 30,000+ files is moved into or out of a shared folder?**
3. **How do we handle corner cases like `node_modules` or 50,000+ subdirectories without blowing up `dir_id` filters?**
4. **What is the estimated index size and recommended sharding strategy for metadata-only search?**
5. **How does index size and resource footprint scale if full-text file content indexing is introduced?**

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

---

## 5. Capacity Estimation & Sharding Strategy (Metadata-Only)

### 5.1 Per-Document Footprint Breakdown

In the v1 metadata model, file contents are not indexed. Sizes below were measured on OpenSearch 2.17 (Lucene 9.11) with the mapping above: 1,000,000 synthetic documents (~480 B of JSON each, 30-char average filenames), one shard force-merged to one segment. N-gram terms repeat across filenames, so they cost little; `search_as_you_type` is the largest filename structure:

| Component | Description | Measured On-Disk Size |
| :--- | :--- | :--- |
| IDs, flags & `name.exact` | `_id`, `resource_key`, system IDs, `type`, `mime`, `class`, `updated_at`, `trashed`, plus LZ4-compressed `_source` | ~390 B |
| `name` (n-gram 3–4) | 30-char filename yields up to ~55 n-gram tokens | ~70 B |
| `name.prefix` | `search_as_you_type` (word shingles + edge n-gram prefixes) | ~350 B |
| **Total per document** | **Average on-disk footprint in Lucene** | **~0.8 KB** |

### 5.2 Scope Index Footprint (`cozy_share_scopes_v1`)

- Stores 1 document per active sharing root (`io.cozy.sharings`), containing authorized member principal IDs and descendant directory keys (`subtree_dir_keys`).
- **Average size**: ~10 KB – 50 KB per document (depending on folder depth and member count; each `subtree_dir_keys` entry is ~65 B, so this assumes ~150–750 subfolders per share). Not measured.
- **Scale**: An organization with 1,000,000 files typically maintains 5,000 – 20,000 active share roots.
- **Total storage**: **~0.5 GB – 2.5 GB** for 50,000 active shares.

### 5.3 Sizing Projections by Workload Tier (Metadata-Only)

| Workload Tier | File & Folder Count | Primary Storage (`cozy_files_v1`) | Storage with 1 Replica (HA) | Active Sharings | Scope Index Size (`cozy_share_scopes_v1`) |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Small / Pilot** | 100,000 | ~80 MB | ~160 MB | ~1,000 | ~10–50 MB |
| **Medium Org** | 1,000,000 | ~0.8 GB | ~1.6 GB | ~10,000 | ~0.1–0.5 GB |
| **Large Enterprise** | 10,000,000 | ~8 GB | ~16 GB | ~50,000 | ~0.5–2.5 GB |
| **Multi-Tenant SaaS** | 50,000,000 | ~40 GB | ~80 GB | ~200,000 | ~2–10 GB |

These are index sizes after a force merge; the deleted documents that live indices hold until segments merge are not measured. As a provisional disk buffer (not validated), provision about 1.3× the replicated index size: keep disk use under OpenSearch's default 85% low watermark (`cluster.routing.allocation.disk.watermark.low`; above it no new shards are allocated to the node, and at the 95% flood stage indices become read-only), with ~10% extra for merges and deleted documents (1.1 / 0.85 ≈ 1.3).

### 5.4 Recommended Sharding Strategy

OpenSearch best practice targets **10 GB – 30 GB per shard** for latency-sensitive search workloads. Over-sharding small indices introduces CPU thread contention, scatter-gather network hops, and distorted relevance scoring (BM25 term frequency skew).

#### `cozy_files_v1` Sharding Rules
- **Dev / Staging / Small (< 1M files, < 0.8 GB)**:
  - `number_of_shards`: **1**
  - `number_of_replicas`: **0** (dev) or **1** (staging)
- **Production Medium (1M – 10M files, 0.8 GB – 8 GB)**:
  - `number_of_shards`: **1** (~8 GB at 10M files, still below the 10 GB floor; the replica spreads reads across 2 nodes)
  - `number_of_replicas`: **1**
- **Production Large / Multi-Tenant (10M – 50M files, 8 GB – 40 GB)**:
  - `number_of_shards`: **2** (yielding ~20 GB per primary shard at 50M files)
  - `number_of_replicas`: **1** (or 2 for multi-AZ clusters)
- **Routing Strategy**: Use default hash-routing on `_id` (`resource_key`).
  > [!IMPORTANT]
  > Do **not** route by `owner_instance_id`. Users frequently search their personal files alongside files shared across different instances in a single request. Custom routing on `owner_instance_id` would force cross-shard scatter anyway or miss shared items.

#### `cozy_share_scopes_v1` Sharding Rules
- `number_of_shards`: **1** (Always 1 primary shard, regardless of cluster size).
- `index.auto_expand_replicas`: `"0-all"`:
  ```json
  "settings": {
    "index.number_of_shards": 1,
    "index.auto_expand_replicas": "0-all"
  }
  ```
- **Rationale**: This index serves OpenSearch's **Native `terms` lookup**. Pinning an in-memory replica to every data node ensures that terms lookups resolve as local key-value reads without crossing the network between nodes, maximizing Lucene Roaring Bitmap cache efficiency.

---

## 6. Capacity & Architectural Impact of Full-Text Content Indexing

If server-side search is extended to index file contents (full-text search inside documents, spreadsheets, presentations, and PDFs), the storage footprint and hardware requirements shift significantly.

### 6.1 Content Mapping & Indexing Design

Indexing file content requires adding a `content` field to `cozy_files_v1`:

```json
"properties": {
  "content": {
    "type": "text",
    "analyzer": "standard",
    "term_vector": "with_positions_offsets"
  }
}
```

> [!WARNING]
> **Never apply n-gram tokenizers to body content.**
> N-grams on body text cause catastrophic index bloat (10× to 30× expansion). Filenames use 3–4 n-grams for substring matching, whereas body content must use standard word tokenizers, lowercase, and language-specific stemmers.
> Setting `term_vector: "with_positions_offsets"` enables OpenSearch's **Unified Highlighter** to extract search snippets directly from the inverted index without re-analyzing large stored texts at search time.

### 6.2 Workload Model & Per-File Sizing

In an enterprise collaborative drive, document types are mixed:

- **Extractable Text Documents (~40% of files)**: PDFs, DOCX, XLSX, PPTX, TXT, Markdown, HTML, CSV.
  - Average extracted text length: ~3,000 words (~18–20 KB of raw UTF-8 text).
  - Truncation ceiling: Recommended extraction cap of **1 MB** per file to prevent single giant log files or data dumps from exhausting memory.
  - Measured on OpenSearch 2.17 with the mapping in 6.1 (~1,240 documents of 18.9 KB English text, force-merged).
  - Inverted index (~5 KB) & term vectors (~10 KB): ~15 KB.
  - Stored `_source` (LZ4-compressed body text): ~9 KB.
  - Total on-disk footprint per text document, with metadata: **~25 KB**.
- **Non-Text / Metadata-Only Files (~60% of files)**: Images, audio, videos, CAD, archives, executables, binaries.
  - Content field is empty (`null`).
  - Total on-disk footprint: **~0.8 KB** (metadata only).
- **Blended Drive Average**:
  $$\text{Blended Avg} = (0.40 \times 25\text{ KB}) + (0.60 \times 0.8\text{ KB}) \approx \mathbf{10.5\text{ KB per file}}$$
- **Pure Document Repository (Worst-Case)**:
  If a tenant stores exclusively text documents (legal, medical, or administrative archives), size scales at **~25 KB per document**.

### 6.3 Sizing Comparison: Metadata-Only vs. Full-Text Content

| Workload Tier | Total Files | Metadata-Only Primary | Metadata-Only (1 Replica) | Content-Indexed Primary (Blended) | Content-Indexed (1 Replica) | Pure Text Repository (1 Replica) |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **Small / Pilot** | 100,000 | ~80 MB | ~160 MB | ~1.0 GB | ~2.0 GB | ~5.0 GB |
| **Medium Org** | 1,000,000 | ~0.8 GB | ~1.6 GB | ~10.5 GB | ~21.0 GB | ~50 GB |
| **Large Enterprise** | 10,000,000 | ~8 GB | ~16 GB | ~105 GB | ~210 GB | ~500 GB |
| **Multi-Tenant SaaS** | 50,000,000 | ~40 GB | ~80 GB | ~525 GB | ~1.05 TB | ~2.5 TB |

### 6.4 Sharding Adjustments for Full-Text Content

With content indexing, shard counts must increase to maintain the **10 GB – 30 GB per shard** operational window:

| Total Files | Estimated Primary Size (Blended) | Recommended Primary Shards | Shard Size Range |
| :--- | :--- | :--- | :--- |
| **1,000,000** | ~10.5 GB | **1 primary shard** | ~10.5 GB |
| **10,000,000** | ~105 GB | **4 to 5 primary shards** | ~21 GB – 26 GB |
| **50,000,000** | ~525 GB | **18 to 22 primary shards** | ~24 GB – 29 GB |

### 6.5 Pipeline & Operational Considerations

1. **Extraction Worker Decoupling**:
   - Content extraction (via Apache Tika, `pdftotext`, or sidecar extractors) is CPU- and memory-intensive.
   - Text extraction must run asynchronously in background job workers (`io.cozy.jobs`) using RabbitMQ tasks, completely decoupled from the synchronous file upload HTTP request.
2. **`_source` Exclusion vs. Snippet Highlighting**:
   - Highlighting needs the original text, so keep `content` in `_source`. Excluding it (`"excludes": ["content"]`) makes snippets come back empty (tested on OpenSearch 2.17), and `_update` / reindex would drop the content. It also saves no disk at first: OpenSearch keeps the full source as `_recovery_source` (still ~25 KB per text document in our test) until soft-delete retention leases let merges prune it; only then would the ~9 KB saving appear.
   - To shrink the index, store offsets in the postings (`"index_options": "offsets"`) instead of `term_vector: "with_positions_offsets"`: ~19 KB instead of ~25 KB per text document (~23% less), and the Unified Highlighter still returns snippets.
3. **JVM Heap Sizing**:
   - With 10M+ content-indexed documents, increase OpenSearch node heap allocations. Segment merging on large text fields requires healthy index buffer memory (`indices.memory.index_buffer_size: 15%`).
   - Sizing rule of thumb: Allocate **50% of node RAM to JVM heap** (capped at 31 GB for compressed OOPs), ensuring the remaining 50% is free for OS filesystem cache (page cache) to hold Lucene segments in RAM.
