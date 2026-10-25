# Cozy Stack capacity model

This document lists the capacity questions the load-test harness answers or
will answer, how each one is measured, and when a measurement passes. It
belongs to [#4864](https://github.com/linagora/cozy-stack/issues/4864) and
covers [#4866](https://github.com/linagora/cozy-stack/issues/4866),
[#4867](https://github.com/linagora/cozy-stack/issues/4867), and the large
databases targeted by
[#4863](https://github.com/linagora/cozy-stack/issues/4863).

See [Testing architecture](../../docs/testing.md) for the delivery order and
shared DEV boundaries. The large levels below are proposals for isolated
capacity work, not defaults for shared DEV regression runs.

> [!IMPORTANT]
> Every number marked **proposed** is a starting point for discussion, not
> an agreed service level. Confirm it with the team before using it to gate
> a release or to compare campaigns.

## How a capacity is measured

A capacity is the highest level of one dimension at which every pass
criterion still holds, with all other dimensions fixed. The harness uses
three methods, chosen by the shape of the dimension:

| Method | Use it when | Implementation |
| --- | --- | --- |
| Point search | Each level needs an exact amount of work, its own data, and cleanup | `src/scripts/upload-campaign.ts` runs separate k6 points: baseline, doubling, bisection, confirmation |
| k6 ramp | The load is sustained and the levels can share one run | `src/scenarios/shared-drive-ramp.ts`: one k6 run, samples tagged with `level`, judged in `handleSummary()` |
| Step ladder | Each level needs data created first, and that data cannot be removed cheaply | Planned: extend the existing API seeder with representative profiles, run a fixed probe workload, and stop at the first failing step |

Bisection does not fit the step ladder: going back down would mean deleting
or recreating large datasets. A ladder result is therefore "passes at L,
fails at the next step", not an exact number.

## Measured today

| Capacity | Unit | Command | Pass criteria |
| --- | --- | --- | --- |
| Concurrent uploads, per file size | upload-and-readback workers | `make upload-capacity FILE_SIZE=…` | Success rate ≥ `MIN_SUCCESS_RATE` (0.99); no dropped uploads; every successful upload reads back with the same checksum; cleanup succeeds |
| Sustained shared-drive activity | level L = L writers, 3L WebSocket readers, 2L changes readers | `make cozy-shared-drive-ramp` | Upload, changes, WebSocket handshake, and WebSocket auth success rates ≥ `MIN_SUCCESS_RATE`; WebSocket events and changes received ≥ `MIN_DELIVERY_RATIO` (0.9) × uploads × readers |
| Organization-drive group fan-out | recipients in one group attach/remove | `make org-drive-fanout` | Owner readiness, every recipient's access, and denial after removal within the configured convergence budget |

Upload tests verify bytes. Activity tests currently check upload status and
aggregate delivery; they do not verify file readback or each reader's complete
expected state. Fan-out verifies recipient access and revocation separately;
see [its criteria and limits](ORG-DRIVE-FANOUT.md).

Upload and activity latency is recorded but judged only when an absolute
`P95_LIMIT_MS` is set,
because a limit derived from a nearly idle baseline mostly measured latency
doubling. The proposed latency criteria below are meant to be passed as
`P95_LIMIT_MS` once the team agrees on them.

The shared-drive ramp runs with the drive owner's credentials on one stack.
It does not exercise the recipient paths (see the README warning).

## Planned dimensions

### 1. Members per organization

**Question:** how many members can one organization have before membership
changes take too long to reach every member instance?

**Why it scales badly:** every `b2b.group.*` event is applied in turn to
every instance of the organization (`forEachOrgInstance` in
`model/orgdirectory/groups.go`). The work for one membership change grows
with the organization size, so the drain time of a batch of changes grows
with it too.

- **Unit:** member instances in the organization.
- **Method:** step ladder at 1k, 10k, 25k, and 50k members.
- **Fixed:** one organization, 50 groups, 3 groups per member on average,
  a batch of 1,000 `b2b.group.member.added` and `b2b.group.member.removed`
  events per step.
- **Measured:**
  - time from publishing the batch until the
    `stack.b2b.group.lifecycle` queue is empty;
  - `POST /data/io.cozy.contacts/_find` filtered by a group, on a member
    instance, while the batch drains.
- **Pass criteria (proposed):**
  - the batch drains within **60 s**;
  - no message is dead-lettered;
  - afterwards, every member instance has the expected group membership
    (convergence check);
  - contact query p95 **< 200 ms**, error rate **< 0.1 %**.
- **Prerequisites:** extend organization fixtures beyond their current 1,000-user
  bound and add real B2B event generation; declare the relevant RabbitMQ queues.
  Prometheus scrape configuration already exists; verify the broker exporter and
  per-queue metrics at runtime. Native group fan-out does not cover this event path.

### 2. Shares per instance

**Question:** how many shared drives can one instance hold before drive
listings, permission checks, and changes feeds become too slow?

- **Unit:** active shared drives on the instance.
- **Method:** step ladder at 100, 1k, 5k, 10k, and 25k drives.
- **Fixed:** 100 files per drive, 5 recipients per drive (contacts and
  groups), 10 VUs running the probe workload.
- **Measured:**
  - `GET /sharings/drives` (drive list);
  - `GET /sharings/doctype/io.cozy.files`;
  - `GET /sharings/drives/:id/:file-id` (every drive request resolves the
    member and checks the route, so this covers permission resolution);
  - `GET /sharings/drives/:id/_changes`;
  - the first drive list request after adding 100 drives. CouchDB updates
    views when they are read, so this captures the index update cost.
- **Pass criteria (proposed):**
  - drive list p95 **< 250 ms**;
  - file request p95 **< 150 ms**;
  - changes feed p95 **< 300 ms**;
  - first drive list after a batch **< 5 s**;
  - error rate **< 0.1 %**.
- **Prerequisites:** extend the existing seeder with populated drives, recipient
  relationships and the fixed probe workload.

### 3. Files per instance

**Question:** how many files can one instance hold before browsing, lookups,
and writes become too slow?

- **Unit:** documents in `io.cozy.files` (files and directories).
- **Method:** step ladder at 10k, 100k, 500k, and 1M documents.
- **Fixed:** a tree 3 to 5 levels deep with 50 to 200 entries per
  directory, 20 VUs running the probe workload, S3 storage (MinIO in the
  bundled stack).
- **Measured:**
  - `GET /files/:dir-id?page[limit]=100` (directory page);
  - `GET /files/:file-id` (metadata by ID);
  - `POST /files/:dir-id?Type=file` with a 1 KiB file;
  - `POST /files/_find` (Mango query);
  - the first `_find` and `GET /files/_changes` request after inserting
    1,000 files, for the same index update reason as above.
- **Pass criteria (proposed):**
  - directory page p95 **< 150 ms**;
  - metadata p95 **< 50 ms**;
  - 1 KiB upload p95 **< 300 ms**;
  - first query after a batch **< 10 s**;
  - error rate **< 0.05 %**.
- **Prerequisites:** extend the existing seeder's flat 1 KiB files with directory
  profiles, fixture verification and the fixed probe workload.

### 4. Request rate per instance

**Question:** what request rate can one instance serve under a mixed
workload before latency or errors exceed their limits?

- **Unit:** requests per second (open model), with VUs as a secondary view.
- **Method:** k6 ramp with `ramping-arrival-rate`, extending the
  shared-drive ramp to a mixed workload.
- **Fixed:** 10,000 files and 50 shared drives seeded beforehand; requests
  split 70 % reads (directory pages, metadata, changes feeds) and 30 %
  writes (1 KiB to 1 MiB uploads, metadata updates); WebSocket readers kept
  at a fixed number per level, because connections are not requests.
- **Measured:** `http_req_duration` tagged by operation, k6's
  `ws_connecting`, and cozy-stack's `go_goroutines` and memory, which
  Prometheus already scrapes.
- **Pass criteria (proposed):**
  - read p95 **< 300 ms**;
  - write p95 **< 500 ms** for uploads up to 1 MiB;
  - WebSocket connect p95 **< 100 ms**;
  - error rate **< 0.1 %**;
  - 30 s after each step, goroutines are back within 10 % of their level
    before the step.
- **Prerequisites:** verified fixture profiles and a mixed arrival-rate scenario.
  Define workflows per second separately from the resulting HTTP request rate.

## Conditions for every campaign

- The bundled stack shares the machine with k6, so it validates the
  harness, not capacity. Record capacity numbers in the dedicated load-test
  environment.
- Record the stack version, its configuration, and the CPU and memory limits
  of cozy-stack and CouchDB with every result. Limits are unset by default;
  set them with `COZY_STACK_CPUS`, `COZY_STACK_MEMORY`, `COUCHDB_CPUS`, and
  `COUCHDB_MEMORY`. `make campaign` records the commit, the topology, and the
  effective limits in each campaign directory.
- Results from a single host, where k6 shares the CPUs with the stack, are
  labelled `topology=single-host`. Compare them only with each other.
- Vary one dimension at a time and keep the others at the fixed values
  above.
- Record data ownership and cleanup outcomes. Upload/activity runs remove their
  temporary directory; fan-out retains its fixture for inspection and explicit
  cleanup. A timeout does not prove server writes have stopped.

## Remaining foundations

1. **Fixtures:** build versioned manifests and representative profiles around the
   existing [API seeder](SEEDING.md), [organization fixtures](ORGANIZATIONS.md)
   and [isolated snapshots](SNAPSHOTS.md). Keep managed DEV provisioning separate.
2. **Membership events:** add the B2B workload and queue declarations; verify the
   existing dependency scrape targets against the running services.
3. **Recipient paths:** use distinct identities, then a second stack with its own
   CouchDB. When the owner instance is on the same stack, shared-drive requests
   from recipients skip the HTTP proxy, so one stack cannot measure that path.
