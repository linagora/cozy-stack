# Sharing members storage: implementation notes

Companion to [ADR 012](012-adr-sharing-members-storage.md). The ADR says what we do and why. This page says how.

## Documents

### Sharing document (`io.cozy.sharings`)

Same document as today, without `members` and `credentials`, and with a marker for the new format:

```json
{
  "_id": "a1f0c3e2d4b5",
  "_rev": "12-...",
  "roster": true,
  "drive": true,
  "active": true,
  "owner": true,
  "description": "Q4 assets",
  "rules": [{ "title": "Q4 assets", "doctype": "io.cozy.files", "values": ["8ff66e0ee58e"] }],
  "groups": [{ "id": "c0ffee", "name": "Marketing", "addedBy": 0, "read_only": false }],
  "triggers": {},
  "created_at": "2026-11-02T09:00:00Z",
  "updated_at": "2026-11-02T09:15:30Z"
}
```

The sharing document is written from its own Go type with no `members` or `credentials` field. A `nil` slice on `Sharing` would still write `"members": null`, because that field has no `omitempty`.

### Member document (`io.cozy.sharings.roster`)

```json
{
  "_id": "a1f0c3e2d4b5/3",
  "_rev": "2-...",
  "sharing_id": "a1f0c3e2d4b5",
  "index": 3,
  "member": {
    "status": "ready",
    "public_name": "Bob",
    "email": "bob@example.com",
    "instance": "https://bob.example.com",
    "groups": [0],
    "only_in_groups": true
  },
  "credentials": {
    "client": { "client_id": "...", "client_secret": "..." },
    "access_token": { "access_token": "...", "refresh_token": "..." },
    "inbound_client_id": "...",
    "xor_key": "..."
  }
}
```

`credentials` holds the credentials used to talk to this member:

| Instance  | Member 0 (owner)              | Members 1..n                 |
|-----------|-------------------------------|------------------------------|
| Owner     | no credentials                | today's `Credentials[i-1]`   |
| Recipient | today's `Credentials[0]`      | no credentials               |

Loading rebuilds the `Members` and `Credentials` slices exactly as they are today, so the rest of the code does not change.

## Reading

- One sharing: `GET` the sharing document, then all its members in one request:
  `_all_docs?startkey="<id>/"&endkey="<id>/￰"&include_docs=true`.
  Sort by the `index` field: in string order, `10` comes before `2`.
- Many sharings (`FindSharings`, `FindActive`, `ListDrives`, `GetSharingsByDocType`): one Mango query with `sharing_id` `$in` the IDs. Add a `sharing_id` index in `pkg/couchdb/index.go`.
- `model/sharing/reupload.go` (`GetAllDocs`) and `model/sharing/reference.go` (`FindDocsRaw`) also read sharings directly. They must go through the same loader, or check that they don't need members.
- Format check, in this order:
  1. The sharing document has a non-empty `members`: old format. Use it and ignore member documents. An older stack may have rewritten a converted sharing, for example after a rollback, and its version is the latest.
  2. The sharing document has `roster: true`: read the member documents.
  3. Otherwise: no members.

## Saving

- One `func (s *Sharing) Save(inst) error` replaces every direct `couchdb.UpdateDoc` / `CreateDoc` on a sharing in `model/sharing`, `web/sharings` and `model/orgdrive`.
- On load, keep each document's `_rev` and a copy of what was read. On save, compare to find what changed.
- Write the changed member documents in one `_bulk_docs`, then the sharing document if it changed. The sharing document goes last, so a failed save can be retried.
- Conflicts are reported per document. Re-read only the member documents in conflict, re-apply the change with the caller's reconcile function (as `updateSharingWithConflictRetry` does today), and write again.
- A new member gets the next index. A conflict when creating it means another writer took that index: reload and retry.
- `ProcessAnswer` sets `Active = true` on every answer. Since nothing changes after the first one, only the first answer writes the sharing document.
- Bulk writes do not send realtime events, so the stack sends them itself (below). There are no CouchDB hooks on `io.cozy.sharings` today (no `AddHook` for it), so nothing else depends on `UpdateDoc` for sharings.

## Realtime

- After a save, add the sharing to its [ADR 011](011-adr-batched-background-updates.md) batch. The batched job publishes one `updated` event on `io.cozy.sharings` with the whole sharing, built like the API response (`APISharing`), so it has no credentials.
- `created` and `deleted` events are sent right away.
- Drive web only reads `doc.drive` from these events and then reloads through the API (`useSharedDrives.js`), so a short delay is fine. Check other clients that subscribe to `io.cozy.sharings` before release 2.

## Access

- Add `io.cozy.sharings.roster` to the blocklist in `model/permission/doctype.go` as `none` (no read, no write), like `io.cozy.sharings`.
- Never publish realtime events for this doctype, and refuse `SUBSCRIBE` to it in `web/realtime`.

## Deleting a sharing

- Delete the sharing document first, then its member documents.
- If the second step fails, the remaining member documents are not linked to anything, and the migration job removes them. Deleting in the other order could leave a sharing with no members.
- Places: `model/sharing/sharing.go` (the drive sharing deletion) and `model/sharing/revoke_trashed.go`.
- Deleting an instance drops all its databases; nothing to add there.

## Moving an instance

`model/move/importer.go` imports `io.cozy.sharings` only for a move, through `importSharing`. Member documents need the same rule: imported only for a move, and together with their sharing.

## Recipients

- `UpdateRecipients` (`PUT /sharings/:id/recipients`) writes only the member documents that changed, in one request.
- A recipient's own credentials are in member document 0. Incoming member list updates no longer collide with the accept or with token refreshes.

## Release steps

1. **Read both formats, write the old one.** Ship `Save`, the readers and the indexes. Behaviour stays the same.
2. **Write the new format** when `sharing.roster_storage: true` is set in the context config. Turn it on only after every stack runs this release. A sharing converts on its first save: member documents first, then the sharing document with `roster: true` and no `members`.
3. **Migrate the rest** with a job per instance, using the same conversion. It also removes member documents whose sharing no longer exists. Then remove the code that reads the old format.

If a conversion stops between its two writes, the sharing document still has `members`, which win. The next conversion reads the existing member documents' revisions and overwrites them.

## Revocation (follow-up, not part of this ADR)

With per-member documents, the revocation from PR #5009 could:

- mark the members to notify as `revoking` on their member documents;
- push a job with only the sharing ID;
- have the job read credentials from the member documents, mark each member `revoked` once notified, and delete a drive sharing when none are left.

Retries then only contact the members still `revoking`, and jobs carry no credentials.

## Tests

- **Unit:**
  - load and save in both formats;
  - old format wins when both exist;
  - an interrupted conversion recovers;
  - a save writes only the changed documents;
  - realtime events never contain credentials.
- **Concurrency:** many `ProcessAnswer` calls in parallel on one sharing. All members end up ready, with no 409. Run with `-race`.
- **System tests:** run the existing sharing scenarios with the flag off and on.
- **Load:** org drive fanout at 100, 250, 500 and 1,000 members. Record the time to accept everyone, the time for member lists to converge, and CouchDB writes per accept. The results set the new default member limit.

## Code map

| Area                                | Files                                                                  |
|-------------------------------------|------------------------------------------------------------------------|
| New doctype                         | `pkg/consts/doctype.go`                                                |
| Index on `sharing_id`               | `pkg/couchdb/index.go`                                                 |
| Load, save, format check (new file) | `model/sharing/storage.go`                                             |
| Loaders                             | `model/sharing/sharing.go` (`FindSharing`, `FindSharings`, `FindActive`, `ListDrives`, `GetSharingsByDocType`), `model/sharing/reupload.go`, `model/sharing/reference.go` |
| Accept                              | `model/sharing/oauth.go` (`ProcessAnswer`, `SendAnswer`, `updateSharingWithConflictRetry`) |
| Invitations                         | `model/sharing/invitation.go` (`updateMemberStatusesWithRetry`)        |
| Members, tokens                     | `model/sharing/member.go` (`UpdateRecipients`, `Credentials.Refresh`)  |
| Groups, revocation, org drives      | `model/sharing/group.go`, `model/sharing/sharing.go`, `model/orgdrive/members.go` |
| Deletion                            | `model/sharing/sharing.go`, `model/sharing/revoke_trashed.go`          |
| Access                              | `model/permission/doctype.go`, `web/realtime/realtime.go`              |
| Instance move                       | `model/move/importer.go`                                               |
