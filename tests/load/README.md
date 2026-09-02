# Twake Drive load-test harness

This directory contains the first infrastructure milestone for
[issue #4864](https://github.com/linagora/cozy-stack/issues/4864). It runs k6
as an on-demand container, stores metrics in Prometheus, and provisions a
Grafana dashboard.

The harness includes the hello-world baseline, an authenticated concurrent
file-upload workload, and an adaptive search for the maximum sustained upload
concurrency of one Cozy instance. Organization membership, sharing, and other
file workloads remain separate future scenarios.

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
$ make smoke
$ make upload-smoke
$ make dashboard-url
```

These smoke targets use the bundled nginx mock to validate the harness without
starting a Cozy stack.

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

`make cozy-provision` creates `load.localhost:8080` when needed, reuses its
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
binary fixtures. Fixtures are generated from random data once and kept under
the `fixtures/` directory, while generated binary files remain ignored. A run
generates only its selected size; use `make fixtures` to prepare the complete
set in advance.

The TypeScript fixture generator runs through `tsx` and streams random bytes
to disk with a bounded 1 MiB buffer. k6 loads the selected file during its
initialization context and keeps upload data in memory, while
`fixtures/upload-data.ts` owns loading and per-upload mutation for reuse by
multiple scenarios.

Run one upload per virtual user against a local stack:

```console
$ make upload BASE_URL=http://cozy.localhost:8080 FILE_SIZE=1M VUS=10
```

`VUS` is the requested upload concurrency. `ITERATIONS_PER_VU` defaults to
one, which keeps the uploaded volume predictable. For example, this runs 50
uploads in five waves of up to 10 concurrent requests:

```console
$ make upload BASE_URL=http://cozy.localhost:8080 FILE_SIZE=100K VUS=10 ITERATIONS_PER_VU=5
```

Each k6 point creates a uniquely named directory before starting its VUs. All
VUs in that point share the same Cozy instance, OAuth token, and directory.
After measurement, teardown moves the directory to the trash and permanently
deletes it. A setup or cleanup failure is reported as an infrastructure
failure rather than a capacity result.

Before each request, the scenario changes 256 bytes in the selected in-memory
fixture. The mutation includes the test ID, VU, iteration, time, and a random
value, so every uploaded file has distinct content without regenerating the
whole fixture. The base fixture on disk remains unchanged.

Run each size separately. According to the
[k6 guidance for large tests](https://grafana.com/docs/k6/latest/testing-guides/running-large-tests/#file-upload-considerations),
file uploads are copied for each VU and require significant memory. The
capacity coordinator applies disk and Docker-memory guards before generating
the selected fixture or starting k6.

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

1. Measures five uploads at 1 VU to establish the same-size p95 baseline.
2. Tries burst levels 2, 4, 8, and so on, with one upload per VU.
3. Retries a failing burst once and uses a third run when the first two
   disagree.
4. Binary-searches between the last passing and first failing levels.
5. Confirms the best candidate with three uploads per VU, searching downward
   again if sustained confirmation fails.

A point passes only when upload success is at least 99%, no iterations are
dropped, and upload p95 is at most twice the 1-VU baseline. Tune these public
parameters when the test contract changes:

```console
$ make upload-capacity FILE_SIZE=10M MAX_VUS=16 LATENCY_MULTIPLIER=2 MIN_SUCCESS_RATE=0.99 CONFIRMATION_ITERATIONS=3
```

Default search caps keep the load-generator footprint bounded:

| File size | Default `MAX_VUS` |
| --------- | ----------------: |
| `1K`      |               256 |
| `100K`    |               256 |
| `1M`      |               128 |
| `10M`     |                32 |
| `100M`    |                 8 |
| `1G`      |                 2 |

Before a campaign, the coordinator estimates k6 memory as
`1 GiB + 3 × file size × MAX_VUS`. It also accounts for the fixture and the
largest confirmation directory, and refuses to leave less than 5 GiB free on
the load-generator filesystem. `FORCE_CAPACITY_RUN=1` bypasses these guards;
use it only after checking Docker memory and both load-generator and Cozy
storage manually.

The aggregate report is written to
`results/<campaign>-upload-capacity.json`. It records every attempt, p95,
success rate, dropped and completed iterations, cleanup result, and the k6
summary path. An `exact` result has a measured failing level above it. A
`lower_bound` result is displayed as `maximum >= MAX_VUS`, because the
configured cap passed and no higher level was attempted.

Grafana can filter k6 metrics by campaign, test ID, file size, concurrency,
and phase. Its local cozy-stack panels use the bundled Compose target when that
profile is running and the `host.docker.internal:6060` target for a stack
started directly on the host.

## Commands

```console
$ make install
$ make typecheck
$ make doctor
$ make config
$ make pull
$ make up
$ make smoke
$ make fixtures
$ make upload-smoke
$ make cozy-up
$ make cozy-provision
$ make cozy-upload FILE_SIZE=1K VUS=1
$ make cozy-capacity FILE_SIZE=1K MAX_VUS=8
$ make cozy-status
$ make cozy-logs
$ make cozy-down
$ make upload BASE_URL=https://target.example FILE_SIZE=1M VUS=10
$ make upload-capacity BASE_URL=https://target.example FILE_SIZE=10M
$ make upload-capacity-all BASE_URL=https://target.example
$ make run BASE_URL=https://target.example VUS=10 DURATION=1m
$ make status
$ make logs
$ make down
```

`make down` preserves the Prometheus and Grafana volumes. Every run writes a
JSON summary under `results/` and tags Prometheus metrics with campaign, test,
file-size, concurrency, and phase identifiers.

Set `SCENARIO` to run another script below `scenarios/`:

```console
$ make run SCENARIO=http.ts BASE_URL=https://target.example
```

k6 2.2 runs the scenario `.ts` files directly. The fixture and capacity tools
run through `tsx`. Separate strict TypeScript configurations keep k6 globals
out of the Node tools and Node globals out of the k6 scenarios; `make
typecheck` validates both environments without emitting build artifacts.

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
- `nginx:1.29-alpine` for the local mock
