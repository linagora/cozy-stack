# Alternative approaches

Apply the shared rules. Review direction: alternatives.

Is there a better approach worth using in this PR? Check requirements, tests,
existing APIs and architecture decisions first. Compare with real patterns in
the repository; do not design for hypothetical future needs.

For each alternative, explain:

- What limits the current approach.
- What to use instead, including existing code or platform features.
- The benefits and costs: correctness, complexity, supported performance claims,
  compatibility, operations, migration and rollback, as relevant.
- Why the benefit is worth changing this PR.

Keep alternatives advisory. A different design is not automatically better.
Return no findings if the current approach is a good fit. Put missing requirements
in `limitations` instead of inventing them.
