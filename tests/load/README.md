# Twake Drive load-test harness

This directory contains the first infrastructure milestone for
[issue #4864](https://github.com/linagora/cozy-stack/issues/4864). It runs k6
as an on-demand container, stores metrics in Prometheus, and provisions a
Grafana dashboard.

The harness includes an authenticated concurrent file-upload workload for
personal drives, a work-in-progress shared-drive mode, and an adaptive search
for the maximum sustained upload concurrency of one Cozy instance.
Organization membership and other file workloads remain separate future
scenarios.

## How it is built

The short version is:

- k6 sends the load and decides whether one test run passed.
- Node prepares data and coordinates several k6 runs.
- Make and Docker Compose connect the commands and services.
- Prometheus and Grafana store and display the results.

The Node code is not another load-testing framework. There is no Jest
dependency here either. `npm test` uses Node's built-in
`node:test` runner to check helper code. All measured HTTP traffic
comes from k6.

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
├── config/
│   ├── cozy/
│   ├── prometheus/
│   └── grafana/
├── data/
│   ├── upload-binaries/ generated input files, ignored by Git
│   └── results/         summaries and capacity reports, ignored by Git
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
- Node.js 22 or newer with npm

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

Each workflow also includes the consistency download, so `VUS=10` does not
mean that 10 upload bodies are always in flight.

Each k6 point creates a uniquely named directory before starting its VUs. All
VUs in that point share the same Cozy instance, OAuth token, and directory.
After measurement, teardown moves the directory to the trash and permanently
deletes it. A setup or cleanup failure is reported as an infrastructure
failure rather than a capacity result.

Before each request, the scenario changes 256 bytes in the selected in-memory
binary. The mutation includes the test ID, VU, iteration, time, and a random
value, so every uploaded file has distinct content without regenerating the
whole binary. The base binary on disk remains unchanged.

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

### Shared-drive upload mode (work in progress)

`concurrent-uploads.ts` has a shared-drive target mode; there is not a separate
shared-drive scenario yet.

> **Work in progress:** the current mode sends every upload from one configured
> instance and access token. It is useful for checking the shared-drive API
> routes, but it does not yet represent concurrent activity from the different
> instances with which a drive is shared.

The complete scenario will prepare an owner and several recipient instances,
share one drive with them, and distribute concurrent uploads across their
individual access tokens. Until that lifecycle is implemented, do not treat
results from this mode as shared-drive capacity results.

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

S3 remains an implementation detail of the target stack. To measure the S3
backend, deploy an S3-enabled stack, configure its `fs.url`, and run this same
HTTP scenario against it. S3 credentials do not belong on the load generator.

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

1. Measures 50 uploads at 1 VU to establish the same-size p95 baseline.
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

A point passes only when upload success is at least 99%, no iterations are
dropped, and upload p95 is within the configured limit. By default the limit is
twice the 1-VU baseline. For a repeatable load-environment contract, set an
absolute `P95_LIMIT_MS`; it replaces the multiplier while the baseline remains
in the report for comparison.

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
success rate, dropped and completed iterations, cleanup result, and the k6
summary path. An `exact` result has a measured failing level above it. A
`lower_bound` result is displayed as `maximum >= MAX_VUS`, because the
configured cap passed and no higher level was attempted.

Grafana can filter k6 metrics by campaign, test ID, file size, concurrency,
and phase. Use the Stack source selector to inspect either the bundled Compose
target or a stack started directly on the host. The stack panels show scrape
health, request rate, error rate, server latency, CPU, memory, goroutines, file
descriptor usage, garbage collection, and upload-related worker queues.

The stack HTTP metrics are labelled by method and status code, but not by
route, file size, instance, or k6 campaign. During a controlled upload campaign,
`POST` responses with status `201` are mostly file uploads, with one directory
creation per test point. Use the shared dashboard time range to correlate stack
and k6 metrics; use the k6 upload metrics for exact scenario attribution.

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

- exact upload concurrency, which we have now;
- shared-drive uploads from several member instances;
- upload throughput in uploads per second;
- mixed read and write user flows;
- long soak tests;
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
$ make cozy-capacity FILE_SIZE=1K MAX_VUS=8
$ make cozy-status
$ make cozy-logs
$ make cozy-down
$ make upload BASE_URL=https://target.example FILE_SIZE=1M VUS=10
$ make upload-capacity BASE_URL=https://target.example FILE_SIZE=10M
$ make upload-capacity-all BASE_URL=https://target.example
$ make status
$ make logs
$ make down
```

`make down` preserves the Prometheus and Grafana volumes. Every run writes a
JSON summary under `data/results/` and tags Prometheus metrics with campaign,
test, file-size, concurrency, and phase identifiers.

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

- Node.js 22 with TypeScript and `tsx` for local tooling
- CouchDB 3.3.3, Redis 7.2, and RabbitMQ 3.13 in the optional `cozy` profile
- cozy-stack built from the current checkout in development mode
- `grafana/k6:2.2.0`
- `prom/prometheus:v3.14.0`
- `grafana/grafana:13.2.0`
- `node:22-alpine` for the streaming local mock
