# Twake Drive load-test harness

This directory contains the first infrastructure milestone for
[issue #4864](https://github.com/linagora/cozy-stack/issues/4864). It runs k6
as an on-demand container, stores metrics in Prometheus, and provisions
Grafana dashboards.

See [Testing architecture](../../docs/testing.md) for lifecycle boundaries,
correctness and result contracts, and the planned bounded shared DEV rollout.

For classified workload limits, tested bounds and fanout readiness evidence, see
[Load test results](CAPACITY-OVERVIEW.md). Workload and infrastructure diagnostics remain available.

The harness includes authenticated exact-work file-upload workloads for
personal drives, shared-drive activity and upload workloads, and an
adaptive search for the maximum upload concurrency of one Cozy instance.
See [CAPACITY.md](./CAPACITY.md) for the operational capacity model and pass criteria.
See [SEEDING.md](./SEEDING.md) for API-only fixtures and
[SNAPSHOTS.md](./SNAPSHOTS.md) for isolated Compose snapshot/restore/discard commands.
See [ORG-DRIVE-FANOUT.md](./ORG-DRIVE-FANOUT.md) for native group attach/remove
convergence. Upstream B2B membership events and broader file workloads remain
separate future scenarios.

## Quick start

| To | Where | Run |
| --- | --- | --- |
| Check the code: tests and types | Anywhere | `make node-checks` |
| Check that every runner, report and metric still works | Locally or on the load VM | `make campaign CAMPAIGN=smoke` |
| Measure every runner's capacity on the bundled Stack | The load VM | `make campaign CAMPAIGN=capacity` |
| Check, then measure, shared DEV | The load VM only | `make campaign CAMPAIGN=smoke LOAD_TARGET=dev`, then `CAMPAIGN=capacity` |

- `smoke` runs every step on its shortest ladder, in about ten minutes after the
  first build; `capacity` runs the full ladders ([Campaigns](#campaigns)).
- Locally, the campaign first builds and starts the bundled Stack from this
  checkout (`make cozy-prepare`), then seeds one organization for the ramp's
  members and the group fan-out and a tree fixture for the
  [mixed workload](#mixed-workload), and removes both after their steps.
- Shared DEV needs `SEED_CLOUDERY_TOKEN_FILE`, `SEED_CLOUDERY_MANIFEST`,
  `DEV_INSTANCE`, and `MIXED_DOMAIN` with `SEED_MANIFEST` for its seeded tree
  fixture. It holds the shared DEV lock and uses the budgets agreed with the
  DEV owner, in `campaigns/capacity.sh` ([Shared DEV](#shared-dev)).
- Each run writes `data/results/campaigns/<run id>-<name>/summary.md`, with a
  Grafana link per step, and exits 0 when completed, 1 on a proven correctness
  failure and 2 when inconclusive.
- One runner alone: `make cozy-prepare`, then `make upload-capacity`,
  `upload-regression`, `shared-drive-ramp` or `org-drive-fanout` (which seeds
  its own organization, [ORG-DRIVE-FANOUT.md](ORG-DRIVE-FANOUT.md)). Add
  `LOAD_TARGET=dev` for shared DEV.

## How it is built

The short version is:

- k6 sends workload traffic and records checks and measurements.
- Node prepares data, coordinates complete runs and observes group fan-out.
- Scenario producers classify results; reports and dashboards preserve them.
- Make and Docker Compose connect the commands and services.
- Prometheus and Grafana store and display the results.

The Node code is not another load-testing framework. There is no Jest
dependency here either. `npm test` uses Node's built-in
`node:test` runner to check helper code. k6 generates measured workload traffic;
the Node fan-out runner measures one administrative mutation and bounded
convergence observations.

~~~mermaid
flowchart LR
    Developer --> Make
    Make --> Node[Node setup tools]
    Make --> K6[k6]
    Capacity[Node capacity search] -->|runs several points| Make
    K6 --> Cozy[Mock, local, or remote Cozy]
    K6 --> Prometheus --> Grafana
    K6 --> Summary[Point result]
    Summary --> Capacity
    Capacity --> Report[Capacity report]
~~~

`tests/load` is one private npm package. The top-level folders separate code,
service configuration, and generated data:

```text
tests/load/
├── src/
│   ├── scenarios/       k6 entry points
│   ├── fixtures/        upload-data code and supported sizes
│   ├── lib/             reusable Cozy and consistency helpers
│   ├── scripts/         Node setup and capacity tools
│   └── mock/            streaming API used by the upload smoke test
├── campaigns/           campaign definitions run by make campaign
├── config/
│   ├── cozy/
│   ├── prometheus/
│   └── grafana/
├── data/
│   ├── upload-binaries/ generated input files, ignored by Git
│   └── results/         summaries, capacity reports, and campaign
│                        directories, ignored by Git
├── Makefile
└── compose.yaml
```

Tests live beside the TypeScript modules they cover. For example,
`src/lib/upload-consistency.test.ts` tests
`src/lib/upload-consistency.ts`. This keeps each small code area together
without turning every folder into a separate npm package.

`tsconfig.k6.json` knows about k6 APIs, while
`tsconfig.node.json` knows about Node APIs. This catches code that
accidentally mixes both runtimes.

## Prerequisites

- Docker Engine with Docker Compose
- GNU Make
- Node.js 22.23.4 or newer with npm (22.23.2 and 22.23.3 break TLS to IPv6
  loopback), or Docker alone with the Node image commands below

## Run locally

From the repository root:

```console
$ make load-test
```

Or run the harness directly:

```console
$ cd tests/load
$ make upload-smoke
$ make dashboard-url
```

The upload smoke target uses the bundled streaming Node.js mock to validate
the complete upload, download, checksum, and cleanup flow without starting a
Cozy stack.

Grafana defaults to <http://localhost:3000/> with the local credentials
`admin` / `admin`. Copy `.env.example` to `.env` to change the credentials,
target URL, published port, retention size, or restart policy. Do not commit
`.env`.

## Run against the bundled Cozy stack

Build the current cozy-stack checkout, start its local dependencies, provision
an instance and OAuth client, then run one real upload lifecycle against it:

```console
$ make cozy-prepare
$ make upload-regression FILE_SIZE=1K VUS=1
```

The opt-in `cozy` Compose profile starts these services on the same private
network as k6, Prometheus, and Grafana:

- cozy-stack built from the current repository checkout
- CouchDB 3.3.3
- Redis 7.2
- RabbitMQ 3.13
- MinIO using the stack's S3 storage backend

The stack is published at <http://load.localhost:8080>, but k6 reaches the
`load.localhost` network alias directly. This keeps load traffic inside Docker
instead of routing it through a host port. The stack admin and metrics endpoint
stays private to the Compose network, where Prometheus scrapes it directly.

`make cozy-prepare` creates `load.localhost:8080` when needed, reuses its
OAuth client, and generates a fresh 24-hour `io.cozy.files` token. It writes the
target (`BASE_URL`) and token to the ignored local `.env` without printing the
token, so every runner then targets the bundled stack.

After the lifecycle check passes, run a bounded capacity search against the
real stack:

```console
$ make upload-capacity FILE_SIZE=1K MAX_VUS=8
```

Inspect the services with `make cozy-status` or `make cozy-logs`. Stop only the
bundled Cozy services with `make cozy-down`; their named volumes and the
Grafana and Prometheus services are preserved.

## Run against a Cozy stack on the host

To exercise a Cozy stack running on the host, `cozy.localhost` and
`host.docker.internal` resolve to the Docker host from the k6 container. The
local stack's port 6060 metrics endpoint is also scraped when it is available.

Start CouchDB, then run the stack and create its development instance:

```console
$ make run
```

In another terminal:

```console
$ make instance
$ cozy-stack instances client-oauth cozy.localhost:8080 http://localhost/twake-load "Twake Drive load tests" twake-drive-load
$ cozy-stack instances token-oauth cozy.localhost:8080 <client-id> io.cozy.files --expire 24h
```

Put the returned access token in the ignored `.env`:

```dotenv
BASE_URL=http://cozy.localhost:8080
COZY_ACCESS_TOKEN=replace-with-the-local-token
```

The values from `.env` are loaded by both Make and Docker Compose, so the
explicit URL is optional after this setup:

```console
$ make upload FILE_SIZE=1M VUS=10
```

## Concurrent uploads

The upload workload supports `1K`, `100K`, `1M`, `10M`, `100M`, and `1G`
binary files. They are generated from random data once and kept under the
`data/upload-binaries/` directory and remain ignored by Git. A run generates
only its selected size; use `make upload-binaries` to prepare all sizes in
advance.

The `src/scripts/generate-upload-binaries.ts` tool runs through `tsx` and
streams random bytes to disk with a bounded 1 MiB buffer. k6 loads the selected
file during its initialization context and keeps upload data in memory, while
`src/fixtures/upload-data.ts` owns loading and per-upload mutation for reuse by
multiple scenarios.

Run one upload per virtual user against a local stack:

```console
$ make upload BASE_URL=http://cozy.localhost:8080 FILE_SIZE=1M VUS=10
```

`VUS` is the number of concurrent upload-and-check workflows.
`ITERATIONS_PER_VU` defaults to one, which keeps the uploaded volume
predictable. For example, this runs 50 uploads with up to 10 active workflows:

```console
$ make upload BASE_URL=http://cozy.localhost:8080 FILE_SIZE=100K VUS=10 ITERATIONS_PER_VU=5
```

`MAX_DURATION` is only a safety deadline, not a way to hold load for a fixed time.

Each workflow also includes the consistency download, so `VUS=10` means ten
active client workflows, not that ten upload bodies are always in flight.

Each k6 point creates a uniquely named directory before starting its VUs. All
VUs in that point share the same Cozy instance, OAuth token, and directory.
After measurement, teardown moves the directory to the trash and permanently
deletes it. A setup or cleanup failure is reported as an infrastructure
failure rather than a capacity result.

Before each request, the scenario changes 256 bytes in the selected in-memory
binary. The mutation includes the execution ID, VU, iteration, time, and a
random value, so every uploaded file has distinct content without regenerating
the whole binary. The base binary on disk remains unchanged.

Before uploading, the scenario calculates the binary MD5 and sends it as
`Content-MD5`. After every `201` response, it validates the returned file ID,
name, size, and MD5, then immediately downloads the complete file and compares
its MD5 with the original upload. Any response, size, range, or checksum
inconsistency aborts the whole k6 run; teardown still removes the test
directory.

k6's HTTP API accepts and returns complete buffers rather than JavaScript
streams. To avoid allocating a second full-file buffer, consistency downloads
use sequential HTTP Range requests and feed each response into an incremental
MD5 hasher. The default chunk is 16 MiB and can be reduced to trade more HTTP
requests for lower load-generator memory usage:

```console
$ make upload FILE_SIZE=1G VUS=2 CONSISTENCY_CHUNK_SIZE_BYTES=4194304
```

Consistency downloads use the `file-consistency-download` operation tag, so
the upload p95 and upload success thresholds still measure only the upload
requests. The downloads remain part of the workload and may overlap with
slower uploads, as they would in an immediate read-after-write check.

### Shared-drive load testing

> [!NOTE]
> Without `ACTORS_DOMAIN`, the drive owner's instance and credentials play every role; with it, organization members on the same Stack write and read (below). Cross-Stack recipients remain a separate scenario.

Shared drives can be tested in two modes:

1. **Activity ramp (`shared-drive-ramp.ts`)**: finds the highest activity level one instance sustains, one k6 run per level, with concurrent collaborative activity combining:
   - **File writers**: concurrent uploads into the shared drive.
   - **WebSocket readers**: persistent connections to `/sharings/drives/:id/realtime` listening for real-time `CREATED`/`UPDATED` events.
   - **Changes feed readers**: periodic polling of `/sharings/drives/:id/_changes`.

   ```console
   $ make shared-drive-ramp CAPACITY_MAX=16 CAPACITY_HOLD=1m
   ```

   The ramp goes through the campaign runner ([Upload campaigns](#upload-campaigns)).
   It creates or reuses the instance's `Load-Test Shared Drive`, stops k6 at
   its deadline or on Ctrl-C, deletes the run's own
   `twake-load-sd-<EXECUTION_ID>-*` directory in the drive, and adds what it
   verified to the report. It targets `BASE_URL`; with `LOAD_TARGET=dev` it
   targets shared DEV, which requires `MAX_VUS`
   (workers and readers at the highest level), `MAX_UPLOAD_BYTES` (writers
   upload at most once a second) and `MAX_RUNTIME`, all checked before any
   request. On DEV, the whole ramp must also fit in one 20-minute k6 run.

   With `ACTORS_DOMAIN`, the ramp runs as members of an organization fixture
   (`make seed-org SEED_USERS=2 SEED_CONTEXT_NAME=load-org-sharing`, read from
   its [manifest](SEEDING.md#fixture-manifest)) on a local Stack, using the
   seeder's `SEED_ADMIN_URL`, `SEED_ADMIN_PASSWORD` and `SEED_STACK_URL`
   (the bundled Stack's by default). The
   organization instance owns a fresh drive and creates the test directory;
   its first member uploads through a group-only grant; its second member
   observes through a read-only grant. Each uses its own URL and token. Before
   the ramp, the runner checks every identity against the manifest and proves
   both members reach the drive and the writer can write. After it, the runner
   proves the reader cannot write, removes the writer's group and proves the
   writer loses drive access while the reader keeps it, then removes the drive,
   groups and contacts. A member allowed more than its grant, or a writer still
   in after removal, exits 1. The report's `actors` records each actor's Stack
   and whether recipients take the same-Stack path, with every access status.

   ```console
   $ make shared-drive-ramp ACTORS_DOMAIN='seed-<32-hex-id>.localhost:8080' CAPACITY_MAX=2
   ```

   Level L runs L writers, 3L WebSocket readers, and 2L changes readers
   (`RAMP_WRITERS_PER_LEVEL`, `RAMP_WS_READERS_PER_LEVEL`,
   `RAMP_CHANGES_READERS_PER_LEVEL`). Each level gets a 10s transition and
   a hold of `CAPACITY_HOLD`; new VUs start in the first half of the
   transition, so every reader is connected before the hold. Samples taken during the hold are tagged
   with `level`; transitions are tagged apart and never judged.

   After the run, `handleSummary()` judges each level on errors and
   missing events:

   - the upload, changes, WebSocket handshake, and WebSocket auth success
     rates must reach `MIN_SUCCESS_RATE` (default 0.99);
   - every reader ready before the hold must report the level: a WebSocket
     session connected before the hold began, or a changes reader whose feed
     answered before it, reports the unique uploads of the level it saw once
     the hold and a 2 s drain are over. A level fails when fewer readers report
     than it runs, or when the worst reader saw less than `MIN_DELIVERY_RATIO`
     (default 0.9) of the level's successful uploads. Repeated events and
     coalesced revisions count once, so they never stand in for a missing file
     or reader; totals stay in the report for reference;
   - a changes reply that cannot be read is a failed poll;
   - writers check that the Stack recorded each upload's checksum, size and
     name. Each changes reader downloads up to `CONTENT_CHECKS_PER_LEVEL`
     (default 2) new files per level and compares them with the bytes their
     writer derived from the name. Wrong content or a refused download fails
     the level and exits 1, whatever the other levels measured. A check that
     reads nothing (a missing file, a server error, a lost connection) is
     recorded as `content_unverified` and keeps the level from passing, as
     does a level with no verified content; the report records the checks
     made as `content_verified` and the scope as
     `criteria.content_checks_per_level`. Wrong content outside any held
     level, such as a writer's upload during a transition, fails the ramp too.

   Latency is judged only when `P95_LIMIT_MS` is set: the upload and
   changes p95 must then stay within it. Otherwise each level's p95 is
   reported for reference.

   The result is written to
   `data/results/<EXECUTION_ID>-shared-drive-ramp.json`.
   `max_sustained_level` is the highest fully completed passing level before
   the first level that did not pass. Each level records `hold_completed` from
   `shared_drive_ramp_hold_completed`: a new worker iteration after the hold
   boundary emits the signal; the changes-reader drain covers the final hold.
   Setup/teardown time and a late in-flight request cannot complete a hold.
   A sampled partial hold is `not_measured`, never a sustained level.

   `result_bound` is `bracket` when a completed level failed: passing 8 and
   failing 16 reports `[8, 16)`, with `first_failed_level: 16`, not an exact
   maximum of 8. It is `lower_bound` when every level passed and `inconclusive`
   when a hold was partial, metrics were missing, or a level was not reached.
   A level with missing metrics never counts as passed. The run stops early
   when more than half of the HTTP requests fail after the first 30 seconds.

   A failing completed level is a result, not an error: the runner exits 0 for
   a bracket and 1 only when the first level fails. Leftover data, an
   interruption or a missing report leaves the run inconclusive (exit 2).
   Cleanup failure (including an absent/empty cleanup metric) invalidates the
   report: `status: infrastructure_failure`, a cleanup `failure_reason`,
   `result_bound: inconclusive` and `max_sustained_level: null`. Raw per-level
   diagnostics remain available. Automation must check status and result bounds,
   not just the maximum or k6 exit code; campaign summaries use the same rules.

2. **Direct upload target mode in `concurrent-uploads.ts`**: sends pure upload traffic directly to an existing shared-drive directory root.

To exercise the current API mode, pass an existing directory-root shared
drive's sharing ID and root directory ID. Read both identifiers from
`GET /sharings/drives` on the instance being tested; the sharing ID is
`data[].id` and the root ID is `data[].attributes.rules[0].values[0]`.

```console
$ make upload BASE_URL=https://member.example \
    FILE_SIZE=100K VUS=10 \
    UPLOAD_TARGET=shared-drive \
    SHARED_DRIVE_ID=aae62886e79611ef8381fb83ff72e425 \
    SHARED_DRIVE_ROOT_ID=357665ec-e79711ef94fbf3d08ccb3ff5
```

`BASE_URL` and `COZY_ACCESS_TOKEN` must identify the same owner or active
recipient instance. The scenario creates its temporary directory inside the
configured shared-drive root and performs upload, trash, and permanent-delete
operations through `/sharings/drives/:id`. It leaves the shared drive itself in
place.

The bundled Cozy stack uses MinIO through the S3 storage backend. For a remote
stack, configure its `fs.url` for S3 and run this same HTTP scenario against
it. S3 credentials do not belong on the load generator.

Run each size separately. According to the
[k6 guidance for large tests](https://grafana.com/docs/k6/latest/testing-guides/running-large-tests/#file-upload-considerations),
file uploads are copied for each VU and require significant memory. The
capacity coordinator applies disk and Docker-memory guards before generating
the selected upload binary or starting k6.

## Find upload capacity

Run the adaptive search for one file size:

```console
$ make upload-capacity FILE_SIZE=10M
```

For each size, the coordinator:

1. Measures 50 uploads at 1 VU as a baseline point.
2. Tries burst levels 2, 4, 8, and so on, with one upload per VU.
3. Retries a failing burst once and uses a third run when the first two
   disagree.
4. Binary-searches between the last passing and first failing levels.
5. Confirms the best candidate with 20 uploads per VU, searching downward
   again if sustained confirmation fails.

The search uses Node because it must wait for a complete k6 point before it can
choose the next VU value. Every point is still a normal k6 run: k6 owns the
VUs, requests, metrics, thresholds, cleanup, and exit status. Node only chooses
which point to run next.

We should not use Node for steady load, ramps, or fixed request rates. k6
already supports those workload shapes.

A point passes when upload success is at least `MIN_SUCCESS_RATE` (99%), no
iterations are dropped, and every upload reads back with the same checksum. A
checksum mismatch stops the search at once. Latency is not judged by default, because a limit derived
from the nearly idle 1-VU baseline mostly measured latency doubling. To add a
latency contract, set an absolute `P95_LIMIT_MS`. Every point's p95 is in the
report either way.

```console
$ make upload-capacity FILE_SIZE=100K MAX_VUS=64 \
    BASELINE_ITERATIONS=50 CONFIRMATION_ITERATIONS=20 \
    P95_LIMIT_MS=2500 MIN_SUCCESS_RATE=0.99
```

The baseline and confirmation counts are configurable because large binaries
can make those defaults intentionally expensive. Lower them only when the
result is being used as a smoke check instead of a capacity measurement.

Default search caps keep the load-generator footprint bounded:

| File size | Default `MAX_VUS` |
| --------- | ----------------: |
| `1K`      |               256 |
| `100K`    |               256 |
| `1M`      |               128 |
| `10M`     |                32 |
| `100M`    |                 8 |
| `1G`      |                 2 |

Before a campaign, the coordinator estimates k6 memory as `1 GiB + MAX_VUS ×
(3 × file size + min(file size, consistency chunk size))`. It also accounts
for the upload binary and the larger of the baseline or confirmation directory,
and refuses to leave less than 5 GiB free on the load-generator filesystem.
`FORCE_CAPACITY_RUN=1` bypasses these guards; use it only after checking Docker
memory and both load-generator and Cozy storage manually. Every successful
upload is downloaded once, so include that additional traffic in network and
object-storage cost estimates.

The aggregate report is written to
`data/results/<campaign>-upload-capacity.json`. It records every attempt, p95,
success rate, consistency rate, dropped and completed iterations, cleanup
result, and the k6 summary path. An `exact` result has a measured failing level above it. A
`lower_bound` result is displayed as `maximum >= MAX_VUS`, because the
configured cap passed and no higher level was attempted.

The search exits 0 with a completed result, 2 when it stopped on an
infrastructure failure, and 1 when no level passed or any upload read back with
a different checksum. The search runs through the shared upload runner, so its
budgets and per-point cleanup are those of [Upload campaigns](#upload-campaigns).

Grafana selects a recorded run through Environment, Mode and Run. Inspecting a
point derives its execution ID, concurrency and phase. A campaign groups every
capacity-search point. An
execution ID identifies one k6 process at one concurrency and attempt; for a
standalone upload run, the campaign and execution ID are the same.
Make generates a UTC execution ID by default. Pass `EXECUTION_ID=name` for a
standalone run or `CAPACITY_RUN_ID=name` for a capacity campaign when you want
a stable name in Grafana.

Workload shows successful upload p50/p95/p99 in seconds, the Prometheus base
unit used by k6 remote write. Final per-attempt report values remain in
milliseconds. Failure counts require recorded evidence; unavailable counts do
not become zero. HTTP status is available in raw k6 metrics; server-side error
messages remain in Stack logs and are not exported as metric labels.

Use the Stack source selector to inspect either the bundled Compose target or
a stack started directly on the host. The stack panels show scrape health,
request rate, error rate, server latency, CPU, memory, goroutines, file
descriptor usage, garbage collection, and upload-related worker queues.

For the bundled stack, Prometheus also scrapes CouchDB
(`/_node/_local/_prometheus`) and RabbitMQ (port 15692, including per-queue
depth). The Infrastructure tab shows CouchDB requests, request time, error
responses, open databases and files, and RabbitMQ memory and queued messages.

The **Workload** tab follows shared-drive activity level by
level: success rates, WebSocket events and changes delivered per upload level,
upload, changes, and WebSocket connect p95, VUs, and stack load.

The stack HTTP metrics are labelled by method and status code, but not by
route, file size, instance, or k6 campaign. During a controlled upload campaign,
`POST` responses with status `201` are mostly file uploads, with one directory
creation per test point. Use the shared dashboard time range to correlate stack
and k6 metrics; use the k6 upload metrics for exact scenario attribution.

## Mixed workload

Fixed-VU tests send less traffic when responses slow down. `make
mixed-workload` instead starts a fixed number of workflows per second with
k6's `constant-arrival-rate` executor, whatever the latency, against a
verified single-instance tree fixture ([SEEDING.md, Profiles](SEEDING.md#profiles))
on a local Stack. Each workflow lists a fixture directory, reads a fixture file's
metadata and checks it against the manifest, and uploads a 1 KiB file into
the run's own directory, checking the checksum and size the Stack recorded.
`MIXED_OBSERVERS` WebSocket observers (default 1) each open one session on the
instance's file events, apart from the arrival rate; a dropped session is
recorded, never retried.

```console
$ make seed-scale SEED_PROFILE=tree
$ make mixed-workload MIXED_DOMAIN='seed-<32-hex-id>.localhost:8080' MIXED_RATE=2 MIXED_DURATION=2m
```

Start at a small fixed rate (`MIXED_RATE`, workflows per second, default 1)
and validate it before any ramp or capacity search. `MIXED_MAX_VUS` (default
10) bounds the VUs k6 may use to keep the rate. `MAX_VUS`, `MAX_REQUESTS`,
`MAX_UPLOAD_BYTES` and `MAX_RUNTIME` are checked before any request against
what the run can need: the arrivals, the observers, k6 setup, both fixture
verifications and cleanup, counted from the fixture's manifest. The runner verifies the fixture before the run,
removes the run's directory afterwards, and verifies the fixture again.

`data/results/<EXECUTION_ID>-mixed-workload.json` records the offered and
achieved workflows per second, dropped iterations, each operation's request
rate, p95 and error rate, the correctness checks, the observers' sessions and
events, and the [result envelope](#result-envelope). Every operation must
reach `MIN_SUCCESS_RATE` (default 0.99) and stay within `P95_LIMIT_MS`
(default 2000), both checked before any request, and every successful metadata read and upload must have been
checked: a response that cannot be checked keeps the run from passing.
Dropped workflows while every operation met its limits mean
the generator fell behind: inconclusive, exit 2. With a slow or failing
target, they are part of that regression, exit 1. Wrong metadata or a
wrongly recorded upload exits 1. On shared DEV (`LOAD_TARGET=dev`), the
workload runs against a tree fixture seeded through the Cloudery
([SEEDING.md](SEEDING.md#shared-dev-through-the-cloudery)), under the shared DEV
lock and with all four budgets. It first removes what an interrupted earlier
run left in the fixture. A soak needs the Stack admin metrics, so it runs locally
only.

### Capacity

`make mixed-capacity` finds the highest rate the fixture sustains: it runs the
same workload at each [capacity level](#capacity-searches) (default 1 to 8
workflows per second), for `CAPACITY_HOLD` each (default 1m), one k6 run per rate, and
stops at the first rate that does not pass. Each rate gets at least eight VUs
per workflow per second, so arrivals drop only once a workflow takes longer
than eight seconds. The budgets cover the first rate; a later rate beyond them ends
the search as a lower bound.

```console
$ make mixed-capacity MIXED_DOMAIN='seed-<32-hex-id>.localhost:8080' CAPACITY_MAX=16
```

`data/results/<EXECUTION_ID>-mixed-capacity.json` keeps every rate's verdict,
the highest passing rate and the first failing one; the dashboard shows each
rate, and each operation's request rate and p95 over its k6 run. Every rate passing gives a
lower bound, and a later rate failing a bracket: both exit 0. The first rate
failing exits 1, as does wrong content at any rate. A rate the generator could
not keep, or that did not run cleanly, ends the search inconclusive, exit 2.
The fixture is verified before the first rate and after the last.

### Soak

`make mixed-soak` keeps the same workload up for longer, to catch memory,
goroutines, queues or data that accumulate. It runs a warm-up at the same
rate (`MIXED_WARMUP`, default 1m, never judged), then, once the warm-up's last
workflows ended within a 10 s grace, the judged steady phase
(`MIXED_DURATION`, default 10m), so the two never run more than
`MIXED_MAX_VUS` workers together. It then waits `MIXED_DRAIN` (default 1m)
before the final verification. The report records each phase's times apart, the
warm-up's workflows, and the Stack's goroutines, heap and resident memory
from its admin `/metrics` before the run and after the drain. Goroutines must
come back within `SOAK_MAX_GOROUTINE_GROWTH` (default 1.25×, plus 20) and the
heap within `SOAK_MAX_HEAP_GROWTH` (default 1.5×, plus 16 MiB); a Stack that
keeps what the run allocated exits 1. Every successful steady-phase response
must have been checked, whatever the warm-up checked, and wrong content in the
warm-up still exits 1. The request, byte and time caps cover
the warm-up and its grace, the observers, both fixture verifications, cleanup
and the metrics samples.

```console
$ make mixed-soak MIXED_DOMAIN='seed-<32-hex-id>.localhost:8080' MIXED_RATE=2 MIXED_DURATION=5m
$ make mixed-soak MIXED_DOMAIN=... MIXED_RATE=2 MIXED_DURATION=1h \
    SOAK_TRIAL=data/results/<trial EXECUTION_ID>-mixed-workload.json
```

A soak longer than 10 minutes needs `SOAK_TRIAL`: the report of a passed
shorter run of the same fixture at its rate or higher. On Ctrl-C or SIGTERM,
k6 stops, the run's directory is removed with any write still landing in it,
the drain is skipped, and the report keeps the interrupted outcome. One mixed
workload or soak runs per local Stack at a time: the runner holds
`data/results/.mixed-workload.lock` (a lock whose process is gone is taken
over), so a scheduled run that finds another in progress exits 2. It also
exits 2 while a k6 container still runs a mixed workload, such as one a killed
coordinator left behind, or while a listed k6 container cannot be inspected;
stop that container first. Soaks never
run on shared DEV; process kills and dependency outages stay isolated work.

## Campaigns

A campaign runs every runner in a row and keeps everything about the run
together. `campaigns/capacity.sh` defines the steps once, with two presets:
`smoke` runs each step on its shortest ladder, in about ten minutes, to check
that every step, report and metric still works; `capacity` runs the full
ladders to find each runner's capacity:

```console
$ make campaign CAMPAIGN=smoke
$ make campaign CAMPAIGN=capacity
$ make campaign CAMPAIGN=capacity LOAD_TARGET=dev   # shared DEV, from the load VM (Shared DEV below)
```

The steps are the 100 KiB upload capacity on the personal drive, the
shared-drive ramp, the group fan-out and, locally, the mixed workload's
capacity search. Locally, one organization fixture, seeded before the ramp and
removed after the fan-out, gives the ramp its writer and reader members and the
fan-out its recipients, and a tree fixture is seeded for the mixed search and
removed after it; `run.log` names both, for `make seed-org-cleanup` or
`make seed-scale-cleanup SEED_DOMAIN=...` after an interrupted run. Locally,
the steps keep going after a failure.
A definition is a `campaign()` function calling `upload_capacity`,
`shared_drive_ramp`, `org_drive_fanout`, `mixed_capacity`, or `run_step`.

Each run writes `data/results/campaigns/<run id>-<name>/`:

- `run.log`: the full output;
- `stats.log`: container CPU and memory every 10s, to tell whether k6 or the
  stack saturated first;
- `environment.txt`: git commit, topology, host, and the configured and
  effective CPU and memory limits;
- `steps.tsv`: one line per step with its campaign ID, times, make's exit
  code, and the producer's own exit code (empty when the producer never ran);
- `summary.md`: the campaign outcome, then one line per step with its result,
  its outcome, and a Grafana link for its time range. Set `GRAFANA_URL` when
  Grafana is not on `localhost`.

`make campaign` exits with the campaign outcome:

| Exit | Meaning |
| ---: | --- |
| 0 | Every step completed in its declared mode, with required verification and cleanup |
| 1 | A proven correctness failure, such as an upload that read back with a different checksum |
| 2 | Inconclusive: setup failure, missing or invalid report, interruption, incomplete verification, or failed cleanup |

A correctness failure takes precedence over inconclusive steps, and the step
outcomes keep both reasons. A failed early step still fails the campaign when
later steps complete. In capacity mode, a lower bound or a tested bracket is a
completed result, including expected overload: k6 stopping the ramp on mostly
failed requests (exit 99) counts as overload once cleanup is verified. A search
whose first level fails measured no capacity and exits 1, and content read back
with other bytes exits 1 at any level, even after passing ones. Performance stays
observational until agreed criteria exist. make reports every failed recipe as
2, so steps also record the producer's exit, and a completed report never
explains an unexpected one.

Ctrl-C or SIGTERM sends the running step a single SIGTERM, so k6 can still stop
gracefully and clean up, skips the remaining steps, and still writes the
summary with outcome 2; `make campaign` itself then ends with the signal's
status. Treat any exit other than 0 and 1, or a run directory without
`summary.md`, as inconclusive: that also covers a campaign killed outright.

### Capacity searches

Every capacity search steps through the same kind of ladder, each in its own
unit. It starts at `CAPACITY_START` (default 1), multiplies the last level by
`CAPACITY_GROWTH` (default 2) and ends at `CAPACITY_MAX`, which it always
tests. The shared-drive ramp and the mixed workload hold each level for
`CAPACITY_HOLD` (default 1m). An upload level holds for its uploads per worker
(`CONFIRMATION_ITERATIONS`), and a fan-out level until its recipients converge.
Budgets stay each runner's own (`MAX_VUS`, `MAX_REQUESTS`, `MAX_UPLOAD_BYTES`,
`MAX_RUNTIME`), locally and on shared DEV.

| Runner | Unit | Default `CAPACITY_MAX` |
| --- | --- | --- |
| `upload-capacity` | concurrent uploads | what `MAX_VUS` and `MAX_UPLOAD_BYTES` allow |
| `shared-drive-ramp` | activity level | 16 |
| `org-drive-fanout` | recipients | 16 |
| `mixed-capacity` | workflows per second | 8 |

Every search advances after each passing level and stops at the first that does
not pass; the ramp and the mixed workload run each level in its own k6 run
(`src/scripts/capacity/step-ramp.ts`). The first level must fit the budgets; a
later level beyond them, or one that would not finish within `MAX_RUNTIME`, ends
the search as a lower bound. The upload search
refines its result by bisection and confirmation; `CONFIRMATION_ITERATIONS=0`
skips that and keeps the bracket its levels found. The result envelope's
`capacity.stop` records why a search stopped: its first failing level and why,
the budget it reached, or that every level passed. The dashboard's **Why the run
stopped** shows its category.

### Result envelope

Each producer report (upload capacity or regression, shared-drive ramp, group
fan-out) also carries `result`, a versioned envelope (`cozy-load-result/1`,
`src/scripts/result-envelope.ts`) written once by the producer. It keeps apart:

- `outcome` and `reason`: the producer's verdict, with the exit codes above;
- `execution` (`completed`, `interrupted`, `incomplete`), `correctness` and
  `cleanup` (`verified`, `failed`, `not_verified`), and `performance`
  (`passed`, `failed`, `not_measured`);
- `capacity`: `exact`, `lower_bound` or `bracket` with its unit, and `verified`
  false when a measurement the run could not verify stays evidence only;
- `versions` (harness and Stack apart), `workload`, `dataset`, `load_target`,
  `topology`, `criteria`, `parameters` and the `time` range.

The campaign summary and the results exporter take the verdict from a valid
envelope and never recompute or upgrade it. An envelope that is invalid or
contradicts itself (a pass that was interrupted, unverified, or left failed
cleanup, for instance) counts as inconclusive. Older reports without one keep
their earlier, conservative classification. The ramp's verdict comes from its
k6 `handleSummary()`; the upload search and the fan-out decide theirs in Node.

### Topology and resource limits

k6 runs on the same host as the stack unless a separate load generator is
used, so its CPU use competes with Cozy. Campaigns record `LOAD_TOPOLOGY`
(default `single-host`), and every k6 run tags its metrics with `topology`.
Compare results only between runs with the same topology and limits.

CPU and memory limits for cozy-stack and CouchDB come from the environment or
`.env`, and are unlimited when unset:

```dotenv
COZY_STACK_CPUS=2
COZY_STACK_MEMORY=4g
COUCHDB_CPUS=1
COUCHDB_MEMORY=2g
```

## Upload campaigns

Every upload campaign goes through one runner, `src/scripts/upload-campaign.ts`.
It runs exact amounts of work, stops k6 at its deadline or on Ctrl-C, checks
every checksum, deletes the run's own `twake-load-*` directory after each k6
run, and classifies the result the same way on every target. Two modes:

| Mode | Command | Result |
| --- | --- | --- |
| Regression | `make upload-regression` | One fixed `VUS` x `ITERATIONS_PER_VU` workload: 0 passed, 1 correctness or criteria failure, 2 inconclusive |
| Capacity | `make upload-capacity` | The [search](#find-upload-capacity): an exact bracket, a lower bound at a budget, or inconclusive |

Budgets are campaign settings, recorded in the report:

| Setting | Bounds |
| --- | --- |
| `MAX_VUS` | Concurrent workers |
| `MAX_UPLOAD_BYTES` | Bytes one k6 run uploads before its cleanup, such as `10M`; it caps the search level |
| `MAX_RUNTIME` | Load time for the whole campaign; no point starts after it, and a running point is stopped |
| `UPLOAD_TIMEOUT` | One request |

A search that reaches `MAX_VUS` or `MAX_UPLOAD_BYTES` ends as a lower bound.
When `MAX_RUNTIME` ends it, only levels that passed confirmation count: the
highest is a lower bound, and without one the search is inconclusive. If the
stopped point's cleanup is not verified, the run exits 2 and the report keeps
that lower bound only as evidence. A
checksum mismatch stops either mode at once and exits 1, even when cleanup also
failed. Leftover data, a timeout or an interruption leaves the run inconclusive
and the report names what remains. The report,
`data/results/<id>-upload-<mode>.json`, records every attempt, the budgets, the
Stack, harness, Node and k6 image versions and monitoring links.

Targets differ only in preparation, credentials and coordination:

| | Local (`LOAD_TARGET=local`) | Shared DEV (`LOAD_TARGET=dev`) |
| --- | --- | --- |
| Preparation | `cozy-prepare`, or any `BASE_URL` | An instance of the Cloudery fixture ([SEEDING.md](SEEDING.md#shared-dev-through-the-cloudery)), checked against the Stack's UUID |
| Credentials | `COZY_ACCESS_TOKEN` from `.env` | A token minted for that instance before each k6 run |
| Budgets | Optional, with search caps by file size | Required, as agreed with the DEV owner |
| Coordination | None | The shared DEV lock, with recovery of killed runs |

Locally, `STACK_URL` is where the host reaches the Stack when k6 uses a Compose
name; it defaults to the bundled stack's host port when `BASE_URL` is that
stack. Runs against the mock or a shared drive
leave cleanup to k6's own teardown check.

### Shared DEV

Run DEV campaigns only from the designated load VM, with budgets agreed with
the DEV owner. [SHARED-DEV.md](SHARED-DEV.md) holds the current agreement, stop
conditions, runbook and baseline:

```sh
export SEED_CLOUDERY_TOKEN_FILE=~/private/cloudery-token.json
export SEED_CLOUDERY_MANIFEST=~/private/<prefix>-cloudery-fixture.json
export DEV_INSTANCE=<prefix>01.<domain> MIN_SUCCESS_RATE=1 UPLOAD_TIMEOUT=60s LOAD_TARGET=dev
make upload-regression MAX_VUS=1 MAX_UPLOAD_BYTES=1K MAX_RUNTIME=5m                   # 1 x 1 x 1 KiB
make upload-regression VUS=2 ITERATIONS_PER_VU=5 FILE_SIZE=1M MAX_VUS=2 MAX_UPLOAD_BYTES=10M MAX_RUNTIME=10m
make upload-capacity FILE_SIZE=1M MAX_VUS=8 MAX_UPLOAD_BYTES=160M MAX_RUNTIME=30m \
  BASELINE_ITERATIONS=10 CONFIRMATION_ITERATIONS=20
make shared-drive-ramp MAX_VUS=96 MAX_UPLOAD_BYTES=20M MAX_RUNTIME=10m
make org-drive-fanout CAPACITY_MAX=16 MAX_RUNTIME=30m   # ORG-DRIVE-FANOUT.md
export MIXED_DOMAIN=<tree prefix>.<domain> SEED_MANIFEST=~/private/<tree prefix>-fixture.json
make mixed-capacity CAPACITY_MAX=16 MAX_VUS=129 MAX_REQUESTS=8000 MAX_UPLOAD_BYTES=4M MAX_RUNTIME=20m
make campaign CAMPAIGN=capacity   # all four, with the budgets in campaigns/capacity.sh
```

The [load test dashboard](CAPACITY-OVERVIEW.md) uses **Environment → Mode → Run**
and three native tabs: **Overview**, **Workload**, **Infrastructure**. Size and
upload target describe the run. Inspecting a tested point carries its execution
and recorded window into the workload and infrastructure views. Producer links
open the same dashboard over each run's UTC window.

On the load VM, provision the private read-only Flux datasource **DEV InfluxDB**
with UID `dev-influxdb`. The Infrastructure tab then shows DEV HTTP, process,
workers, host, CouchDB and Redis measurements at 60-second sampling. These are
shared host metrics, not isolated organization usage. Without that datasource,
the tab reports DEV infrastructure unavailable. Set `DEV_MONITORING_URL` if an
additional platform monitoring link is useful in reports and summaries.

A shared DEV campaign (`LOAD_TARGET=dev`) takes the lock once and holds it
through every step and its cleanup; its steps run on the DEV targets and reuse
that lock, and the summary classifies them like local steps. With
`DEV_MONITORING_URL` set, the summary also links the DEV dashboard over each
step's time range. A step whose producer exits non-zero skips the remaining
steps; completed capacity brackets and lower bounds continue to the next one.

Each k6 run gets a fresh 30-minute instance token and is stopped after 20
minutes. No local Stack is started.

One run at a time: DEV campaigns hold `DEV_LOCK_FILE`
(`/tmp/cozy-load-shared-dev.lock`) through k6, verification and cleanup, and
exit 2 while another run holds it. Use the same path from every checkout, and
mount it into any container that starts DEV work.

A free lock does not prove DEV is idle: a killed runner can leave its k6
container running. Before its first point, a DEV campaign stops any k6
container whose target is a manifest instance, then deletes every
`twake-load-*` directory in the instance root, the test drive (for the ramp)
and the trash, so a killed run is cleaned up by the next one. When Docker cannot list or inspect its k6
containers, the campaign exits 2 before touching any data. The report also records the k6 containers it
stopped and the DEV monitoring link (`DEV_MONITORING_URL`, else `unknown`).

## Adding another scenario

We already use k6 scenarios, executors, `setup()` and `teardown()`, checks,
thresholds, custom metrics, tags, test aborts, and Prometheus output. We do
not need every k6 feature, but we should use a native k6 feature whenever it
fits the test.

Start with the question we want to answer:

| Question | k6 executor |
| --- | --- |
| Run an exact amount of work per user | `per-vu-iterations` |
| Keep a fixed number of active users | `constant-vus` |
| Increase active users over time | `ramping-vus` |
| Send work at a fixed rate | `constant-arrival-rate` |
| Increase the request rate until it fails | `ramping-arrival-rate` |
| Run one small smoke check | `shared-iterations` |

See the k6 docs for
[executors](https://grafana.com/docs/k6/latest/using-k6/scenarios/executors/),
[the test lifecycle](https://grafana.com/docs/k6/latest/using-k6/test-lifecycle/),
and
[open and closed workload models](https://grafana.com/docs/k6/latest/using-k6/scenarios/concepts/open-vs-closed/).

When adding a scenario:

1. Write down the question, result unit, and pass conditions first.
2. Change one main dimension at a time.
3. Keep the workload, HTTP calls, checks, thresholds, and tags in k6.
4. Move reusable k6 code to `src/lib/`.
5. Use Node only for file generation, local setup, or decisions between runs.
6. Give every run its own data and always clean it up.
7. Keep Make targets small. They should pass settings and start the right tool.
8. Reuse the upload and validation helpers for personal drive, shared drive,
   and S3 when the Cozy HTTP API is the same.

The next test families should stay separate:

- exact-work and fixed-duration upload concurrency, which we have now;
- shared-drive uploads from several member instances;
- upload throughput in uploads per second;
- mixed read and write user flows;
- mixed-workload and multi-instance soak tests;
- data-scale setup for users, organizations, shares, and files;
- membership change load.

This follows [issue #4864](https://github.com/linagora/cozy-stack/issues/4864)
without making one scenario change everything at once.

### Checking changes

| Change | Run |
| --- | --- |
| Node helper code | `npm test` and `make typecheck` |
| Compose settings | `make config` and `make doctor` |
| k6 scenario or request code | `make typecheck` and the matching mock smoke test |
| Cozy API flow | `make cozy-prepare` then `make upload-regression FILE_SIZE=1K VUS=1` |
| Load model or limits | A small trial, then the dedicated load environment |

`make node-checks` runs `npm ci`, the tests and the type checks in the Node
image Compose uses, on a copy of the sources: the checkout, its `node_modules`
and `.env` stay untouched, and the container is removed. Use it wherever the
host Node is too old, for example on the load VM. `compose.yaml` pins that image
by digest (`x-node-image`), since a cached tag can hold an unsupported Node; to
refresh it, pull `node:22-alpine`, check `node --version`, and update the digest.

The `Load harness` workflow runs `make node-checks`, `make config` and
`make upload-smoke` for changes under `tests/load`, and keeps the results and
Compose logs when it fails. It needs no credentials and never contacts DEV.

The mock checks our test code and API flow. It cannot give us a Cozy capacity
number. The bundled Cozy stack is useful for integration testing, but it
shares the machine with k6, so it is not a production benchmark either.

Real campaigns run manually from the dedicated load-test environment, not
from GitHub Actions. Keep the report, dashboard time range, target version,
and target configuration together when sharing a result.

### Known follow-up

The capacity script currently reads k6's generic `--summary-export` file.
k6 now recommends
[`handleSummary()`](https://grafana.com/docs/k6/latest/results-output/end-of-test/custom-summary/)
for machine-readable results. Later, we should use it to write a small result
format owned by this suite.

## Commands

```console
$ make install
$ make typecheck
$ make doctor
$ make config
$ make pull
$ make up
$ make upload-binaries
$ make upload-smoke
$ make cozy-up
$ make cozy-prepare
$ make upload-regression FILE_SIZE=1K VUS=1
$ make upload-capacity FILE_SIZE=1K MAX_VUS=8
$ make cozy-shared-drive-prepare
$ make shared-drive-ramp
$ make campaign CAMPAIGN=smoke
$ make cozy-status
$ make cozy-logs
$ make cozy-down
$ make upload BASE_URL=https://target.example FILE_SIZE=1M VUS=10
$ make upload-capacity BASE_URL=https://target.example FILE_SIZE=10M
$ make status
$ make logs
$ make down
```

`make down` preserves the Prometheus and Grafana volumes. Every run writes a
JSON summary under `data/results/` and tags Prometheus metrics with campaign,
execution, file-size, concurrency, phase, and topology identifiers.

k6 drops its `url` system tag (`K6_SYSTEM_TAGS` in `compose.yaml`): upload
names and changes sequence tokens would otherwise create new Prometheus series
for every request. Requests are identified by their fixed `name` tag instead,
such as `POST /files/:dir-id`.

## Deployment

For a shared host, create `.env` with a strong Grafana password and set the
published interface, port, data retention, and restart policy explicitly:

```dotenv
GRAFANA_ADMIN_USER=admin
GRAFANA_ADMIN_PASSWORD=replace-with-a-strong-password
GRAFANA_BIND_ADDRESS=0.0.0.0
GRAFANA_PORT=80
PROMETHEUS_RETENTION_SIZE=20GB
RESTART_POLICY=unless-stopped
```

Grafana authentication does not encrypt traffic. Put the service behind TLS
before exposing credentials or sensitive test results on an untrusted
network.

## Components

- Node.js 22.23.4 or newer with TypeScript and `tsx` for local tooling
- CouchDB 3.3.3, Redis 7.2, and RabbitMQ 3.13 in the optional `cozy` profile
- cozy-stack built from the current checkout in development mode
- `grafana/k6:2.2.0`
- `prom/prometheus:v3.14.0`
- `grafana/grafana:13.2.0`
- `node:22-alpine` for the streaming local mock
