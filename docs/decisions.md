# Decisions

Decisions taken on cozy-stack, oldest first. Append only: a decision that changes gets a new entry that replaces the old one and cites it. Entry format: the `twake-decisions` skill of [linagora/twake-guidelines](https://github.com/linagora/twake-guidelines/blob/main/skills/twake-decisions/SKILL.md).

Categories: UX, UI, Product, Architecture, Infra, Tech.

## 2026-11-24, [Tech] Shared roots are found through the referenced-by view

- Decision: the access resolver finds the shared roots above a document by reading the `referenced-by` view for `io.cozy.sharings` and keeping the directories at the ancestor paths, not with a Mango `$in` on `dir-by-path`.
- Decided by: agent ([#5382](https://github.com/linagora/cozy-stack/issues/5382)), to be confirmed by the cozy-stack maintainer who reviews the PR.
- Why: CouchDB cannot use `$in` as an index range, so the Mango query read every directory on each move and access check ([#5380](https://github.com/linagora/cozy-stack/issues/5380)).
- Scope: `model/sharing` (`AccessResolver`), used by file moves, shared drives, opening files, drive activity and recents.
