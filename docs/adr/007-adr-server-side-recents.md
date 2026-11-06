# ADR: Recents from personal activity

## Status

Proposed

## Date

2026-11-05

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
it, whoever owns it. Each row also holds what Drive needs to display the item,
kept up to date when the item changes, so that listing recents reads nothing
but the user's own rows.

Scope for now: only instances of the same stack are covered. A file whose
owner's instance lives on another stack gets no recents entry and no activity
tracking. Instance moves are not handled either (see the known limits).

### What appears in recents

| What happens | Effect on my recents |
|---|---|
| I successfully edit a file, including inside a folder shared with me | Add the file or update my last-edited time. |
| I create a folder, including inside a folder shared with me | Add the folder or update my last-edited time. |
| Another member edits a shared file | Do not add it or change my recency. |
| Another member edits or renames an item already in my recents | Refresh its displayed metadata without changing my recency. |
| I open a document in its editor (notes, drawing, office) | Add it or update my last-viewed time. |
| I download or preview a file | Do not record a personal activity. |
| An item of my recents is trashed | Keep it: it comes back with its row when restored. |
| An item of my recents is destroyed for good | Remove it. |

Creating/uploading a file, creating a folder and saving a file's contents
count as edits, and so does restoring a file from the trash: the restorer acts
like the item was recreated, and this is a genuine personal edit. Rename, move,
trash, copy and permission changes do not count. The automatic writes of the
flagship app do not count either: the photo backup (files tagged with
`metadata.backupDeviceIds` and its folders), the konnector folders, and the
konnector traces and support logs under `/Settings/Logs` and
`/Settings/AALogs`. Uploads from the share sheet of the mobile app count as
file creations.

### Stored data and sorting

A row is keyed by the ID of its file or folder, used as its `_id`:

```text
drive_id             string    sharing ID of the shared drive, for an item of
                               a drive the user does not own
owner_domain         string    domain of the owner's instance, for the same
                               items
last_edited_at       string    my latest successful edit, if any (RFC 3339)
last_viewed_at       string    my latest open, if any (RFC 3339)
last_activity_at     string    max of the two fields above (RFC 3339)
name                 string    display metadata of the item: name, type,
type                 string    MIME type, class, size, last modification,
mime                 string    parent folder and path
class                string
size                 string
updated_at           string
dir_id               string
path                 string    from the root of the drive for a drive item
schema_version       integer   version of this record format
```

Keep `last_edited_at` and `last_viewed_at` as separate components and compute
`last_activity_at` as their maximum. Adding a new kind of activity later then
only adds a component and recomputes the sort key, without losing the dates
already recorded.

The items that can enter my recents are the files and folders of my instance,
and the items of the shared drives I am a member of, which stay on the owner's
instance. Drive opens a drive item with the ID of its sharing (`driveId`) and
its file ID, and finds the members of the drive in the sharing document of my
instance: the file ID and `drive_id` identify the item, and `owner_domain`
finds the rows of a user on the items of an owner when their drives change. The
internal ID of an instance is never stored. File and folder IDs are UUIDs,
unique across the instances of the stack, except the fixed IDs that every
instance shares (`io.cozy.files.` prefix: root, trash, system folders), on
which no activity is recorded. A move between two drives of the same owner
keeps the ID of the item and only changes its `drive_id`.

An item can be in several drives: a folder or a file of a drive can be the root
of another drive, nested in the first one, with its own members. A row holds
one of them, computed on the owner's instance from the position of the item,
never from the route or the code of the request, with the same rule when the
activity is recorded and when the row is updated: among the drives that cover
the item and of which the user is a ready member, the drives where the user can
write come first, then the outermost one. Drive shows an item with the rights
of the member in the drive of the item, so this drive gives the highest rights;
among drives with the same rights, the outermost one does not change when a
drive is created inside it. `path` is relative to the root of that drive, and
empty for the root itself.

Sort by `last_activity_at` descending, then `_id` descending. Use the time of
the successful edit or open, not the time the row was written. Another
person's change to `updated_at` must not affect this order.

### Recording edits reliably

Record the editor at the backend that actually saves the file. Use authenticated
user or sharing-member identity, including for files owned by another instance
on the same stack. The owner, proxy server and app name are not necessarily the
editor.

Map every supported write path to its identity source:

| Write path | Identity source |
|---|---|
| Browser/mobile with a webapp or mobile session | The instance owner. |
| Shared drive proxy (same stack) | The recipient, when their original token is a person. |
| Shared drive member with a `share-interact` or `share-preview` code (notes and editors opened on the owner's instance) | The drive member, resolved by the actor middleware, once they have accepted the sharing. |
| Flagship OAuth client (certified, or with the `*` scope when its context skips the certification) | The instance owner, except for its automatic writes (see above). |
| OAuth clients that declare the `desktop` kind (`cozy-desktop`) | The instance owner. |
| Other app tokens and konnectors | Not a person. No edit recorded. |
| OnlyOffice callback | The user or member who opened the editing session, recorded server-side when the session starts. |
| Background saves and workers | Not a person. No edit recorded. |

Verified against the current code: `cozyMetadata` records only app slugs, never
a person, so it cannot identify the editor. The actor middleware
(`ResolveRequestActor`) tells, from the original token of the request, whether
it stands for a person, and carries the instance of that person when it lives
on this stack. The shared drive proxy swaps the recipient's token for the drive
token on the owner's side, and keeps the actor resolved from the original
token; a proxied route without authentication gets an actor that is not a
person. A file of a shared drive is always opened on the owner's instance: the
open route of the drive gives the member a `share-interact` code (or a
`share-preview` code, read-only), which the notes editor then uses directly on
the owner's instance. These activities are recorded for the member when the
item is in a drive they are a member of, with the drive chosen as above. A
member whose instance is on another stack is skipped silently. Do not trust a
client-supplied editor ID. Verify callback identities on the server.

Known limit: the `desktop` kind is declared by the OAuth client itself, like
the other client metadata. Any OAuth client that registers with this kind
counts as its user.

Known limit: the shared drive proxy checks the original token only when it
serves the owner's instance directly on this stack. When it cannot load that
instance from the URL of the owner (an alias, an old domain, a port, a
transient error), it forwards the request over HTTP, and the owner's side only
sees the drive token: any request of the recipient then counts as the
recipient, a konnector or service token included.

Known limit: whoever holds the `share-interact` or `share-preview` code of a
drive member counts as that member.

Opening a document counts as personal activity, recorded by the routes that
open it, never detected from reads: a content read is the same HTTP request as
a download, a preview or a sync, whatever the app. Drive opens a drawing in its
excalidraw editor and a PDF file in its viewer with `GET /editor/:file-id/open`,
the notes editor loads a note with `GET /notes/:id`, and OnlyOffice sessions
are opened server-side. Each of these routes records the open for the person
behind the request. The mobile app shows Drive and Notes in webviews with
session-bound app tokens, so its opens count like the browser's. Downloads,
metadata reads, and reads with OAuth, CLI or konnector tokens never count, and
neither does the `cozy-viewer` preview of Drive, which only reads the content
of the file. A new editor must open its files through such a route for its
opens to be recorded.

Keep one row per user/item pair. If Alice saves and then Bob saves, both users'
activity must survive: each user's row is written on their own instance. The
file's latest editor and `updated_at` are not enough. We need the latest edit
per person, not a log of every save.

CouchDB cannot write the file and its activity marker in one atomic operation.
Decision: the `files` handlers (`web/files/files.go`) and the `drives` handlers
(`web/sharings/drives.go`, for the routes that do not call the `files`
handlers) write the per-user/file recents row right after the successful file
write, in the same request, with the metadata of the item as written. Failed
writes must never become successful-edit records: the row is written only after
success. If the process dies between the two writes, or the row write fails,
that activity is lost; this is accepted for a recents list. The row write
upserts by `_id` and keeps the maximum date per component, so writing a row
twice is a no-op.

Two activities of the same kind on a row less than 60 seconds apart are one:
the row is not rewritten, unless its metadata changed, so that the hot write
paths (typing in a note, a sync client uploading files) do not write it on
every request.

Dates are stamped by the server that captures the activity, never by the
client: a browser clock is not trusted. One stack means one clock, so no
cross-stack skew needs clamping.

### Keeping the metadata up to date

When a file or folder changes on its owner's instance (rename, move, new
content, trash or restore), a job updates the rows that the owner and the
members of the shared drives giving access to the item hold for it: only the
members on this stack, with the recents enabled, that still have access and
already have a row. Destroying an item for good deletes these rows. The job is
fed by the stack's own events on `io.cozy.files`, not by the clients, and
coalesces the changes of an item, so that frequent saves cause a bounded number
of updates. An edit of a shared file writes only the rows that exist for it,
never a row for every member with access.

A move within the instance of the owner keeps the ID of the item: its rows
follow it like any other change. A move to the shared drive of another owner of
the stack creates the items again there, with new IDs, and deletes the
originals. The move knows the old and new ID of each item it creates again, and
stores this table once. For the owner and the members of the source drive that
have the recents enabled, a job moves the rows under the old IDs to the new
IDs, with their dates merged at their maximum, and with the `drive_id` of the
destination for the members of that drive. The propagation of the deletion of
the originals reads the same table, so that it never deletes a row before it is
moved. A user who cannot open the destination loses the row, as for an item
destroyed for good.

When the drives of a member change on the owner's instance (the member accepts
a drive or is revoked from it, their rights in it change, or a drive is created
inside another one), a job takes the rows of that member on the items of the
owner, found by `owner_domain`, chooses their drive again with the same rule,
and refreshes their `drive_id`, `path` and metadata. It is fed by the same
events, on `io.cozy.sharings`. A row whose item the member cannot open any more
is left as it is.

Known limit: an instance that has lost access to an item keeps the last
metadata it was given until it is given access again: its row is not updated
meanwhile, so a rename made after the revocation is not shown to it. Opening
the item still goes through the usual rights.

Known limit: when one of my items moves to the shared drive of another owner
that I cannot open, my row is deleted, and being given access to that drive
afterwards does not bring it back: it only comes back with my next activity on
the item.

Known limit: `path` is updated when the item itself changes, not when one of
its parent folders is renamed or moved: it can be out of date until the next
change of the item. `dir_id` stays right, and the link to the parent folder
with it.

Known limit: rows whose item can never be shown again without being destroyed
for good (the instance of a shared drive destroyed, a member who left a drive)
are kept for now. Storing too many rows is accepted until a cleanup is
designed.

Known limit: instance moves are not handled. The rows of the members on the
items of a moved instance keep its metadata, frozen with the instance, until it
is destroyed.

### Reading recents

Add a dedicated endpoint, provisionally `GET /files/recents`. It reads the
user's rows with a sort index on `(last_activity_at, _id)`, a default page size
of 50, a maximum page size and a cursor for the next page, the position
`(last_activity_at, _id)` of the last row of the page. The response is JSON:API:
one resource per row, built from its stored fields only, with a `links.next`
cursor. No request is made to the owners' instances.

Bind cursors to the user and the query parameters. A traversal goes on after
the position of its cursor, whatever activity happens meanwhile; refreshing
starts a new traversal. The client decides how many entries to display by
choosing page sizes and pages; the server keeps all history.

Being in recents does not check access: a row stays listed after its item has
become unreadable, with the last metadata it was given (see the known limits).
Every later open, download or edit uses the existing file APIs and their
permission checks.

Apps with a permission on `io.cozy.files.recents` can subscribe to its
realtime events: the rows hold nothing more than the file IDs, drive IDs,
owner domains and display metadata that the user already sees. The generic
`/data` routes, `@event` triggers and instance exports stay closed to the
doctype. Client code must use normal cozy-client queries, centralized
definitions, explicit aliases and fetch policies, with endpoint support in the
library. Offline, users see the last recorded state; optimistic offline updates
may come later, as client-side future work.

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
folders they can see, with their metadata. A seeded file's `last_edited_at` is
its last modification date, a seeded folder's is its creation date.
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
- A colleague's edit or rename refreshes the stored metadata of the members
  that have a row, without changing recency.
- A note of a shared drive opened and edited by a member is in the member's
  recents, with the drive.
- Destroying an item for good removes its rows; trashing it keeps them.
- Joining or leaving a drive, or a change of rights in it, moves the rows of
  the member to the drive that gives the highest rights.
- An item moved to the shared drive of another owner keeps its rows, under
  its new ID, for the users who can open it there.
- Crashes during saves, stale callbacks and every supported editor/client
  path.
- Paging through recents gives every row once, in order, even when new
  activity happens between two pages.
- Old rows stay reachable through the pages: nothing caps or expires the
  history.
- Frequent saves of a shared document (notes, OnlyOffice) cause a bounded
  number of row writes, for the editor and for the update of the other
  members' rows.

Measure capture delays, update jobs, record count and query time. Document
unsupported write paths before rollout.

## Alternatives considered

- Asking owners for their most recently modified files includes colleagues'
  edits and does not give us a personal list.
- Copying every accessible file to the user's stack stores too much data.
- Filtering by the file's latest editor loses my earlier edit after someone
  else saves it.
- Recording activity only in the browser misses other devices and editors.
- Reading the metadata and the access from the owners' instances at each read
  keeps the rows minimal, but makes every page cost requests to every owner,
  page refills within a work limit, and repairs of the rows; it also keeps the
  doctype out of realtime.
- Keying the rows by the internal ID of the source instance survives domain
  changes, but exposes that ID to the members of a shared drive, and is not
  needed once the rows are not read back from the owners.

## Consequences

- Recents becomes a personal list of my edits and opens. A folder gets a row
  only when I create it: browsing a folder or listing its contents is not
  personal activity.
- Listing recents reads only the user's own database, and the list can be kept
  live with realtime.
- The cost moves to the writes: a change of an item updates the rows that
  exist for it, through a job.
- Recents and search use the same item IDs, but store different data.
- We need reliable editor identity. Existing `updated_at` values cannot provide
  personal edit history.

## References

- [Server-side search](https://github.com/linagora/cozy-stack/pull/4950),
  [sharing member provenance](../../model/sharing/member.go),
  [request actor resolution](../../web/middlewares/actor.go),
  [file attribution metadata](../../model/vfs/cozy_metadata.go),
  [file mutation handlers](../../web/files/files.go),
  [drive mutation handlers](../../web/sharings/drives.go),
  [file openers](../../model/sharing/open_file.go),
  [effective access](../../model/sharing/effective_access.go), and
  [sharing setup](../../model/sharing/setup.go).
- `cozy-web-data-proxy`: `src/dataproxy/worker/worker.ts` and `data.ts`;
  `twake-drive-web`: `src/hooks/useRecentFiles.jsx`, `src/queries/index.ts`,
  `src/modules/filelist/virtualized/cells/columns/ShareCell.jsx` and
  `src/modules/navigation/hooks/helpers.ts`.
- Review baseline, 2026-09-20: cozy-stack `46fd052d4`, twake-drive-web
  `80e0f8141`, cozy-web-data-proxy `8f499e219`, cozy-client `404ee3af6`,
  cozy-libs `9d438658e`.
