# Twake Drive load-test harness

This directory contains the first infrastructure milestone for
[issue #4864](https://github.com/linagora/cozy-stack/issues/4864). It runs k6
as an on-demand container, stores metrics in Prometheus, and provisions
Grafana dashboards.

See [Testing architecture](../../docs/testing.md) for lifecycle boundaries,
correctness and result contracts, and the planned bounded shared DEV rollout.

For classified workload limits, tested bounds and fanout readiness evidence, see
[Stack Capacity Overview](CAPACITY-OVERVIEW.md). The existing diagnostic dashboards remain available.

The harness includes authenticated exact-work and fixed-duration file-upload
workloads for personal drives, shared-drive activity and upload workloads, and an
adaptive search for the maximum upload concurrency of one Cozy instance.
See [CAPACITY.md](./CAPACITY.md) for the operational capacity model and pass criteria.
See [SEEDING.md](./SEEDING.md) for API-only fixtures and
[SNAPSHOTS.md](./SNAPSHOTS.md) for isolated Compose snapshot/restore/discard commands.
See [ORG-DRIVE-FANOUT.md](./ORG-DRIVE-FANOUT.md) for native group attach/remove
convergence. Upstream B2B membership events and broader file workloads remain
separate future scenarios.

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
an instance and OAuth client, and run one real upload lifecycle:

```console
$ make cozy-upload FILE_SIZE=1K VUS=1
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
target and token to the ignored local `.env` without printing the token.

After the lifecycle check passes, run a bounded capacity search against the
real stack:

```console
$ make cozy-capacity FILE_SIZE=1K MAX_VUS=8
```

Or keep a known concurrency active for a fixed time:

```console
$ make cozy-upload-soak FILE_SIZE=1M VUS=8 DURATION=10m
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

Use a soak run to find out whether a concurrency level stays healthy over
time. This keeps 10 workflows active for 30 minutes and runs as many complete
upload-and-check iterations as they can finish:

```console
$ make upload-soak BASE_URL=http://cozy.localhost:8080 FILE_SIZE=1M VUS=10 DURATION=30m
```

The soak run uses k6's `constant-vus` executor. When `DURATION` ends, it stops
starting iterations and gives active upload-and-check workflows up to
`GRACEFUL_STOP` to finish; the default is 30 minutes so large uploads are not
cut off during validation. The regular `upload` target continues to use
`per-vu-iterations`. Its `MAX_DURATION` is only a safety deadline, not a way to
hold load for a fixed time.

Uploaded files stay in the execution's temporary directory until teardown.
Before a long soak, use a short run to estimate upload throughput and check
that the target has enough free storage for `throughput × file size ×
duration`. A normal teardown deletes the complete temporary directory.

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

> [!WARNING]
> **Owner-only workload**: The `shared-drive-activity.ts` scenario runs against the drive owner's instance and credentials. It does not exercise recipient/member sync paths, cross-instance replication, or multi-member permission propagation. Testing multi-instance recipient workflows remains a separate future scenario.

Shared drives can be tested in two modes:

1. **Combined activity scenario (`shared-drive-activity.ts`)**: exercises concurrent collaborative activity combining:
   - **File writers**: concurrent uploads into the shared drive.
   - **WebSocket readers**: persistent connections to `/sharings/drives/:id/realtime` listening for real-time `CREATED`/`UPDATED` events.
   - **Changes feed readers**: periodic polling of `/sharings/drives/:id/_changes`.

   To prepare the shared drive on a local Cozy instance and run the activity scenario:
   ```console
   $ make cozy-shared-drive-prepare
   $ make cozy-shared-drive-load DURATION=1m WRITER_VUS=3 WS_READER_VUS=5 CHANGES_READER_VUS=3
   ```

   To find the highest activity level one instance sustains, run the same
   mix as a ramp in a single k6 run:

   ```console
   $ make cozy-shared-drive-ramp RAMP_LEVELS=1,2,4,8,16 RAMP_STEP_DURATION=1m
   ```

   Level L runs L writers, 3L WebSocket readers, and 2L changes readers
   (`RAMP_WRITERS_PER_LEVEL`, `RAMP_WS_READERS_PER_LEVEL`,
   `RAMP_CHANGES_READERS_PER_LEVEL`). Each level gets a 10s transition and
   a hold of `RAMP_STEP_DURATION`. Samples taken during the hold are tagged
   with `level`; transitions are tagged apart and never judged.

   After the run, `handleSummary()` judges each level on errors and
   missing events:

   - the upload, changes, WebSocket handshake, and WebSocket auth success
     rates must reach `MIN_SUCCESS_RATE` (default 0.99);
   - WebSocket readers must receive a `CREATED` event, and changes readers
     a change, for every successful upload of the level. k6 cannot match
     events to uploads across VUs, so this compares totals: received
     divided by uploads × readers must reach `MIN_DELIVERY_RATIO`
     (default 0.9), which absorbs the reconnect at each level boundary.

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

   A failing completed level is a result, not an error: k6 still exits with 0.
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

Or run all six sizes sequentially:

```console
$ make upload-capacity-all
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
checksum mismatch aborts the k6 point and fails it; the search then continues
below that level. Latency is not judged by default, because a limit derived
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
a different checksum. A mismatch still lets the search continue and report a
capacity, but it is a correctness failure, never a measurement.

Grafana can filter k6 metrics by campaign, execution ID, file size,
concurrency, and phase. A campaign groups every capacity-search point. An
execution ID identifies one k6 process at one concurrency and attempt; for a
standalone upload or soak run, the campaign and execution ID are the same.
Make generates a UTC execution ID by default. Pass `EXECUTION_ID=name` for a
standalone run or `CAPACITY_RUN_ID=name` for a capacity campaign when you want
a stable name in Grafana.

The dashboard keeps successful and failed upload p95 values separate and uses
seconds, which is the Prometheus base unit used by k6 remote write. It also
shows failed-upload totals, a failure-rate timeline, and failures grouped by
execution, concurrency, and HTTP status. The HTTP status is available in k6
metrics; the server-side error message still comes from stack logs.

Use the Stack source selector to inspect either the bundled Compose target or
a stack started directly on the host. The stack panels show scrape health,
request rate, error rate, server latency, CPU, memory, goroutines, file
descriptor usage, garbage collection, and upload-related worker queues.

For the bundled stack, Prometheus also scrapes CouchDB
(`/_node/_local/_prometheus`) and RabbitMQ (port 15692, including per-queue
depth). The overview's last row shows CouchDB requests, request time, error
responses, open databases and files, and RabbitMQ memory and queued messages.

The **Twake Drive shared-drive activity** dashboard follows the ramp level by
level: success rates, WebSocket events and changes delivered per upload level,
upload, changes, and WebSocket connect p95, VUs, and stack load.

The stack HTTP metrics are labelled by method and status code, but not by
route, file size, instance, or k6 campaign. During a controlled upload campaign,
`POST` responses with status `201` are mostly file uploads, with one directory
creation per test point. Use the shared dashboard time range to correlate stack
and k6 metrics; use the k6 upload metrics for exact scenario attribution.

## Campaigns

A campaign runs several measurements in a row and keeps everything about the
run together:

```console
$ make campaign CAMPAIGN=quick
$ make campaign CAMPAIGN=breaking-point
```

`campaigns/quick.sh` measures 1 MiB upload capacity on a personal drive and a
shared drive, then runs the shared-drive ramp to level 16, in about 30 to 60
minutes. `campaigns/breaking-point.sh` pushes every suite until errors, lost
data, or missing events, with uploads slower than 5 minutes counted as errors;
it takes several hours. A definition is a `campaign()` function calling
`upload_capacity`, `shared_drive_prepare`, `shared_drive_ramp`, or `run_step`.

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

## Shared DEV

`make dev-upload` runs the exact-work upload scenario against one instance of
the Cloudery fixture ([SEEDING.md](SEEDING.md#shared-dev-through-the-cloudery)).
Run it only from the designated load VM:

```sh
export SEED_CLOUDERY_TOKEN_FILE=~/private/cloudery-token.json
export SEED_CLOUDERY_MANIFEST=~/private/<prefix>-cloudery-fixture.json
make dev-upload DEV_INSTANCE=<prefix>01.<domain>                                 # 1 x 1 x 1 KiB
make dev-upload DEV_INSTANCE=<prefix>01.<domain> VUS=2 ITERATIONS_PER_VU=5 FILE_SIZE=1M
```

The instance must be in the manifest, with the same UUID on the Stack. Every
request uses a token minted for that instance. Limits are fixed and checked
before any request: at most 2 workers, 5 uploads each and 1 MiB per upload;
60 s per request, 10 minutes for k6, and 5 minutes for each cleanup, listing
included. No local Stack is started.

One run at a time: `make dev-upload` holds `DEV_LOCK_FILE`
(`/tmp/cozy-load-shared-dev.lock`) through k6, verification and cleanup, and
exits 2 while another run holds it. Use the same path from every checkout, and
mount it into any container that starts DEV work.

A free lock does not prove DEV is idle: a killed runner can leave its k6
container running. Before cleanup, the runner stops any k6 container whose
target is a manifest instance. Before and after k6, it deletes every
`twake-load-*` directory in the instance root and trash, so a killed run is
cleaned up by the next one. A content mismatch exits 1 even when cleanup also
failed. The report, `data/results/<EXECUTION_ID>-dev-upload.json`, records the
outcome (0, 1 or 2 as for campaigns), the k6 containers it stopped, parameters, times, the Stack, harness, Node and
k6 image versions, monitoring links (`DEV_MONITORING_URL`, else `unknown`) and
the identifiers of any data it could not delete.

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
| Cozy API flow | `make cozy-upload FILE_SIZE=1K VUS=1` |
| Load model or limits | A small trial, then the dedicated load environment |

To run the Node checks on the same runtime as Compose, for example on the load
VM, whose host Node is too old, use `node:22-alpine` from `tests/load`. Pull the
tag first, because a cached tag can still hold an unsupported Node. Keep the
printed version and digest with the results:

```console
$ docker pull node:22-alpine
$ docker image inspect node:22-alpine --format '{{index .RepoDigests 0}}'
$ docker run --rm -v "$PWD":/app -v /app/node_modules -w /app node:22-alpine \
    sh -c 'apk add --no-cache make bash && node --version && npm ci && npm test && npm run typecheck'
```

The anonymous `/app/node_modules` volume keeps the host install untouched.

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
$ make cozy-upload FILE_SIZE=1K VUS=1
$ make cozy-upload-soak FILE_SIZE=1M VUS=8 DURATION=10m
$ make cozy-capacity FILE_SIZE=1K MAX_VUS=8
$ make cozy-shared-drive-prepare
$ make cozy-shared-drive-load DURATION=1m
$ make cozy-shared-drive-ramp
$ make campaign CAMPAIGN=quick
$ make cozy-status
$ make cozy-logs
$ make cozy-down
$ make upload BASE_URL=https://target.example FILE_SIZE=1M VUS=10
$ make upload-soak BASE_URL=https://target.example FILE_SIZE=1M VUS=10 DURATION=30m
$ make upload-capacity BASE_URL=https://target.example FILE_SIZE=10M
$ make upload-capacity-all BASE_URL=https://target.example
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
