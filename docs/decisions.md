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

## 2026-11-27, [Tech] Search purges a deleted instance after its databases, without retry

- Decision: `Destroy` removes the search documents of an instance after its CouchDB databases are deleted, and a failure is only logged: nothing retries it.
- Decided by: Anton Shepilov (author), [review comment](https://github.com/linagora/cozy-stack/pull/5266#discussion_r4717666580)
- Why: an index job reads its checkpoint and the changes from CouchDB, so none can write after the purge; and a deletion that fails before the databases keeps the instance with its documents. A failed purge leaves documents that an instance created again with the domain finds, until #5398.
- Scope: `model/instance/lifecycle`, `model/search/purge`.

## 2026-11-27, [Tech] Search purges in background OpenSearch tasks

- Decision: the purge starts one throttled `_delete_by_query` task per index (1000 documents per second), and does not wait for it.
- Decided by: Anton Shepilov (author), [review comment](https://github.com/linagora/cozy-stack/pull/5266#discussion_r4717669927)
- Why: a synchronous delete grows with the instance and fails past the 10 s timeout of the client, and it ran unthrottled on indices shared by all the instances; the purge is best effort, so not seeing the failures of the tasks loses little.
- Scope: `model/search/purge`, `pkg/opensearch`.
