# Sources and licenses

Updated on 2026-11-15. These local prompts adapt selected ideas from the sources
below. No remote skills are fetched.

| Source and author | Version read | What we used | License |
| --- | --- | --- | --- |
| [Ponytail review](https://github.com/DietrichGebert/ponytail/blob/main/skills/ponytail-review/SKILL.md), Dietrich Gebert | Plugin 5.3.3 | Trace callers, require evidence, simplify code, write clear findings | [MIT](https://github.com/DietrichGebert/ponytail/blob/main/LICENSE) |
| [Go skills](https://github.com/samber/cc-skills-golang), Samuel Berthe | Security 1.2.2; safety 1.3.2; concurrency 1.2.1; error handling 1.3.1; testing 1.4.0 | Go checks in common, security, correctness and tests/performance | [MIT](https://github.com/samber/cc-skills-golang/blob/main/LICENSE) |
| [Code review](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/code-review/commands/code-review.md), Anthropic | Plugin 1.0.1 | Separate review passes, verification and inline reporting | [Apache-2.0](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/code-review/LICENSE) |
| [Code simplifier](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/pr-review-toolkit/agents/code-simplifier.md) and [test analyzer](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/pr-review-toolkit/agents/pr-test-analyzer.md), Anthropic | Plugin 1.0.1 | Simplification without behavior changes; useful test gaps | [Apache-2.0](https://github.com/anthropics/claude-plugins-official/blob/main/plugins/pr-review-toolkit/LICENSE) |

The cozy-stack checks are local additions.
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
