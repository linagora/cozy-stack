# Tests and performance

Apply the shared rules. Review direction: tests-performance.

## Tests

Which important behavior changed, and would an assertion catch it breaking?
Focus on data, permissions, retries, concurrency and API compatibility. Check
failure cases, fixture isolation, cleanup, shared services and timing assumptions.

For a missing test, name the case, expected result and existing suite where it
belongs. Reuse `tests/testutils` where suitable. Follow existing patterns; do not
demand table tests, parallelism, build tags or a coverage target by default.

`make unit-tests` uses `-short`, so distinguish it from integration/system tests.
`go build -race` does not run code or detect races by itself.

## Performance

Identify the code path and workload that make the concern matter. Check repeated
queries, unbounded results/buffers, file copying, lock contention, fan-out and
retries. Explain how work grows with input or cite supplied measurements.

Tests of a load tool are not measurements of cozy-stack. If evidence is missing,
suggest a focused benchmark and state the uncertainty. Skip tiny optimizations
without a clear benefit.
