# ADR: Nested Shared Folders

## Status

Draft

## Summary

Support sharing a folder that is already inside another shared folder without
turning the child folder into a restrictive boundary by default. Nested folder
shares are additive: parent access still applies, and the child share can add
new users or higher rights. A future `limited_access` mode can make a child
folder restrictive, but that is not part of the first implementation.

## Context

Today, shared drives are treated as isolated top-level sharing boundaries. The
backend prevents creating a shared drive from a folder or file that is already
shared, is inside another shared drive, or contains another shared drive. This
keeps permission checks simple because a file belongs to a single effective
shared-drive scope.

The nested shared folders user story changes that model. A parent folder can be
shared with one set of users, and a subfolder can be shared with additional
users. Users who have access through the parent should keep access to the child
folder; users added directly on the child should see only that child folder when
they do not have access to the parent.

This is different from a Google-style limited access folder. Limited access is a
separate feature where parent access stops at the child folder. We want the data
model and permission resolver to leave room for that future mode, but the first
iteration should implement normal nested sharing as additive inheritance.

### Endpoint Impact And Permission Check Complexity

| UI journey | Endpoint called | What to check and why | Complexity |
|---|---|---|---|
| Create a shared folder from a folder inside another shared folder | `POST /sharings/drives` | Check the caller can read/share the selected root and can manage sharing in the parent effective scope, because the child share adds a new direct access path. | **Medium**: current anti-nesting validation must change, and inherited access must be derived. |
| Create a file share inside a shared folder | existing file sharing / sharing creation endpoints | Check the caller can read/share the file in its effective scope. The file share adds access but does not become a boundary. | **Low to Medium**: additive file shares already fit the model, but nested containment checks must not treat them as drive boundaries. |
| Create a new shared folder by name | `POST /sharings/drives` | Check `POST` on the parent folder, because a real VFS folder is created before it is shared. | **Low**: mostly existing folder-create permission. |
| Open parent folder containing shared child folders | `GET /sharings/drives/:id/:file-id` | Check effective `GET` through parent and additive child scopes. Parent users can still open child content unless future `limited_access` blocks inheritance. | **Medium**: listing may need to annotate child shared roots and inherited/direct access. |
| Open child shared folder directly | `GET /sharings/drives/:child-id/:file-id` | Check effective `GET` for the child scope. Direct child recipients can enter through the child share even if they cannot see the parent. | **Medium**: route checks must accept direct child access and inherited parent access where applicable. |
| Rename/move item inside one effective scope | `PATCH /sharings/drives/:id/:file-id` | Check effective write on the item and write/create on the destination parent. | **Medium**: source and destination may have different additive scopes. |
| Move item from parent shared folder into child shared folder | `PATCH /sharings/drives/:parent-id/:file-id` or `POST /sharings/drives/move` | Check write/remove on source and write/create on destination; effective access after the move is parent plus child additive scopes. | **Medium**: destination effective access must be recomputed. |
| Move item from child shared folder back to parent | `PATCH /sharings/drives/:child-id/:file-id` or `POST /sharings/drives/move` | Check write/remove on child source and write/create on parent destination; preserve direct child access only if product requires it. | **Medium**: moving can change inherited access. |
| Move shared folder into another shared folder | `POST /sharings/drives/move` | Compute effective users before the move, then merge with destination inherited users and keep the higher permission. | **High**: must materialize missing direct grants to avoid losing pre-move access. |
| Copy/move folder containing nested shared roots | `POST /sharings/drives/move` | Find nested shared roots under the moved folder and apply merge/preserve rules for each impacted share. | **High**: proportional to nested shared roots and needs clear copy-vs-move semantics. |
| Download file through parent or child shared route | `POST /sharings/drives/:id/downloads`, then `GET /sharings/drives/:id/downloads/:secret/:name` | Check effective `GET` for the requested file and verify the route is a valid access path for that user. | **Medium**: existing broad route checks need effective-scope validation. |
| Download archive from a shared folder | `POST /sharings/drives/:id/archive` | Check effective `GET` for archive entries. For v1 additive sharing, inherited parent access can include child content. | **Medium to High**: archive construction still walks content and must account for nested shared roots. |
| Create share-by-link inside nested shared folder | `POST /sharings/drives/:id/permissions` | Check effective `GET` on target and link-creation rights in the effective scope. | **Medium**: current containment checks must become effective-access checks. |
| List share-by-link permissions | `GET /sharings/drives/:id/permissions?ids=...` | Check each requested ID is visible through effective access. | **Medium**: batch validation needed for all IDs. |
| Edit/remove share-by-link | `PATCH /sharings/drives/:id/permissions/:perm-id` or `DELETE /sharings/drives/:id/permissions/:perm-id` | Check the permission doc belongs to a scope the caller can manage. | **Medium**: direct and inherited managers may differ. |
| Watch live changes in parent shared folder | `GET /sharings/drives/:id/_changes` or `GET /sharings/drives/:id/realtime` | Send events only when the user has effective access to the changed item. | **High**: path-prefix filtering must be replaced or supplemented by effective-access filtering. |
| Show shared folders in sidebar / shared section | `GET /sharings/drives` | Return direct shares and enough metadata to distinguish inherited users from direct recipients. | **Medium**: UI needs both "Shared with me" and "Shared by me" behavior from the user story. |
| Delete/trash shared parent folder containing nested shared folders | `DELETE /sharings/drives/:id/:file-id` or `DELETE /files/:file-id` | Find nested shared roots and decide whether deletion removes them for everyone or requires preserving direct shares. | **High**: destructive operation with revocation and orphaning risk. |
| Parent access removed while child direct access remains | `GET /sharings/drives`, child routes | Recompute effective access so the user loses parent-derived access but keeps direct child access. | **Medium**: requires derived inheritance instead of copied inherited members. |
| Background sync/replication | internal `io.cozy.shared` updates | Track sharing refs according to effective additive scopes and preserve direct child access. | **High**: fan-out, locking, and replication semantics become more complex. |

## Models Considered

- Yandex Disk avoids nested shared folders entirely.
- Google Drive normally uses inherited, additive sharing. It supports
  restrictive child folders only through a special limited access feature.
- SharePoint, OneDrive, and Nextcloud are closer to a full ACL model, where
  folders or files can have inherited and overridden permissions.

For Twake, the first implementation should follow the normal additive behavior:
share downward by inheritance, add access on child folders, and defer
restrictive limited access until the product explicitly needs that mode.

## Decision

Nested folder sharing will be additive by default.

An `io.cozy.sharings` document remains the sharing scope and member list. The
shared root remains marked on `io.cozy.files` with `referenced_by`. For nested
folder shares, effective access is derived from all applicable additive sharing
scopes on the path.

Rules:

- Parent folder access applies to child folders unless a future
  `limited_access` mode explicitly blocks inheritance.
- Child folder sharing can add users or grant higher rights.
- If the same user has access through parent and child scopes, the higher right
  wins.
- File shares are additive access only. They do not create restrictive
  boundaries.
- Limited access is reserved as an explicit access mode for a later iteration.

## Solution

Add an explicit access mode to sharing documents:

```text
io.cozy.sharings
- access_mode: additive | limited_access
```

The default value is `additive` for existing and newly created nested folder
shares. `limited_access` is reserved for the future feature and should not be
enabled by this first implementation.

The permission resolver should distinguish between sharing scopes and
restrictive boundaries:

```text
effectiveAccessScopes(file_or_folder)
  returns additive parent and child sharing scopes that apply to the item

nearestRestrictiveBoundary(file_or_folder)
  reserved for future limited_access mode

childSharedRootsUnder(folder)
  returns nested shared roots impacted by recursive operations
```

For v1, `effectiveAccessScopes` is the main permission check. A user can access
an item when they are a member of any applicable additive scope. The final
permission is the highest permission found across those scopes.

Example:

```text
Folder A shared with Alice as editor
  Folder B shared with Bob as viewer
```

Effective access to `Folder B`:

```text
Alice: editor, inherited from Folder A
Bob: viewer, direct access from Folder B
```

Bob should see `Folder B` in "Shared with me" without seeing `Folder A`. The
owner/editor should see both `Folder A` and `Folder B` in "Shared by me" as
separate shared items.

## Implementation Notes

Creation:

- Allow creating a folder share inside an existing shared folder.
- Reject or keep out of scope any operation that tries to create a restrictive
  nested folder by default.
- Store `access_mode = additive` explicitly or treat missing `access_mode` as
  `additive` for backwards compatibility.

Permission checks:

- Replace single-scope checks with an effective access resolver for nested
  folder content.
- Show inherited users separately from direct users in the sharing dialog when
  the UI needs to explain why a user has access.
- Do not copy inherited parent members into every child sharing during normal
  updates. Derive inherited access from the parent chain.

Move:

- Moving a shared folder into another shared folder should merge permissions.
- Users who had effective access to the moved folder before the move must keep
  access after the move.
- Users who have access to the destination folder gain access through the new
  parent inheritance.
- If the same user exists in both sources with different rights, keep the higher
  right.
- To preserve pre-move access when parent inheritance changes, compute the
  moved folder's effective access before the move and materialize missing users
  as direct grants on the moved folder sharing.

Future limited access:

- `limited_access` will be the mode that turns a child folder into a hard
  boundary.
- When a `limited_access` boundary is encountered, parent scopes above it should
  stop applying.
- Move, delete, archive, download, realtime, and sync flows will need additional
  boundary checks before enabling that mode.

## Consequences

This approach matches the user story for nested shared folders and avoids a full
ACL model. It keeps file shares additive and keeps inherited parent access
derived instead of copied.

The tradeoff is that permission checks become more complex than the current
single shared-drive scope. The backend needs shared helpers for effective access
resolution and for finding nested shared roots impacted by recursive operations.

Limited access remains possible later, but it must be implemented as an
explicit mode with additional checks instead of being the default behavior of
nested sharing.
