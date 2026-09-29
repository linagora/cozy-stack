# ADR: Server-side search without copying shared trees to every user

## Status

Draft

Related to [personal recents](https://github.com/linagora/cozy-stack/pull/4949). We still need to
choose the search engine, prove that permission filtering works at scale, and
define how servers query each other before accepting this ADR.

## Context

Today, `cozy-dataproxy-lib` builds browser search indexes for local files and
each share. Drive also has an older `useSearch` / `indexFiles` path that
downloads local metadata through `_all_docs`. Both need file metadata on the
device before they can search it.

Moving those copies to each user's stack would remove browser work, but still
copy a large company folder into every employee's search database. Batching
requests helps with request count; it does not remove the copies or updates.

Recents only needs my edits and items shared directly with me. Search must also
find old, unchanged files and files I can access through a group or parent folder.
The two features need the same IDs and permission rules, but different storage.

## Decision

Index each file's metadata once on the backend that owns it. Hosted instances
use a shared search service that checks which instance each record belongs to
and who may search it. Sharing a folder adds an access rule; it does not copy
the folder's index for each recipient.

Use stable source-instance ID plus file ID as the record key. Database replicas
and index partitions may still be needed for capacity and availability, but their
number does not depend on the number of recipients. This shared search service
is new backend work.

Add a search endpoint on the user's stack. It queries the hosting backend and,
when needed, other compatible servers that own shared files. Each remote server
searches its own files and returns a limited page of authorized results.

Do not require a per-user `io.cozy.files_search` or `io.cozy.files_index`
database. The search engine needs an index, and cozy-client may need a query
type, but neither requires copying all accessible metadata into each user's
CouchDB. Contact/app search and searching file contents are separate features.

### How much data and how many requests?

Let `F` be the number of files/folders, `U` the recipients, `B` the average
metadata size, `P` the batch size and `C` the number of changed items.
For full batches:

| Work | Copy the tree to every user | Index at the source |
|---|---|---|
| Initial metadata | `F * U * B` | About `F * B`, plus permission records and index overhead |
| Initial requests to copy trees | `U * ceil(F / P)` | No tree copy for each recipient |
| Updates after `C` items change | Up to `C * U` recipient updates | About `C` index updates, plus infrastructure replicas |
| A search request | Read the user's copy | Query source indexes and return limited result pages |

For example, a folder with 100,000 items shared with 1,000 users, at 1 kB per
item and 1,000 items per batch, would produce:

- Per-user copies: 100 million records, about 100 GB of metadata and 100,000
  initial batch requests.
- One source index: about 100 MB of metadata, before index overhead and replicas.
- Changing 1,000 items: about 1 GB of metadata copying in the per-user design.

These are examples, not measurements. Real sizes include permission records,
text indexes, request overhead, retries and replicas. Compression and whether
servers exchange data over the network also affect traffic.

Source indexing still has costs. More users mean more searches. Renaming or
moving a folder may require updating paths for all its children.

### IDs and permissions

Store file metadata separately from sharing and group membership. Keep access
rules on shared files/folder roots and resolve access through their parents.
Do not store every employee on every file record. The prototype must show that
these permission checks remain fast for large trees and groups.

The rules are:

- A user can have access through a parent, child and direct file share at once.
  Return the file once. Removing one share must preserve other valid access.
- Equal file IDs on different instances are different resources. Define stable
  instance IDs and how they survive domain changes. Keep search record IDs and
  revisions separate from source file IDs and revisions.
- Sharing an already indexed folder makes its children searchable through that
  share. Acceptance, group changes and revocation need reliable permission
  updates, without copying the tree for each user.
- Check the calling app's OAuth permissions as well as the user's access.
  Keep the engine private. Clients must not choose arbitrary source URLs,
  bypass instance filters or send unrestricted engine queries.
- Index only approved metadata fields and internal share references. Do not
  put sharing credentials, file contents or full member lists in every record.

Visible paths need special care. If only a child folder is shared with me,
search must neither display nor match its private parent names. Removing the
private path from the response after matching it is too late.

Check current owner permissions before returning results. Old permissions must
not leak file names through hits, highlights, suggestions or counts. Block
revoked access before background index cleanup. Check current metadata and path
too: moving a parent may leave a child's own file revision unchanged.

Reuse `sharing.AccessResolver` rules. The index itself does not grant access.
Hosted and remote results both need owner validation or an equally strict,
synchronized permission check. Until another approach is agreed, omit items
whose access cannot be checked and report incomplete results. Cached permissions
would need an agreed maximum delay for revocation. Opening or changing a result
still goes through the existing file APIs.

### Keeping the index up to date

Reuse existing jobs, locks and CouchDB changes feeds. File/sharing changes wake
workers; saved progress and periodic catch-up recover missed notifications.
Process files once at their source, not once for each recipient.

Workers must:

- Track file and sharing progress separately and read their current state.
  The two databases do not share one transaction or event order.
- Make retries safe, check each bulk-write result and save progress only after
  successful writes. Keep failed work for repair and reject outdated workers.
- Handle trash, deletion, restoration, accepted shares and group changes.
- Update child paths and permissions after folder moves/renames, even if child
  revisions have not changed. Large updates must be resumable.

Build each source index once, then catch up changes that happened during the
build. Define where the initial scan ends and catch-up begins so no changes are
missed. Sharing an indexed tree changes permissions; it does not trigger another
full copy. Track whether an index is ready or incomplete. Repair jobs must
remove stale entries as well as add missing ones. Full rebuilds can be rare.

When the record format or text-processing rules change, build a replacement
index, catch it up and switch to it. Invalidate cursors tied to the old version.
The index must be rebuildable from source data. Index failures must not undo
successful file operations, and indexing must not modify source documents.

### Search API

The proposed endpoint is `POST /files/search`, with search text, supported
filters, a maximum page size and an opaque cursor for the next page.

The first version must:

- Search file/folder names and visible paths, including old child items and
  directly shared files. Exclude trash and internal entries.
- Keep case/accent-insensitive and word-order-independent matching. Agree on
  examples for prefixes, matches inside words and ranking from both current
  implementations before switching. Do not replace matches inside words with
  prefix-only search. Cover punctuation, extensions, accented letters,
  non-Latin scripts and numeric names.
- Limit input size, filters, page size and execution time; support cancellation.
  Return highlight positions, not untrusted HTML.
- Return one ranked page with resource key as a stable tie-breaker. Bind cursors
  to the user, query, filters, ranking rules and index version. Define what
  happens when files change between pages; do not promise a frozen snapshot.
- Report unavailable sources or work limits as incomplete results. A local-only
  result must not look like a complete search. Leave out global totals and
  grouped counts in the first version.

### Querying other servers

Send one query for all accepted shares handled by the same backend. Authenticate
through existing sharing relationships and validate every requested share.
This is a new search capability. The current per-share `_changes` endpoint
does not provide it.

Remote servers must check permissions and remove duplicates before applying
page limits. The user's stack merges their results and requests more pages when
needed, within a fixed work limit. Keep each source cursor to load later pages.

Scores from different search indexes may not be comparable. Agree on shared
ranking rules or a way to normalize scores before promising exact global order.
Fetching a few extra hits and reranking them gives only approximate ordering
unless we can prove otherwise.

Hosted instances use one logical query to the common index. With `D` independent
external backends, a search can still need up to `D` remote requests, plus later
pages and retries. This removes tree copies, but many external servers can still
make search expensive. Benchmark this and check remote API capabilities.
Do not silently copy entire trees when an older server lacks the search API.

Use cozy-client's normal query/cache pipeline, centralized query definitions,
parameter-specific aliases and explicit fetch policies. Search results are not
mutable file documents: resolve the source file and route before preview,
download or edit, and keep index rows out of the mutable file cache.

### Choosing the engine

Choose one engine based on matching, permissions, folder hierarchy and load
tests. Reuse an already deployed service if it meets those requirements.
The current RAG service indexes selected assistant knowledge bases/content;
that does not prove it can provide complete file search with these permissions.

CouchDB sorted indexes can serve recents, but do not provide this search behavior.
Mango `$regex` does not use those indexes and is not a scalable substitute.
Compare an existing text-search service, an embedded index and a separate search
service, including instance ownership, replicas, recovery and operating cost.
Do not build a generic engine plugin framework.

Content extraction, OCR and semantic search are outside this ADR.

## Removing the browser DataProxy

Search and recents are only part of the work:

| Current use | What needs to change |
|---|---|
| `useRecentFiles` | Use the personal recents API for my edits and directly shared items. |
| Shared-bar `cozy-search` | Use backend file search and provide a replacement for contact/app results. |
| Drive `useSearch` / `indexFiles` | Remove `_all_docs` downloads and browser indexing, including older UI routes. |
| `DataProxyProvider` for files, contacts and apps | Replace all enabled consumers before removing it. |
| Feature-flagged `DataProxyLink` | Decide how to replace offline queries for the affected doctypes. |
| Native/Flagship search intent | Update supported clients or keep explicit compatibility behavior. |
| Shared-folder browsing and file actions | Keep the existing stack routes; they can work without the browser DataProxy. |

Contacts in sharing dialogs can use normal client queries. They do not belong
in the file index. Check indirect uses in cozy-bar, cozy-search and cozy-sharing.
This ADR provides online search; offline behavior needs a separate decision.
Only remove caches and subscriptions unused by Drive. Do not delete workers or
databases still needed by other apps or tabs.

## Rollout and validation

Roll this out separately from recents, using capability checks and feature flags.
Build and catch up the source indexes, verify permissions and matching, then
switch consumers. Define compatibility and rollback behavior without silently
bringing back full copies for every user.

Test at least these cases:

- Sharing a large company folder with more employees creates no extra copies
  of its search metadata. File updates do not write to each recipient's search
  index. The editor's personal recents may get a separate update.
- Many shares on one backend, many external backends, old unchanged files,
  overlapping shares and identical file IDs on different instances.
- Private parent names, group revocation during indexing/search, folder moves,
  unavailable sources and later result pages.
- Worker restarts, index rebuilds and every supported UI and result-opening path.

Measure record/index size, index update work, permission-check cost, indexing
delay, search time, remote requests, result size and rebuild cost. Check that
permission filtering and ranking still work when result/work limits are reached.
A shared index can still have expensive permission queries.

## Consequences

- Index storage grows with source files, rather than files multiplied by users.
  Normal infrastructure replicas still exist.
- Sharing changes access rules. Searching needs the source index to be available.
- We need a shared search service, reliable permission checks and a server-to-server
  query API. Many external sources remain a performance limit.
- Recents keeps its own small per-user activity records. It does not need a copy
  of the searchable file tree.

## Open questions before acceptance

- Which engine and index layout can meet the matching and performance needs?
  How will folder hierarchy and permission filtering work?
- How do stable source IDs, server authentication, current permission checks,
  multi-share queries, cross-server ranking and pagination work?
- What are the query and source-count limits? What happens with unsupported
  sources? What replaces contact/app search, and what works on mobile/offline?

## References

- [Personal recents](https://github.com/linagora/cozy-stack/pull/4949),
  [effective access](../../model/sharing/effective_access.go),
  [checkpointed indexing](../../model/rag/index.go), and [RAG scope](../ai.md).
- `cozy-libs`: `packages/cozy-dataproxy-lib/src/search/SearchEngine.ts`,
  `indexDocs.ts`, `consts.ts`, and
  `packages/cozy-search/src/components/Search/useFetchResult.jsx`.
- `twake-drive-web`: `src/modules/search/components/helpers.js`,
  `src/modules/search/hooks/useSearch.jsx`, `src/lib/DriveProvider.jsx`,
  `src/targets/browser/setupAppContext.js`.
- [CouchDB Mango index limitations](https://docs.couchdb.org/en/stable/ddocs/mango.html).
- Review baseline, 2026-09-20: cozy-stack `46fd052d4`, twake-drive-web
  `80e0f8141`, cozy-web-data-proxy `8f499e219`, cozy-client `404ee3af6`,
  cozy-libs `9d438658e`.
