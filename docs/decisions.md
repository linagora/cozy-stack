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
