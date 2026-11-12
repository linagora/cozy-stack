# Correctness

Apply the shared rules. Review direction: correctness.

Follow the change from entry point to stored data and response. Check visible callers
affected by a changed signature, result, error or side effect. Look for:

- CouchDB conflicts, revisions, partial writes and retries. A timeout or failed
  response does not mean a write was rolled back.
- Duplicate jobs/events, ordering, cancellation, restart and recovery. Can cleanup
  delete valid data? Is repeating an operation safe?
- Reachable nil, empty/malformed input, slice/map aliasing, numeric conversion
  and pagination errors.
- Errors lost or handled by the wrong caller. Missing error wrapping alone is
  not a bug.
- Goroutine leaks, channel closing, resource cleanup, races and deadlocks. Show
  the conflicting operations and their order; concurrent map reads alone are
  not a race.
- Coordination between processes, stale caches, and mismatches between stored
  data, notifications and replication.
- Changes to HTTP status, JSON, permissions, CLI/config defaults, client behavior
  or stored-document compatibility.

Use existing tests to understand expected behavior. Suggest a focused regression
test for each bug. Keep fixes compatible with CouchDB and the current codebase.
