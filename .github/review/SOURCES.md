# Sources and licenses

Prepared on 2026-11-12. These prompts adapt selected ideas from the sources below.
All instructions needed for a review are in this directory; no upstream skills,
plugins or prompts are loaded at runtime.

| Source and author | Version read | What we used | License |
| --- | --- | --- | --- |
| [Ponytail review](https://github.com/DietrichGebert/ponytail/blob/main/skills/ponytail-review/SKILL.md), Dietrich Gebert | Plugin 5.3.3 | Trace callers, require evidence, simplify code, write clear findings | [MIT](https://github.com/DietrichGebert/ponytail/blob/main/LICENSE) |
| [Go skills](https://github.com/samber/cc-skills-golang), Samuel Berthe | Security 1.2.2; safety 1.3.2; concurrency 1.2.1; error handling 1.3.1; testing 1.4.0 | Go checks in common, security, correctness and tests/performance | [MIT](https://github.com/samber/cc-skills-golang/blob/main/LICENSE) |
| [Code review](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/code-review/commands/code-review.md), Anthropic | Plugin 1.0.1 | Separate review passes, verification and inline reporting | [Apache-2.0](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/code-review/LICENSE) |
| [Code simplifier](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/pr-review-toolkit/agents/code-simplifier.md) and [test analyzer](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/pr-review-toolkit/agents/pr-test-analyzer.md), Anthropic | Plugin 1.0.1 | Simplification without behavior changes; useful test gaps | [Apache-2.0](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/pr-review-toolkit/LICENSE) |

The gateway setup and cozy-stack checks are local additions. The output fields
follow PR-Agent’s review schema; summary, status and limitations are local additions.
License texts are linked above rather than bundled. These links do not replace
the repository license or settle upstream redistribution requirements.

## What we changed

- Removed Claude-specific tools, model choices and automatic edits or GitHub writes.
- Replaced confidence scores and automatic approval with code evidence and human
  decisions. Kept missing context and failed passes visible.
- Included bugs in shared code and bugs that need specific inputs. A bot author
  or an earlier review is not a reason to skip a PR.
- Made design advice optional. Removed style rules, automatic hardening requests,
  mandatory error wrapping and JavaScript-specific assumptions.
- Followed cozy-stack's existing tests and dependencies.

## Runtime dependency

The workflow uses [PR-Agent's prebuilt GitHub Action image](https://hub.docker.com/r/pragent/pr-agent),
pinned to `sha256:548e894a88071180832b2f4233f734d0acd0db1a879e2fcbaa70bac8b8372ebe`.
The CLI and reviewer files were checked against [v0.50.0 source](https://github.com/The-PR-Agent/pr-agent/tree/e673a02076ce09a4726c3d0c3cc85780e59ca52b).
See its [MIT license](https://github.com/The-PR-Agent/pr-agent/blob/e673a02076ce09a4726c3d0c3cc85780e59ca52b/LICENSE).

We use its review command, diff preparation, output schema, comment formatting and
finding history. The system and user review templates are replaced by local files;
no default model review instructions or external skills are used. The context
fields and finding-history instructions follow PR-Agent's template conventions.
