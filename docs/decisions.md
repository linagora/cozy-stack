# Decisions

Decisions taken on cozy-stack, oldest first. Append only: a decision that changes gets a new entry that replaces the old one and cites it. Entry format: the `twake-decisions` skill of [linagora/twake-guidelines](https://github.com/linagora/twake-guidelines/blob/main/skills/twake-decisions/SKILL.md).

Categories: UX, UI, Product, Architecture, Infra, Tech.

## 2026-11-24, [Tech] Shared roots are found through the referenced-by view

- Decision: the access resolver finds the shared roots above a document by reading the `referenced-by` view for `io.cozy.sharings` and keeping the directories at the ancestor paths, not with a Mango `$in` on `dir-by-path`.
- Decided by: agent ([#5382](https://github.com/linagora/cozy-stack/issues/5382)), to be confirmed by the cozy-stack maintainer who reviews the PR.
- Why: CouchDB cannot use `$in` as an index range, so the Mango query read every directory on each move and access check ([#5380](https://github.com/linagora/cozy-stack/issues/5380)).
- Scope: `model/sharing` (`AccessResolver`), used by file moves, shared drives, opening files, drive activity and recents.

## 2026-11-25, [Tech] Search keys join the domain and the doc id with a colon

- Decision: `Key` is `<domain>:<doc id>`, as in ADR 008, although a domain with a port and a doc id with a colon could give the same key.
- Decided by: Anton Shepilov (author), [review comment](https://github.com/linagora/cozy-stack/pull/5259#discussion_r4682719679)
- Why: a domain only has a colon with a port, which happens in dev only; a collision needs two instances of one stack that differ only by a port, plus a doc id with a colon chosen by a sharing peer.
- Scope: `model/search`, the `files_v1` and `share_scopes_v1` indices.

## 2026-11-25, [Architecture] Search scopes list the member addresses until #5395

- Decision: `active_member_principals` holds the lowercased hosts of the members who accepted a drive; the search uses them as is until it also checks that the caller holds the sharing (#5395).
- Decided by: Anton Shepilov (author), [review comment](https://github.com/linagora/cozy-stack/pull/5260#discussion_r4681654061)
- Why: the check reads the caller's sharings, so it belongs in the search query, which is a later change of the stack.
- Scope: `model/search`, the `share_scopes_v1` index.

## 2026-11-25, [Product] Search leaves out the trash

- Decision: the search does not return trashed files and folders, as ADR 008 says.
- Decided by: Anton Shepilov (author), [review comment](https://github.com/linagora/cozy-stack/pull/5261#discussion_r4695299650)
- Why: Drive shows search results without a trashed state, and the trash has its own view; showing trashed hits needs that display first.
- Scope: `model/search`, `POST /files/search`.

## 2026-11-25, [Product] Search drops the hits of an owner missing from the stack cluster

- Decision: `VisiblePaths` drops a drive hit whose owner is not in the global CouchDB, instead of returning it with an empty path as a drive owned outside of the stack cluster.
- Decided by: Anton Shepilov (author), [review comment](https://github.com/linagora/cozy-stack/pull/5266#discussion_r4631076331)
- Why: the indices only hold docs written by the instances of this stack cluster, so a missing owner is a deleted instance whose purge failed; its former members must not keep finding its file names.
- Scope: `model/search`, `POST /files/search`; drives of other stack clusters wait for the federated search.

## 2026-11-23, [Architecture] The Stack keeps the SSO uuid of the owner

- Decision: at each OIDC login and app token exchange that the SSO verified for the owner, the Stack keeps the `uuid` claim (the LDAP entryUUID) on the instance as `oidc_uuid`, and names the owner by it in the events it publishes on `activity`.
- Decided by: agent (session of Théo Poizat, 2026-11-23), to be confirmed by a cozy-stack maintainer.
- Why: Twake apps name people by uuid in their events (ADR 067 of linagora/twake-adr), and the webhook gateway only delivers by uuid (ADR 075); `oidc_id` holds the `sub`, which differs.
- Scope: OIDC login, `/auth/token_exchange` with `exchange_type` `app`; admin exchanges are left out, as their token may belong to another member.

## 2026-11-23, [Architecture] One event per file created in a personal drive, off by default

- Decision: with the context setting `drive_activity.file_events`, the Stack publishes `com.twake.drive.file.created.v1` for each file uploaded through `POST /files/:dir-id` in a personal drive, to its owner by uuid; shared drives keep the digest of ADR 010. The events of the personal drive live in `model/driveevent`, apart from the digest of `model/driveactivity`, so that packages the sharings depend on, such as `model/office`, can publish them too.
- Decided by: agent (session of Théo Poizat, 2026-11-23), to be confirmed by a cozy-stack maintainer.
- Why: the n8n trigger of Twake Drive needs an event per file, while ADR 010 keeps the volume of a sync out of the bus by default.
- Scope: the personal drive of an instance, uploads through `/files` only (not copies, notes, office files or sharing replication).
