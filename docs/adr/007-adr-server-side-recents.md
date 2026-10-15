# ADR: Personal recents from personal activity

## Status

Draft

Related to [server-side file search](https://github.com/linagora/cozy-stack/pull/4950).

## Context

Today, `cozy-web-data-proxy` creates a browser Pouch database for each received
share. To load recents, it waits for replication, reads up to 50 files from each
file database, then combines the results. Drive's fallback reads only local
`io.cozy.files`.

Both paths sort by `updated_at`, so a colleague's edit can move a shared file
into my recents. We want different behavior: show my own activity, whoever owns
the file. Access to a shared folder alone should add nothing.

Copying a whole shared folder is unnecessary for this list. Showing 50 results
also does not mean we only download or store 50 records.

## Decision

Store recents on the backend in a per-user `io.cozy.files.recents` doctype.
Keep one row per file. An item is there because I edited or opened it, whoever
owns it. The source data stays in the owners' `io.cozy.files`.

Send only these activity updates to the user's stack. When the user
loads recents, check permissions and fetch current metadata for the candidate
items. Do not copy shared trees or ask every owner for recently modified files.

### What appears in recents

| What happens | Effect on my recents |
|---|---|
| I successfully edit a file, including inside a folder shared with me | Add the file or update my last-edited time. |
| Another member edits a shared file | Do not add it or change my recency. |
| Another member edits or renames an item already in my recents | Refresh its displayed metadata without changing my recency. |
| I edit or open a child item of a shared folder | That item gets its own entry. |
| I open a document in its editor (notes, drawing, office) | Add it or update my last-viewed time. |
| I download or preview a file | Do not record a personal activity. |
| A background reader, a sync client or a background job updates it | Do not record a personal edit. |
| An edit fails | Do not record a successful edit. |

Creating/uploading a file and saving its contents count as edits, and so does
restoring a file from the trash: the restorer acts like the file was recreated,
and this is a genuine personal edit. Rename, move, trash, copy and permission
changes do not count. A write made with an app token is not a human edit,
except the trusted sync clients listed below.

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
the time of the successful edit or open, not the time a worker processed it.
Another person's change to `updated_at` must not affect this order.

For example, I edit a file on Monday and a colleague edits it on Tuesday.
My recency stays Monday.

Being in recents and having access are separate checks. A row stays visible
only while I still have read access, by direct share, group or parent folder;
losing access hides it, regaining it shows it again with my date. Hide
trashed, deleted or inaccessible items. Granting access again must not create
a fake personal edit; restoring, by contrast, is a genuine edit of the user
who restores.

### Recording edits reliably

Record the editor at the backend that actually saves the file. Use authenticated
user or sharing-member identity, including for remote files. The owner, proxy
server and app name are not necessarily the editor.

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

Opening a document counts as personal activity, detected from reads. The same
HTTP request serves an editor open and a download, so the stack tells them
apart by who reads what: a content read by the app that edits this file type
counts as an open — a note read by the notes app, a drawing read by the
excalidraw app. Downloads, previews and reads of non-editable files by any app
do not count. Reads with OAuth, CLI or konnector tokens never count, and
neither do metadata reads such as folder listings. OnlyOffice sessions are
visible server-side and need no heuristic. Audit each editor app's background
reads before rollout: an editor that starts prefetching would create false
opens. Mobile apps read with OAuth tokens, so their opens do not count until a
trusted-client rule covers them.

Keep a durable last-activity record for each user/file pair. If Alice saves and
then Bob saves before a worker runs, both users' activity must survive.
The file's latest editor and `updated_at` are not enough. We need the latest
edit per person, not a log of every save.

CouchDB cannot write the file and its activity marker in one atomic operation.
Decision: record the durable per-user/file activity right after the successful
file write, and run a periodic reconciliation job over the durable records. A
crash between the file write and the activity record can lose one edit; this
window is accepted and documented. Failed writes must never become
successful-edit records: the activity record is written only after success.

A CouchDB changes feed over file documents can miss intermediate edits.
A feed over the durable user/file activity records can support replay instead.

### Sending updates between servers

Reuse existing jobs, locks and authenticated sharing connections. Send edit
activity to the editor's stack. Repeated saves for the same user/file can be
combined into one latest update. Batch updates going to the same
recipient/backend. An edit must not send activity to everyone with access.

Workers must handle retries and out-of-order delivery:

- Acknowledge successful delivery and keep failed updates for retry.
- Save progress only after writes succeed, checking each result in a bulk write.
- Keep newer activity when an older edit arrives.

The receiver upserts by `resource_key` and keeps the maximum date per
component, so replaying an update and receiving updates out of order are both
no-ops. At-least-once delivery with retries is therefore sufficient. If
activity retraction is ever added, that semantics is no longer enough and
updates will need ordering information. Delivery is best effort: there is no
latency requirement; rollout metrics observe actual delays.

### Clock differences

Dates are stamped by the server that captures the activity, never by the
client: a browser clock is not trusted. The receiving stack clamps future
timestamps to its own clock (`min(source_ts, receiver_now)`): a row stamped
far ahead would otherwise sit on top of recents and poison the keep-newer
rule, because every later real activity would look older. Stacks are expected
to run NTP; residual skew of seconds only causes cosmetic ordering
differences between local and remote activities.

WebSockets can wake workers or refresh an open screen. They cannot be the only
way to recover updates. Periodic jobs should catch up in batches from saved
activity records.

This server-to-server delivery is new work. Existing browser WebSockets and
ordinary sharing replication do not already provide it for Drive shares.

### Reading recents safely

Add a dedicated endpoint, provisionally `GET /files/recents`. Read the user's
local recents records, with a matching sort index, a default page size of 50,
a maximum page size and an opaque cursor for the next page. The response is
JSON:API: one resource per item with the fields the interface needs (name,
type, size, last modification metadata), the source instance for remote items,
a `links.next` cursor and a `meta.incomplete` flag set when the page may miss
items. Only visible paths are returned, never the owner's private parent
folders.

For each candidate, check that it still belongs in recents and that the caller
can read it. Fetch current metadata and permissions from its owner, batching
requests where possible. Requests depend on the owners of candidate items, not
every accessible share.

No existing inter-stack route provides batched metadata plus read-access
checks. Add one, authenticated with the sharing credentials, for example
`POST /sharings/recents/meta`: the request lists source IDs, the response
gives visible metadata and a read-access verdict per ID, and nothing else.
The owner's stack applies the same effective-access rules it applies for the
sharing itself.

Skip trashed, deleted or unauthorized items and read more candidates to fill
the page, within a fixed work limit. If an owner is unavailable or the limit
is reached, report that the result may be incomplete. If an access check
changes a row's visibility or sort date, repair it and refill or restart the
affected page.

Bind cursors to the user, query parameters and recents rebuild version. Define how
pagination behaves when new activity arrives; refreshing starts a new traversal.
The client decides how many entries to display by choosing page sizes and pages;
the server keeps all history.

Use the existing effective-access rules, including access through several shares,
and check the calling app's OAuth permissions. Hide remote items whose access
cannot be checked. Using cached permissions would need an agreed maximum delay
for revocation. Every later open, download or edit still uses the existing file
APIs and their permission checks.

Protect the recents doctype from direct access through generic `/data`, export
and realtime routes. Send authorized change notifications, not raw internal rows
or credentials. Return only visible paths; never reveal the owner's private
parent folders.

Small metadata caches are allowed, but their contents need the same permission
checks. Keep recents record IDs/revisions separate from source file IDs/revisions.
Client code must use normal cozy-client queries, centralized definitions,
explicit aliases and fetch policies, with endpoint support in the library.
Offline, users see the last recorded state; optimistic offline updates may
come later, as client-side future work. Metadata already sent to a device
cannot be taken back.

### Storage and history

Storage grows with the files the user edits or opens, whoever owns them.
Repeated activity updates an existing row. Measure real row sizes, delivery
records and database replicas. A user who touches every file they can access
can still build a large history.

A 50-result page is not a storage limit: we keep all history. There is no
time or record-count limit. Pagination bounds what the client downloads, not
what the server stores. Source activity records are kept without expiry, so
recents can always be rebuilt; growth with the user's activity, not with the
number of distinct files, is accepted and measured at rollout.

## Rollout and validation

Roll this out separately from search, using capability checks and a feature flag.
Import past edits only
when existing data proves who edited the file and what they did. If reliable
dates or history are missing, leave them unknown or start recording at rollout.
Migration itself must not make old items look newly edited or shared.

Older remote stacks may not support personal activity delivery. Show this as
incomplete history; do not silently use their global modification dates.
Removing DataProxy completely also needs the migrations in the search ADR.

Check at least these cases:

- Alice edits, then Bob edits before processing: keep both users' activity.
- Editing a file inside a shared 100,000-file folder adds one row, for that
  file only.
- A colleague's edit or rename refreshes display data without changing recency.
- Retries, late updates, crashes during saves/delivery, stale callbacks and
  every supported editor/client path.
- Losing all my access to a shared item: the row is hidden; access restored:
  it reappears with my date, without a fake edit.
- Filtering before page limits, later pages, history limits, unavailable owners
  and different owners using the same local file ID.

Measure capture/delivery delays, failed jobs, permission-check requests, record
count, query time and rebuild time. Confirm that one shared-file edit does not
write to every member with access. Document unsupported write paths before rollout.

## Alternatives considered

- Asking owners for their most recently modified files includes colleagues'
  edits and does not give us a personal list.
- Copying every accessible file to the user's stack stores too much data.
- Filtering by the file's latest editor loses my earlier edit after someone
  else saves it.
- Recording activity only in the browser misses other devices and editors.

## Consequences

- Recents becomes a personal list of my edits and opens, whoever owns the item.
  Folders no longer get rows: no personal activity applies to them today.
- Large shared folders do not require copying their contents into recents.
- Recents and search use the same resource IDs and permission rules, but store
  different data.
- We need reliable editor identity, crash recovery and server-to-server delivery.
  Existing `updated_at` values cannot provide personal edit history.

## References

- [Server-side search](https://github.com/linagora/cozy-stack/pull/4950),
  [sharing member provenance](../../model/sharing/member.go),
  [request actor resolution](../../web/middlewares/actor.go),
  [file attribution metadata](../../model/vfs/cozy_metadata.go),
  [file mutation handlers](../../web/files/files.go),
  [effective access](../../model/sharing/effective_access.go), and
  [sharing setup](../../model/sharing/setup.go).
- `cozy-web-data-proxy`: `src/dataproxy/worker/worker.ts` and `data.ts`;
  `twake-drive-web`: `src/hooks/useRecentFiles.jsx` and `src/queries/index.ts`.
- Review baseline, 2026-09-20: cozy-stack `46fd052d4`, twake-drive-web
  `80e0f8141`, cozy-web-data-proxy `8f499e219`, cozy-client `404ee3af6`,
  cozy-libs `9d438658e`.
