# Decisions

Decisions taken on cozy-stack, oldest first. Append only: a decision that changes gets a new entry that replaces the old one and cites it. Entry format: the `twake-decisions` skill of [linagora/twake-guidelines](https://github.com/linagora/twake-guidelines/blob/main/skills/twake-decisions/SKILL.md).

Categories: UX, UI, Product, Architecture, Infra, Tech.

## 2026-11-23, [Architecture] An action without a result goes to the LLM as a tool call

- Decision: in the history sent to the LLM, an action without a result whose tool the query declares is its tool call, answered by a `tool` message of the stack with the outcome the client wrote; an action of a tool the query does not declare stays described as `(Proposed to the user: name {params})`.
- Decided by: Paul Tran-Van, in a session with an agent (Claude Code), 2026-11-23
- Why: described as text, the LLM wrote its next calls as that text instead of calling the tool, and the apps had to read that text as a call (twake-assistant #182).
- Scope: the chat of the AI assistant, every client; twake-assistant can drop its workaround of #182 once this is deployed.
