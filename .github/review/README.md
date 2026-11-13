# PR review bot

The [AI review workflow](../workflows/ai-review.yml) uses PR-Agent's prebuilt Docker
image with DeepSeek through LINAGORA AI Gateway. The image is pinned by digest.
There is no Python setup, package installation or custom model runner in this repo.

All review instructions are local: [common.md](common.md), one direction prompt,
[report.md](report.md) for the output format, and [context.md](context.md) for PR
context. They replace PR-Agent's default review prompts. Installed skills and
repository-provided PR-Agent settings are disabled.

The bot posts comments. Humans decide what to fix, resolve threads and merge.
[Sources and license links](SOURCES.md) are kept here too.

## Setup

1. Add the repository Actions secret `LINAGORA_AI_GATEWAY_KEY`.
2. Confirm that the key allows `deepseek-v4-pro` and the request limits below.
   To choose another gateway alias, set the repository Actions variable
   `AI_REVIEW_MODEL`, for example `openai/deepseek-v4-pro`.
3. For a trial before merging, follow the steps below. After the trial, remove
   its temporary trigger and merge these files into the default branch. Normal
   runs load that branch's code and prompts, even when reviewing changes to them.
4. Open a non-draft PR, or select **Actions → AI review → Run workflow** and enter
   a PR number. Check the first run's comments, gateway usage and timing.

The endpoint is fixed to `https://ai-api.linagora.com/v1`. The `openai/` prefix
selects the API protocol; requests still go through LINAGORA. There is no fallback
to another model. This setup accepts public repositories and public PR branches
only. Keep credentials and private data out of PRs.

The gateway policy inspected on 2026-11-12 marks DeepSeek aliases N1 Public, with
worldwide routing for V4 Pro. Recheck the [gateway model policy](https://github.com/linagora/ai-gateway/blob/main/infra/litellm/liste-blanche-openrouter.yaml)
when configuring the key.

## Trial on this PR

The temporary `pull_request` trigger runs only for the `chore/review-prompts`
branch in the same repository. It skips forks and drafts, and the person triggering
the run must have write access.

1. Configure the gateway secret as described above.
2. Push `chore/review-prompts` to the target repository and open a non-draft PR.
   If the PR is already open, push a commit or mark it ready for review.
3. Check **Actions → AI review** and the comments on that PR. Each new push runs
   the five reviews and verification using that commit's helper and prompts.

This makes real gateway calls and posts real comments. `/review` and manual
dispatch still use default-branch files; use PR events for this first trial.

Before merging, remove the temporary `pull_request` event, its `prepare` job
condition, the `trial-` concurrency branch, and the PR checkout exception. Restore
the initial checkout to `github.event.repository.default_branch` and remove the
trial-only revision check in `workflow.cjs`. The normal triggers stay in place.

## Review flow

A new PR or commit starts five independent calls, with at most three running at
once. Each gets the shared rules and only its own direction prompt:

| Prompt | Question |
| --- | --- |
| [security.md](security.md) | Does this PR create or expose a security hole? |
| [correctness.md](correctness.md) | Can it break behavior, data or compatibility? |
| [simplicity.md](simplicity.md) | Can the same behavior use simpler code? |
| [alternatives.md](alternatives.md) | Is there a better approach worth using here? |
| [tests-performance.md](tests-performance.md) | Are useful tests missing? Can this get slow? |

A sixth call uses [verify.md](verify.md) to check candidates and merge duplicates.
Only this call publishes. PR-Agent formats the summary and inline comments; no
seventh model call is needed. All six calls initially use the same DeepSeek model.

The small [workflow helper](workflow.cjs) authorizes manual requests, checks PR
revisions, loads prompts and joins result files. PR-Agent handles model requests,
diff preparation, GitHub formatting and finding history.

## After fixes

Every push reviews the full current PR diff again. Draft and closed PRs are
skipped. A person with write access can request another run with exactly `/review`.
The Actions page also supports manual runs. The bot does not wait for CI or run tests.

PR-Agent updates its existing summary and uses its finding history and inline
comment fingerprints to reduce repeats. The verification prompt asks it to check
previous issues and respect human-dismissed threads. Humans still resolve GitHub
threads and decide whether to approve or merge; the bot never approves a PR.

PR-Agent can mark an absent finding as resolved in its stored review history after
a complete run. This is a model-assisted assessment, not proof of a fix, and does
not resolve the GitHub thread. Check important fixes yourself.

## Limits and failures

The prebuilt reviewer sees diff hunks and nearby lines. It cannot search the rest
of the repository on request. Missing callers, tests or architecture context can
make a review partial. Binary and filtered files still need human review.

Each request has a 32,000-token context budget, an 8,192-token output limit and a
300-second timeout, with one retry for eligible provider errors. There are six
review passes before retries. Large-PR chunking is disabled. Verification candidates
are capped at 200,000 characters and must also fit the model's context budget.

If any initial pass fails, reports a partial review, or omits/clips files, the
verification job does not run. Check that pass's Actions artifact for details.
Missing output is an error, not an empty successful review. Artifacts last three days.
A partial final review may be posted by PR-Agent, but the workflow then fails its
completion check. Read the limitations in the comment as well as the check status.

The first five jobs have read-only GitHub access. The final PR-Agent call needs
both the gateway key and comment-write permissions to verify and publish. Normal
runs load default-branch review files. The temporary trial loads the helper and
prompts from its exact PR commit. Neither mode builds or runs application code.

The helper checks the PR head and target base before and after each call. New
pushes cancel older automatic runs, and final jobs are serialized per PR.
PR-Agent publishes inside the final call, so a push during that call can still
leave an outdated comment before the post-call check fails. Treat comments from
failed or superseded runs as outdated and wait for the latest review.

## Changing and testing

Change prompts and workflow code through a normal PR. Keep instructions local;
do not auto-fetch upstream prompts. Update the image digest deliberately when
upgrading PR-Agent, and check prompt/schema compatibility.

The temporary trial above allows this first PR to run before the workflow exists
on the default branch. Normal `pull_request_target` and manual runs use the default
branch in this setup. See GitHub's [event rules](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#pull_request_target)
and [manual-run requirements](https://docs.github.com/en/actions/how-tos/manage-workflow-runs/manually-run-a-workflow).

The pinned container was also checked locally with mocked model responses and
networking disabled: six separate prompts, candidate handoff, JSON output and
Markdown formatting. This does not test gateway authentication, real GitHub
publication or review quality. Try a small PR before relying on the bot.
