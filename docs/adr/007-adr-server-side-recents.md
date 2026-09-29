# ADR: Personal recents from edits and direct shares

## Status

Draft

Related to ADR 008: server-side file search.

## Context

Today, `cozy-web-data-proxy` creates a browser Pouch database for each received
share. To load recents, it waits for replication, reads up to 50 files from each
file database, then combines the results. Drive's fallback reads only local
`io.cozy.files`.

Both paths sort by `updated_at`, so a colleague's edit can move a shared file
into my recents. We want different behavior: show my edits and items shared
directly with me. Access to a company folder alone should add nothing.

Copying a whole shared folder is unnecessary for this list. Showing 50 results
also does not mean we only download or store 50 records.

## Decision

Store recents on the backend in a per-user `io.cozy.files_recents` doctype.
Keep one row per file or folder. An item can have two reasons for being there:
I edited it, or someone shared it directly with me.
The source data stays in `io.cozy.files` and `io.cozy.sharings`.

Send only these activity and sharing updates to the user's stack. When the user
loads recents, check permissions and fetch current metadata for the candidate
items. Do not copy shared trees or ask every owner for recently modified files.

### What appears in recents

| What happens | Effect on my recents |
|---|---|
| I successfully edit a file, including inside a company folder | Add the file or update my last-edit time. |
| Someone shares a file directly with me | Add it when the share becomes usable. |
| Someone shares a folder directly with me | Add the folder itself. Do not add its contents. |
| A colleague edits a company file | Do not add it or change my recency. |
| A colleague edits or renames an item already in my recents | Refresh its displayed metadata without changing my recency. |
| I get access through a group or parent folder | Do not add a direct-share reason or entries for the contents. |
| I edit a child item, or someone shares it directly with me | That item gets its own entry. |
| I open or download a file, or a background job updates it | Do not record a personal edit. |
| An edit fails | Do not record a successful edit. |

A direct share names the user as a recipient. Expanding a group into a list of
members does not make those members direct recipients. Reuse
`Member.OnlyInGroups` after checking that it is set and passed along correctly.
A user can have both group access and a direct share.

Pending invitations count only after acceptance or automatic acceptance gives
access. Visiting a public link does not count as a direct share.

Creating/uploading a file and saving its contents count as edits. Decide whether
rename, move and other actions count before implementation. A write made with
an app token is not necessarily a human edit.

### Stored data and sorting

A row needs these fields:

```text
resource_key       stable source-instance ID + file/folder ID
source_locator     where to fetch the original file or folder
last_my_edit_at     my latest successful edit, if any
direct_share_refs  active direct shares and when each became usable
recent_at          latest time from my edit or an active direct share
schema_version     version of this record format
```

The same file can have an edit reason and several direct shares, but appears
once. Files from different owners remain separate even if their local IDs match.
Do not use names, paths or checksums to identify them. Define stable instance IDs
and how they survive domain changes before implementation.

Sort by `recent_at` descending, then `resource_key` descending. Use the time of
the successful edit or usable share, not the time a worker processed it.
Another person's change to `updated_at` must not affect this order.

For example, I edit a file on Monday and a colleague edits it on Tuesday.
My recency stays Monday. If someone shares it directly with me on Wednesday,
it moves to Wednesday because of that share. If that share is later removed
but I still have group access, my edit reason remains and the date returns
to Monday.

Being in recents and having access are separate checks. Removing a direct share
removes that reason even if group access remains. Keep the row visible only
while another reason and read access remain. Hide trashed, deleted or
inaccessible items. Restoring a file or granting access again must not create
a fake personal edit.

### Recording edits reliably

Record the editor at the backend that actually saves the file. Use authenticated
user or sharing-member identity, including for remote files. The owner, proxy
server and app name are not necessarily the editor.

Check every supported write path: browser/mobile, desktop/WebDAV, office-editor
callbacks and background saves. Verify callback identities on the server.
Do not trust a client-supplied editor ID. Existing actor middleware and
`cozyMetadata` can help, but app/instance attribution is not always a person.
Check what they cover before adding another identity mechanism.

Keep a durable last-edit record for each user/file pair. If Alice saves and
then Bob saves before a worker runs, both users' activity must survive.
The file's latest editor and `updated_at` are not enough. We need the latest
edit per person, not a log of every save.

Choose a way to recover activity if the server crashes during a save. This can
be a marker saved atomically with the source change, or a durable operation
record/outbox that can confirm completion and recover unfinished work. Saving
the file and then sending a best-effort job can lose activity between those
steps. Failed writes must never become successful-edit records.

A CouchDB changes feed over file documents can miss intermediate edits.
A feed over the durable user/file activity records can support replay instead.

### Sending updates between servers

Reuse existing jobs, locks and authenticated sharing connections. Send edit
activity to the editor's stack and direct-share changes to the named recipients.
Repeated saves for the same user/file can be combined into one latest update.
Batch updates going to the same recipient/backend. A company file edit must
not send activity to every employee.

Workers must handle retries and out-of-order delivery:

- Give updates a stable ID or version so replaying them is safe.
- Acknowledge successful delivery and keep failed updates for retry.
- Save progress only after writes succeed, checking each result in a bulk write.
- Reject old share versions so a late update cannot restore a revoked share.
- Keep newer activity when an older edit arrives. Define how to handle clock
  differences between source servers.

WebSockets can wake workers or refresh an open screen. They cannot be the only
way to recover updates. Periodic jobs should catch up in batches from saved
activity and direct-share records. Repair those records without scanning shared-folder
contents; a directly shared folder needs only its root metadata.

This server-to-server delivery is new work. Existing browser WebSockets and
ordinary sharing replication do not already provide it for Drive shares.

### Reading recents safely

Add a dedicated endpoint, provisionally `GET /files/recents`. Read the user's
local recents records, with a matching sort index, a default page size of 50,
a maximum page size and an opaque cursor for the next page.

For each candidate, check that it still belongs in recents and that the caller
can read it. Fetch current metadata and permissions from its owner, batching
requests where possible. A shared-folder row fetches the folder itself.
Requests depend on the owners of candidate items, not every accessible share.

Skip trashed, deleted or unauthorized items and read more candidates to fill
the page, within a fixed work limit. If an owner is unavailable or the limit
is reached, report that the result may be incomplete. If a share check changes
a row's reasons or sort date, repair it and refill or restart the affected page.

Bind cursors to the user, query parameters and recents rebuild version. Define how
pagination behaves when new activity arrives; refreshing starts a new traversal.

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
Decide offline behavior separately: metadata already sent to a device cannot
be taken back.

### Storage and history

Storage grows with files the user edits and roots shared directly with them.
Repeated edits update an existing row. A company group share creates no personal
recents rows. Sharing one folder directly with 1,000 people creates up to 1,000
folder rows, regardless of how many files it contains.

For illustration, at 1 kB per row, those 1,000 folder rows take about 1 MB.
Copying a 100,000-file tree to all 1,000 people would take about 100 GB.
Measure real row sizes, delivery records and database replicas. A user who edits
every file can still build a large history.

A 50-result page is not a 50-record history limit. Before rollout, choose a time
or record-count limit, or explicitly decide to keep all history. Keep enough
source activity and sharing data to rebuild that promised history. If old
activity expires, explain the limit instead of filling recents with colleagues'
edits.

## Rollout and validation

Roll this out separately from search, using capability checks and a feature flag.
Seed active direct shares from reliable sharing records. Import past edits only
when existing data proves who edited the file and what they did. If reliable
dates or history are missing, leave them unknown or start recording at rollout.
Migration itself must not make old items look newly edited or shared.

Older remote stacks may not support personal activity delivery. Show this as
incomplete history; do not silently use their global modification dates.
Removing DataProxy completely also needs the migrations in the search ADR.

Check at least these cases:

- Alice edits, then Bob edits before processing: keep both users' activity.
- A directly shared 100,000-file folder adds one row; group access adds none.
  An edited or directly shared child gets its own row.
- A colleague's edit or rename refreshes display data without changing recency.
- Retries, late updates, crashes during saves/delivery, stale callbacks and
  every supported editor/client path.
- Removing a direct share with group access remaining, with or without a
  personal-edit reason; losing all access while history is being imported.
- Filtering before page limits, later pages, history limits, unavailable owners
  and different owners using the same local file ID.

Measure capture/delivery delays, failed jobs, permission-check requests, record
count, query time and rebuild time. Confirm that one company file edit does not
write to every employee's recents. Document unsupported write paths before rollout.

## Alternatives considered

- Asking owners for their most recently modified files includes colleagues'
  edits and does not give us a personal list.
- Copying every accessible file to the user's stack stores too much data.
- Filtering by the file's latest editor loses my earlier edit after someone
  else saves it.
- Recording activity only in the browser misses other devices and editors.

## Consequences

- Recents becomes a personal list of edits and direct shares, including folders.
- Large shared folders do not require copying their contents into recents.
- Recents and search use the same resource IDs and permission rules, but store
  different data.
- We need reliable editor identity, crash recovery and server-to-server delivery.
  Existing `updated_at` values cannot provide personal edit history.

## Open questions before acceptance

- Which actions beyond creation/content saves count as edits? How do we identify
  the user on every write path and save activity without losing it on a crash?
- How do stable instance/user IDs, old share dates, server clock differences
  and remote capability checks work?
- How much history do we keep, how quickly must updates arrive, and what do
  users see offline or when older history is unavailable?

## References

- ADR 008: server-side file search,
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
