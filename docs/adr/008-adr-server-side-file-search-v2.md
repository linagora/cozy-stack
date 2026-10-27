# ADR 008: Multi-Tenant Server-Side File Search with Normalized Directory Scopes

## Status

Proposed (Counter-Proposal)

Related to [Personal Recents (ADR 007 / PR #4949)](./007-adr-server-side-recents.md) and the companion technical blueprint [OpenSearch Data Model & High-Fanout Directory Research](./008-opensearch-architecture-research.md).

## Context

Today, `cozy-dataproxy-lib` builds browser search indexes by downloading file metadata through `_all_docs` and per-share PouchDB replications. Moving search to the backend must eliminate two major scalability bottlenecks:

1. **Per-Recipient Data Duplication (`O(F × U)`)**: Copying a shared folder of `F` files into `U` recipient databases (e.g., 100,000 files × 1,000 employees = 100M records) causes unacceptable storage bloat and write amplification.
2. **Subtree Batch Write Amplification (`O(M)` File Rewrites on Folder Move/Rename)**:
   - In `cozy-stack`'s CouchDB VFS (`model/vfs`), `DirDoc` stores `Fullpath`, whereas `FileDoc` intentionally stores **only `dir_id`** (its immediate parent directory ID) and omits `path`. This VFS invariant ensures that renaming or moving a directory only updates child `DirDoc` records in CouchDB, leaving all child `FileDoc` records and revisions untouched.
   - If the search index stores materialized visible paths or recipient ACL lists on every file document, renaming a folder or moving a subfolder into/out of a shared root forces background workers to recursively scan CouchDB and re-index all `M` child files.
3. **Query-Time `sharing.AccessResolver` Bottlenecks**:
   - In `model/sharing/effective_access.go`, `AccessResolver.applicableSharingsForDoc` resolves `file.Path(fs)`, queries CouchDB Mango (`dir-by-path`) for ancestor `referenced_by` links, and loads `io.cozy.sharings`. Executing this per candidate search hit creates an `N+1` CouchDB query bottleneck.

## Decision

### 1. Same-Stack Multi-Tenant Scope First (Aligned with ADR 007 / PR #4949)

Scope v1 to instances hosted on the same `cozy-stack` deployment (Twake Workplace / hosted multi-tenant cluster). All instances on the same stack share a logical multi-tenant OpenSearch cluster. Cross-server federated fan-out (`D` external backends) is deferred to a separate ADR.

### 2. Normalized File Index (`cozy_files_v1`) Without Materialized Paths or ACL Lists

Index each file and directory **once** in OpenSearch (`cozy_files_v1`), keyed by `resource_key` (`<owner_instance_id>:<doc_id>`).

- File records store **only** local/immediate attributes: `resource_key`, `owner_instance_id`, `doc_id`, `dir_id`, `scoped_dir_key` (`<owner_instance_id>:<dir_id>`), `type`, `name`, `mime`, `class`, `updated_at`, and `trashed`.
- **Never store `path` (`Fullpath`) or recipient member lists on file documents in OpenSearch.**

### 3. Two-Stage Permission Checking via `cozy_share_scopes_v1` & Roaring Bitmaps

Maintain a second lightweight OpenSearch index (`cozy_share_scopes_v1`, or Redis cache) containing **1 document per active sharing** (`_id = sharing_id`):

- Stores `active_member_principals` (authorized instance IDs and group IDs) and `subtree_dir_keys` (the list of `<owner_instance_id>:<dir_id>` directory identifiers nested under that share's `RootID`).
- At search time (`POST /files/search`), `cozy-stack`:
  1. Resolves the caller's active `sharing_id`s from `cozy_share_scopes_v1` where `active_member_principals` matches the caller.
  2. Queries `cozy_files_v1` using OpenSearch's **Native `terms` Lookup** (backed by Lucene Roaring Bitmaps) so only files owned by the caller or residing in an authorized `scoped_dir_key` are matched.
  3. Reconstructs the relative visible path for the returned page (e.g., 20 hits) from the cached Directory Graph, stopping at the share's `RootID` so private parent directory names are never matched or leaked.

### 4. High-Fanout Directory Defense (`node_modules`, `.git`, and 50k+ Subfolder Trees)

To prevent pathological directory trees (e.g., `node_modules` with 40,000 subdirectories) from exceeding OpenSearch `terms` limits or polluting search relevance:

1. **Shallow-Index Vendor/VCS Artifacts**: Index the root directory entry (`node_modules`, `.git`, `.venv`, `__pycache__`, `vendor`) itself, but skip recursive indexing of its transitive child directories and files.
2. **Depth/Fanout Anchor Coalescing (`scope_anchor_id`)**: For legitimate deep directory trees exceeding depth 3 or 500 subdirectories, child directories inherit their ancestor's `scope_anchor_id` so `subtree_dir_keys` remains strictly bounded well below OpenSearch's 65,536 `terms` limit.

## Consequences

- **Zero Per-User Duplication**: Storage scales with `O(F)` source files, not `O(F × U)`.
- **Zero File Rewrites on Folder Rename or Move Across Shares**: Moving a 30,000-file folder into or out of a shared root updates **1 scope document** in `cozy_share_scopes_v1` and **0 file documents** in `cozy_files_v1`.
- **Sub-15ms Authorized Search**: Eliminates per-hit `AccessResolver` CouchDB roundtrips at search time.
