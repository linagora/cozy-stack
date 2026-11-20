# ADR 011: Batched background updates

## Status

Proposed

## Date

2026-11-02

## Context

Some changes start background work that sends the latest state somewhere. When many changes arrive together, the stack does that work once per change, even though only the last run matters.

The sharing member list is the main example today:

- After a change to the members of a sharing (accept, add, revoke, read-only, group change), the owner sends the full member list to every recipient with `NotifyRecipients`. 13 places in the code call it.
- When a group of N members auto-accepts, each accept sends the list to every recipient: up to N × N requests, each one carrying all N members.
- 12 of the 13 calls start a goroutine with a copy of the sharing. It waits 3 seconds, then sends that copy, one request per recipient, one after the other. The copy can already be outdated, and a restart during the wait loses it.
- The last call is in the member setup. It holds the sharing lock while it waits 3 seconds and sends. Other setups wait for the lock, and after 20 seconds they give up without an error, skipping the whole setup of their member. So recipients often keep an outdated member list. The org drive fanout load test sees it: recipient member lists can stay unresolved.

Other features have the same shape. [ADR 010](010-adr-drive-activity-digest.md) counts drive activity and sends it at the end of a time window, with its own Redis keys and an `@in` trigger.

The job system can already batch, but only for triggers stored in CouchDB. A debounced `@event` or webhook trigger sets a fire time on its first event, collects the payloads of the next events, and then runs one job with all of them (`model/job/redis_scheduler.go`). Code that has no trigger document cannot use it.

## Decision

- Add a way to push a batched job from code, without a trigger document: `job.PushBatched(inst, request, key, window)`.
- A batch is identified by the instance, the worker type and a key chosen by the caller, for example the sharing ID.
- The first call for a batch starts its window. Calls during the window only add their payload, if they have one. At the end of the window, one job runs with all the payloads.
- The window starts at the first call, not the last one. Work is late by at most one window, even when changes never stop.
- The job reads the current state when it runs. Payloads are hints, such as which members changed, never the data to send. Running the same batch twice must be safe.
- Only one job runs per batch at a time. If a job of the same batch is still running, the new one pushes the batch again and stops. So the last run always reads the latest state.
- First user: the sharing member list. The 13 call sites push a batched `share-members` job with the sharing ID as the key. The 3 second wait becomes the batch window, and the lock around the sending goes away. The job:
  - loads the sharing and sends the list once to each active recipient, a few requests at a time. There is no `except` member anymore: when two members accept in the same window, both need the new list.
  - refreshes tokens one at a time, and reloads the sharing on a conflict.
  - pushes the batch again when a recipient fails, at most 3 times.
- Second user: realtime events for sharings, once [ADR 012](012-adr-sharing-members-storage.md) moves members out of the sharing document.
- Other features use it when they have the same shape: many changes, and one result that only depends on the latest state. Candidates: ADR 010's activity digest (its counts stay in its own store, the batch only replaces the `@in` trigger), and file search indexing ([ADR 008](008-adr-server-side-file-search-v2.md)), with one batch per instance instead of an `@event` trigger on every instance.

### Flow

```mermaid
sequenceDiagram
  participant C as code (e.g. accept)
  participant R as Redis
  participant P as scheduler poll (every second)
  participant J as batched job
  C->>R: add the batch with fire time = now + window (only if absent)
  C->>R: append the payload to the batch list
  Note over C,R: more calls in the window only append payloads
  P->>R: batches whose fire time has passed?
  P->>J: push one job with all the payloads
  P->>R: delete the batch
  J->>J: read the current state and do the work once
```

- Redis holds a sorted set of batches scored by fire time, the job request of the first call, and one list of payloads per batch. It works like debounced triggers, but a batch has no trigger document, so it gets its own sorted set and its own Lua script to pick it up.
- The existing scheduler poll loop runs that script. It takes the payload list in the same step (`RENAME`), so a payload added at that moment goes to the next batch.
- The job is pushed before the batch is deleted, unlike debounced triggers today. If pushing fails, the batch stays and the next poll tries again. If the stack stops in between, the batch is picked again after 10 seconds, as triggers are: the job runs twice, which is safe.
- A batch keeps at most 100 payloads. After that, the job gets a flag to reload everything. Payloads are saved in the job document, so they stay small.
- A call that arrives after the batch is picked up starts a new window. While Redis works, a change is never lost: either the running job reads it, or it runs in the next batch.
- Without Redis (a single stack, dev, tests), the in-memory scheduler does the same with a timer. Open batches are lost on restart, like today's goroutines.
- If Redis fails when adding to a batch, the error is logged. Jobs go through Redis too, so a normal job would fail the same way. The next change sends the list again.
- The stack logs, for each batch, how many payloads it had and how late it ran.

### Configuration

```yaml
jobs:
  workers:
    share-members:
      batch_window: 3s
```

Each batched worker has a default window. `batch_window` sits next to the existing worker settings (`concurrency`, `max_exec_count`, `timeout`) and can change it.

### Volume

- A group of 100 members whose answers arrive over 30 seconds, with a 3 second window: about 10 rounds of 100 requests, instead of up to 10,000.
- A single change: one round, up to 3 seconds later, as today.

## Consequences

- Recipients see member list changes up to 3 seconds late, as today: each run already waits 3 seconds. The difference is that the latest list now arrives, also when a request fails.
- A revoked member is told by the revocation, not by the member list, so this does not delay it. Only the member list sent to the other recipients waits for the window.
- The time spent on member lists grows with the number of members, not with members × changes.
- Redis holds one more sorted set, and one small request and list per open batch.
- Tests that expect the member list to be sent right away must run the batch, or use a zero window.
- The member setup still gives up without an error when it waits more than 20 seconds for the lock. It happens less, as the setup no longer waits and sends while holding the lock. Fixing it is left for another change.

## Open questions

- Should the first change of a window be sent right away, and only the next ones batched? It would remove the delay for single changes. It can be added later without changing the callers.
