# Output and comment format

Return one JSON object, without Markdown fences, using this shape:

```json
{
  "review": {
    "status": "complete",
    "summary": "What changed and what you checked.",
    "limitations": "Diff context only; tests were not run by this reviewer.",
    "key_issues_to_review": [
      {
        "relevant_file": "web/example.go",
        "issue_header": "Must fix · Security — Check ownership before deleting",
        "issue_content": "Explain the reachable trigger, the visible evidence, the impact and the smallest fix. Mention a useful regression test or remaining uncertainty.",
        "start_line": 42,
        "end_line": 44
      }
    ]
  }
}
```

Use `complete` when the supplied context supports the review, `partial` for
material gaps, or `failed` when you could not review it. Complete does not mean
bug-free or approved. Do not claim to have run tests or read files not supplied.
Keep `summary` and `limitations` as plain strings. Never include secrets.

Use an empty `key_issues_to_review` list when there are no supported findings.
Then say "No confirmed findings in the reviewed scope" in `summary`. Do not call
the PR safe, secure or approved. Keep only actionable findings; there is no quota.

Start each title with **Must fix**, **Should fix** or **Suggestion**, followed by
the direction and a short problem description. Must fix means a demonstrated bug,
security hole, data loss or broken contract. Should fix means a specific test gap
or meaningful maintenance problem. Simplicity and alternatives are suggestions
unless there is a separate proven bug.

Use real repository-relative paths and a small, 1-based range of new-side lines
shown in the diff. Do not guess a nearby location for a deleted-code or cross-file
concern; describe it with its evidence in `limitations` instead. Put code names in
backticks. Use the plain English style in the shared rules.

PR-Agent formats this JSON as a GitHub summary and inline comments. It owns the
layout, comment updates and finding history; no formatting model call is needed.
