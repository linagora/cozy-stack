# AI-assisted PR review

The [AI review workflow](../workflows/ai-review.yml) uses Warden v0.53.0 with
DeepSeek through LINAGORA AI Gateway. The prebuilt action is pinned to a commit.
There is no Python setup, package installation or custom model runner.

The workflow posts Warden's native inline comments and Checks, and saves the JSON
report as a GitHub Actions artifact. Humans decide what to fix and whether to
approve or merge. The bot does not request changes or block the PR on findings.

Warden first analyzes the PR. A check stops publication if a direction is missing
or the report contains analysis, chunk or extraction failures. Warden's `report`
mode then publishes the results and resolves fixed threads; GitHub write failures
fail the job. The JSON report is saved even when this check or publication fails.

## Run and read the review

The repository Actions secret `LINAGORA_AI_GATEWAY_KEY` must allow
`deepseek-v4-pro` at `https://ai-api.linagora.com/v1`.

Add the `ai-review` label to opt a PR in. Reviews run only on labeled, non-draft
PRs from branches in the same public repository. Fork PRs are excluded, and the
person triggering the run must have write access.

Adding `ai-review` starts a run. While the label remains, opening, reopening,
marking the PR ready or pushing new commits starts a full review. A new review
cancels the previous one for that PR. Other label changes do not start or cancel
reviews. Removing `ai-review` stops future automatic runs; cancel any running
review separately if needed. To repeat a run for the same revision, use
**Re-run jobs** on its Actions page while the label remains.

Existing PR branches need the workflow and `.github/review/` files. After this
setup merges, update older branches from `master` before adding the label; the
workflow checks out the PR's head revision.

Read the inline threads in the PR's **Files changed** tab and the per-direction
reports in **Checks**. Warden handles comment formatting and deduplication.
Findings without a suitable diff location remain in Checks.

Only findings with medium or high confidence are posted, to reduce speculative
comments. Confidence is a filter, not proof: humans still check the evidence and
decide what to fix.

After Warden posts, a small `actions/github-script` step adds a visible severity
line to its inline comments: 🔴 High severity, 🟠 Medium severity or 🔵 Low
severity. It uses Warden's final metadata, so the label matches the reported
severity. The title, evidence and tracking markers stay intact. Warden v0.53.0
has no setting for inline severity labels; recheck this step when upgrading it.

For the raw results, open **Actions → AI review (Warden)** and download the
`warden-review` artifact. It includes findings, locations, skill results and usage.
Check errors and failed hunks as well as findings; a green job or zero findings
does not prove the PR is safe. The artifact expires
after three days. `/review` is not enabled.

## Prompts and models

The workflow builds five separate skills, each containing [common.md](common.md)
and exactly one direction prompt:

| Prompt | Question |
| --- | --- |
| [security.md](security.md) | Does this PR create or expose a security hole? |
| [correctness.md](correctness.md) | Can it break behavior, data or compatibility? |
| [simplicity.md](simplicity.md) | Can the same behavior use simpler code? |
| [alternatives.md](alternatives.md) | Is there a better approach worth using here? |
| [tests-performance.md](tests-performance.md) | Are useful tests missing? Can this get slow? |

Edit these source prompts, not the generated files in `.warden/skills/`.
Checks use the direction names, such as `warden: security`. Warden can read and
search related files in the checkout. Review tools cannot run commands or edit
files. The workflow does not build the project or run its tests.

[warden.toml](warden.toml) selects the review, verification and consolidation
models. All use DeepSeek initially. The `deepseek/` prefix selects the adapter;
the request sends `deepseek-v4-pro` to LINAGORA, without that prefix.

Warden splits the diff into chunks and runs each skill separately. Five directions
can mean many model calls. We allow three concurrent analyses and use Warden's
default limit of 50 agent turns per chunk. Analysis has a 110-minute deadline;
the job has 120 minutes, leaving time to validate and upload results after a
timeout. [models.json](models.json) caps agent output
at 65,536 tokens. Helper calls have their own smaller limits; their reasoning is
disabled to leave room for the JSON answer. Gateway limits still apply.
These are ceilings, not a fixed number of calls. Longer investigations can cost
more when the model uses the additional time or turns.

If analysis times out, the job fails and does not publish comments. The artifact
keeps Warden's latest partial report when one exists. It is incomplete review
data, not a clean result. A cancelled job can still stop before artifact upload.

The output limit is per call, not per PR. Reasoning and the final answer share
that budget. No custom input-token limit is configured here. The model context,
Warden's chunking and scan limits, and the gateway still bound what can be read.
A token or turn limit can stop useful investigation; a larger limit only helps
when that limit was the reason the review stopped. Verification currently shares
the helper configuration, including reasoning being off.

The direction prompts and shared rules are local. Warden's output, verification
and consolidation instructions remain in the pinned dependency. Remote skill
loading and model-catalog refresh are disabled. See [sources](SOURCES.md).

## Follow-up reviews and limits

Every push to an eligible PR starts another review. Warden can update, deduplicate
and automatically resolve its own threads when it judges an issue fixed or no
longer present.
This release has no separate switch to disable automatic thread resolution.
Check important fixes yourself; resolving a thread is not approval to merge.

GitHub requires `contents: write` as well as PR write access to resolve threads.
The review job grants that broader permission for this purpose; see
[GitHub's explanation](https://github.com/github/gh-aw/issues/35726).

Treat the comments as review candidates:

- A malformed verifier response can leave an unverified candidate in the results.
- Verification can still retain false positives.
- The failure check covers errors Warden exports. Its report omits some skipped-file
  details, so a green job still does not guarantee complete coverage.
- We still need to measure quality on application changes, not only this workflow.

This setup trusts repository writers and the same-repository branches they
review. Before allowing fork PRs or other untrusted branches, load review
configuration and executable workflow code from a trusted revision. Do not
switch to `pull_request_target` while checking out and executing PR code.

The gateway policy inspected for this setup marks DeepSeek V4 Pro as public-data
only, with worldwide routing. Recheck the [gateway model policy](https://github.com/linagora/ai-gateway/blob/main/infra/litellm/liste-blanche-openrouter.yaml)
when configuring the key. This workflow accepts public repositories only.
