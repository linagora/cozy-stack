# AI-assisted PR review prompts

These prompts drive local reviews with the
[`cozy-review`](../../.agents/skills/cozy-review/SKILL.md) and
[`cozy-review-quick`](../../.agents/skills/cozy-review-quick/SKILL.md) skills.
Each review pass combines [common.md](common.md) with exactly one direction
prompt:

| Prompt | Question |
| --- | --- |
| [security.md](security.md) | Does this PR create or expose a security hole? |
| [correctness.md](correctness.md) | Can it break behavior, data or compatibility? |
| [simplicity.md](simplicity.md) | Can the same behavior use simpler code? |
| [alternatives.md](alternatives.md) | Is there a better approach worth using here? |
| [tests-performance.md](tests-performance.md) | Are useful tests missing? Can this get slow? |

The skills replace the Warden output schema mentioned in `common.md` with their
own report format.

Treat the findings as review candidates. Humans check the evidence and decide
what to fix and whether to approve or merge. See [sources](SOURCES.md).
