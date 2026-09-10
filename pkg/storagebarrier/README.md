# Per-instance storage barrier

This package provides admission and draining for storage maintenance. It does
not yet change HTTP handlers, workers, VFS implementations, or migration commands.
Those integrations must establish the lifecycle below before a migration can
claim a consistent source snapshot.

## Operation lifecycle

Use one `Service` backed by the stack's lock Redis. `New(nil)` is only suitable
for a single-process stack; independent in-memory services do not coordinate.
Every participant must use the same canonical instance key, including its
CouchDB cluster and database prefix, and the same Redis database.

1. Load the instance's persisted storage generation with its backend selection.
2. Call `Enter(ctx, key, generation, nil)` before using that backend to write.
3. Keep the returned permit until both the object write and the CouchDB index
   update have finished. For uploads this includes the writer's `Close`, not
   just `CreateFile` or receiving the request body.
4. Close the permit on all completed/error paths and report release failures.

A request or job can pass its live permit as the parent of nested storage
operations. The last nested operation keeps its parent registration alive even
if the outer request has returned. A closed parent cannot admit new nested work.
Permits are local handles: never serialize or copy their values.

`ErrBusy` means maintenance has stopped admission. HTTP/CLI clients should get a
retryable response; jobs should remain queued or be deferred without exhausting
their normal execution retries. `ErrStale` means the caller must reload its
instance/backend, not retry using the same VFS handle.

All storage mutations need coverage, including background jobs, admin/CLI
requests, sharing writes to another instance, file versions, deletes, avatars,
and direct index updates. Admission around HTTP traffic alone is insufficient.

## Maintenance lifecycle

1. Call `Block` to close admission and elect a single owner for the instance.
2. Call `Wait` to drain already-admitted operations. Their nested work can
   finish, while unrelated instances and concurrent ordinary writers remain
   independent.
3. Copy and verify using maintenance-owned storage handles, without entering
   the barrier again. Calling ordinary admission here would reject the owner.
4. Call `Check` before destructive steps or cutover. On any coordination error,
   stop; do not switch backends or purge data.
5. Persist the new backend and a monotonically increasing storage generation
   together, then call `Open` with that generation to resume admission.

`Open` checks ownership and requires zero outstanding operations atomically.
An earlier generation is rejected, including a VFS handle from before a
round-trip migration back to the original backend. `Cancel` is only for aborting
before backend/content changes; it reopens admission without changing generation
and can be called while operations are still draining.

A failed or ambiguous backend update must leave admission closed. The package
does not make Redis and CouchDB one transaction and does not fence direct writes
at either storage server. Its safety depends on every writer participating and
the coordinator stopping on errors. Checks alone cannot protect against an
operator concurrently resetting coordination state.

## Failure and recovery

Permits and owner registrations have **no TTL**. Expiring a registration could
mistake a paused writer for a completed upload. A crashed writer, owner, or an
ambiguous Redis reply can therefore require manual recovery instead of automatic
availability. Cancelling `Wait` does not release other processes' permits.

Redis must retain these keys: use durable storage and a non-evicting policy. Do
not flush the lock database, delete barrier keys, restore stale snapshots, or
switch Redis endpoints while stack processes are operating. Losing registrations
can hide active writers; generation checks are not a substitute for durability.
The in-memory fallback must not be used across multiple stack processes.

For recovery, first stop and confirm termination of every process that can write
to the affected instance, including external jobs. Inspect the authoritative
backend/generation and resolve any uncertain cutover before repairing only the
affected `storage-barrier:<canonical-instance-key>` hash. Preserve/reset its
`generation` field to the authoritative value and remove stale owner/operation
fields only after proving their holders cannot resume. Restart writers using
fresh instance state. Never clear registrations merely because they are old.

Roll out admission tracking to **all** stack/worker processes and drain older
versions before enabling any maintenance caller. The barrier API alone does not
fix the migration cutover race.

## Tests

```sh
go test -race -short ./pkg/storagebarrier
COZY_TEST_REDIS_URL=redis://localhost:6379/0 go test -race ./pkg/storagebarrier
```

The Redis tests use two independent clients, create uniquely named test keys,
and remove only those keys. They cover admission/cutover races, nested uploads,
cancelled draining, stale generations, lost ownership, and Redis errors.
