# ADR: Recents from personal activity

## Status

Proposed

## Date

2026-10-24

## Context

Related to [server-side file search](https://github.com/linagora/cozy-stack/pull/4950).

Today, `cozy-web-data-proxy` creates a browser Pouch database for each received
share. To load recents, it waits for replication, reads up to 50 files from each
file database, then combines the results. Drive's fallback reads only local
`io.cozy.files`.

Both paths sort by `updated_at`, so a colleague's edit can move a shared file
into my recents. We want different behavior: show my own activity. Access to
a shared folder alone should add nothing.

## Decision

Store recents on the backend in a per-user `io.cozy.files.recents` doctype.
Keep one row per file or folder. An item is there because I edited or opened
it, whoever owns it. The source data stays in the owners' `io.cozy.files`.

Scope for now: only instances of the same stack are covered. A file whose
owner's instance lives on another stack gets no recents entry and no activity
tracking.

The handlers record activity directly in the recents doctype of each involved
instance. When the user loads recents, check permissions and fetch current
metadata for the candidate items. Do not copy shared trees or ask every owner
for recently modified files.

### What appears in recents

| What happens | Effect on my recents |
|---|---|
| I successfully edit a file, including inside a folder shared with me | Add the file or update my last-edited time. |
| I create a folder, including inside a folder shared with me | Add the folder or update my last-edited time. |
| Another member edits a shared file | Do not add it or change my recency. |
| Another member edits or renames an item already in my recents | Refresh its displayed metadata without changing my recency. |
| I open a document in its editor (notes, drawing, office) | Add it or update my last-viewed time. |
| I download or preview a file | Do not record a personal activity. |

Creating/uploading a file, creating a folder and saving a file's contents
count as edits, and so does restoring a file from the trash: the restorer acts
like the item was recreated, and this is a genuine personal edit. Rename, move,
trash, copy and permission changes do not count.

### Stored data and sorting

A row needs these fields:

```text
resource_key         string    stable source-instance ID + file ID
source_locator       string    where to fetch the original file
last_edited_at       string    my latest successful edit, if any (RFC 3339)
last_viewed_at       string    my latest open, if any (RFC 3339)
last_activity_at     string    max of the two fields above (RFC 3339)
schema_version       integer   version of this record format
```

Keep `last_edited_at` and `last_viewed_at` as separate components and compute
`last_activity_at` as their maximum. Adding a new kind of activity later then
only adds a component and recomputes the sort key, without losing the dates
already recorded.

The same file appears once. Files from different owners remain separate even
if their local IDs match.
Do not use names, paths or checksums to identify them. The stable instance ID
is the CouchDB-generated `_id` of the instance document
(`instance.Instance.DocID`): a UUID assigned once at creation, never modified,
independent of the domain, so it survives domain changes. The optional manager
`Instance.UUID` is not used for this, as operators may leave it empty.

Sort by `last_activity_at` descending, then `resource_key` descending. Use
the time of the successful edit or open, not the time the row was written.
Another person's change to `updated_at` must not affect this order.

### Recording edits reliably

Record the editor at the backend that actually saves the file. Use authenticated
user or sharing-member identity, including for files owned by another instance
on the same stack. The owner, proxy server and app name are not necessarily the
editor.

Map every supported write path to its identity source:

| Write path | Identity source |
|---|---|
| Browser/mobile with a webapp or mobile session | The instance owner. |
| Sharing write by a recipient (`share-interact` token) | The sharing member, resolved by the actor middleware. |
| `cozy-desktop` / `cozy-flagship` OAuth clients | The instance owner. |
| Other app tokens and konnectors | Not a person. No edit recorded. |
| OnlyOffice callback | The user or member who opened the editing session, recorded server-side when the session starts. |
| Background saves and workers | Not a person. No edit recorded. |

Verified against the current code: `cozyMetadata` records only app slugs, never
a person, so it cannot identify the editor. The actor middleware
(`ResolveRequestActor`) covers the owner session and `share-interact` members,
the two cases above. There is no inbound WebDAV server path; writes from remote
members arrive through the sharing drive routes. Do not trust a client-supplied
editor ID. Verify callback identities on the server.

Opening a document counts as personal activity, recorded by the routes that
open it, never detected from reads: a content read is the same HTTP request as
a download, a preview or a sync, whatever the app. Drive opens a drawing in its
excalidraw editor and a PDF file in its viewer with `GET /editor/:file-id/open`,
the notes editor loads a note with `GET /notes/:id`, and OnlyOffice sessions
are opened server-side. Each of these routes records the open for the person
behind the request, a sharing member included, read-only or not. Downloads,
previews, metadata reads, and reads with OAuth, CLI or konnector tokens never
count. A new editor must open its files through such a route for its opens to
be recorded. Mobile apps act with OAuth tokens, so their opens do not count
until a trusted-client rule covers them.

Keep one row per user/item pair. If Alice saves and then Bob saves, both users'
activity must survive: each user's row is written on their own instance. The
file's latest editor and `updated_at` are not enough. We need the latest edit
per person, not a log of every save.

CouchDB cannot write the file and its activity marker in one atomic operation.
Decision: the `files` handlers (`web/files/files.go`) and the `drives` handlers
(`web/sharings/drives.go`, for the routes that do not call the `files`
handlers) write the per-user/file recents row right after the successful file
write, in the same request. Failed writes must never become successful-edit
records: the row is written only after success. If the process dies between
the two writes, or the row write fails, that activity is lost; this is accepted
for a recents list.

All involved instances live on the same stack, so there is no
server-to-server delivery: no jobs, locks, retries or ordering machinery. The
row write upserts by `resource_key` and keeps the maximum date per component,
so writing a row twice is a no-op.

Dates are stamped by the server that captures the activity, never by the
client: a browser clock is not trusted. One stack means one clock, so no
cross-stack skew needs clamping.

### Reading recents safely

Add a dedicated endpoint, provisionally `GET /files/recents`. Read the user's
local recents records, with a matching sort index, a default page size of 50,
a maximum page size and an opaque cursor for the next page. The response is
JSON:API: one resource per item with the fields the interface needs (name,
type, size, last modification metadata), the source instance for items owned
by another instance, a `links.next` cursor and a `meta.incomplete` flag set
when the page may miss items. Only visible paths are returned, never the
owner's private parent folders.

For each candidate, check that it still belongs in recents and that the caller
can read it. Fetch current metadata and permissions directly from the owner's
instance database on the same stack, batching reads where possible. Requests
depend on the owners of candidate items, not every accessible share. The
owner's instance applies the same effective-access rules it applies for the
sharing itself.

Skip trashed, deleted or unauthorized items and read more candidates to fill
the page, within a fixed work limit. If an owner is unavailable or the limit
is reached, report that the result may be incomplete. If an access check
changes a row's visibility or sort date, repair it and refill or restart the
affected page.

Bind cursors to the user and the query parameters. Define how pagination
behaves when new activity arrives; refreshing starts a new traversal.
The client decides how many entries to display by choosing page sizes and pages;
the server keeps all history.

Being in recents and having access are separate checks. A row stays visible
only while I still have read access, by direct share, group or parent folder;
losing access hides it, regaining it shows it again with my date. Granting
access again must not create a fake personal edit.

Use the existing effective-access rules, including access through several shares,
and check the calling app's OAuth permissions. Hide items owned by another
instance whose access cannot be checked. Using cached permissions would need
an agreed maximum delay for revocation. Every later open, download or edit
still uses the existing file APIs and their permission checks.

Protect the recents doctype from direct access through generic `/data`, export
and realtime routes. Send authorized change notifications, not raw internal rows
or credentials.

Small metadata caches are allowed, but their contents need the same permission
checks. Keep recents record IDs/revisions separate from source file IDs/revisions.
Client code must use normal cozy-client queries, centralized definitions,
explicit aliases and fetch policies, with endpoint support in the library.
Offline, users see the last recorded state; optimistic offline updates may
come later, as client-side future work. Metadata already sent to a device
cannot be taken back.

### Storage and history

Storage grows with the files the user edits or opens.
Repeated activity updates an existing row. Measure real row sizes and database
replicas. A user who touches every file they can access can still build a
large history.

A 50-result page is not a storage limit: we keep all history. There is no
time or record-count limit. Pagination bounds what the client downloads, not
what the server stores. Rows are kept without expiry; growth with the number
of distinct items the user touches is accepted and measured at rollout.

## Rollout and validation

Roll this out separately from search, using capability checks and a feature flag.
At the switch from `cozy-web-data-proxy`, users must not see an empty page:
seed each user's recents with the 20 most recently modified files and created
folders they can see. A seeded file's `last_edited_at` is its last modification
date, a seeded folder's is its creation date.
The seed is best effort and capped at 20: a colleague's touch can appear in it
even though the normal rules would exclude it, and seeded rows become ordinary
rows, updated by real activity afterwards. Beyond the seed, import past edits
only when existing data proves who edited the file and what they did. If
reliable dates or history are missing, leave them unknown or start recording
at rollout. Migration itself must not make old items look newly edited.

Files owned by an instance on another stack are out of scope for now: they
simply do not appear in recents, and there is no inter-stack activity delivery
to degrade. Removing DataProxy completely also needs the migrations in the
search ADR.

Check at least these cases:

- The switch seeds at most 20 rows with real dates; no seeded row is stamped
  with the migration time.
- Alice edits, then Bob edits the same file: keep both users' activity.
- Editing a file inside a shared 100,000-file folder adds one row, for that
  file only.
- Creating a folder inside a shared folder adds one row for that folder only,
  not for its contents.
- A colleague's edit or rename refreshes display data without changing recency.
- Crashes during saves, stale callbacks and every supported editor/client
  path.
- Losing all my access to a shared item: the row is hidden; access restored:
  it reappears with my date, without a fake edit.
- Filtering before page limits, later pages, history limits, unavailable owners
  and different owners using the same local file ID.

Measure capture delays, permission-check reads, record count and query time.
Confirm that one shared-file edit does not write to every member with access.
Document unsupported write paths before rollout.

## Alternatives considered

- Asking owners for their most recently modified files includes colleagues'
  edits and does not give us a personal list.
- Copying every accessible file to the user's stack stores too much data.
- Filtering by the file's latest editor loses my earlier edit after someone
  else saves it.
- Recording activity only in the browser misses other devices and editors.

## Consequences

- Recents becomes a personal list of my edits and opens. A folder gets a row
  only when I create it: browsing a folder or listing its contents is not
  personal activity.
- Recents and search use the same resource IDs and permission rules, but store
  different data.
- We need reliable editor identity. Existing `updated_at` values cannot provide
  personal edit history.

## References

- [Server-side search](https://github.com/linagora/cozy-stack/pull/4950),
  [sharing member provenance](../../model/sharing/member.go),
  [request actor resolution](../../web/middlewares/actor.go),
  [file attribution metadata](../../model/vfs/cozy_metadata.go),
  [file mutation handlers](../../web/files/files.go),
  [drive mutation handlers](../../web/sharings/drives.go),
  [effective access](../../model/sharing/effective_access.go), and
  [sharing setup](../../model/sharing/setup.go).
- `cozy-web-data-proxy`: `src/dataproxy/worker/worker.ts` and `data.ts`;
  `twake-drive-web`: `src/hooks/useRecentFiles.jsx` and `src/queries/index.ts`.
- Review baseline, 2026-09-20: cozy-stack `46fd052d4`, twake-drive-web
  `80e0f8141`, cozy-web-data-proxy `8f499e219`, cozy-client `404ee3af6`,
  cozy-libs `9d438658e`.
