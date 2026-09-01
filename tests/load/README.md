# Twake Drive load-test harness

This directory contains the first infrastructure milestone for
[issue #4864](https://github.com/linagora/cozy-stack/issues/4864). It runs k6
as an on-demand container, stores metrics in Prometheus, and provisions a
Grafana dashboard.

The harness includes the hello-world baseline and an authenticated concurrent
file-upload workload. Organization membership, sharing, and other file
workloads will be added as the capacity model is implemented.

## Prerequisites

- Docker Engine with Docker Compose
- GNU Make

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

Grafana defaults to <http://localhost:3000/> with the local credentials
`admin` / `admin`. Copy `.env.example` to `.env` to change the credentials,
published port, retention size, or restart policy. Do not commit `.env`.

To exercise a Cozy stack running on the host, `cozy.localhost` and
`host.docker.internal` resolve to the Docker host from the k6 container:

```console
$ make run BASE_URL=http://cozy.localhost:8080 VUS=10 DURATION=1m
```

## Concurrent uploads

The upload workload supports `1K`, `100K`, `1M`, `10M`, `100M`, and `1G`
binary fixtures. Fixtures are generated from random data once and kept under
the `fixtures/` directory, while generated binary files remain ignored. A run
generates only its selected size; use `make fixtures` to prepare the complete
set in advance.

Fixture generation is a shell preparation step because k6 JavaScript reads
files during its initialization context and keeps upload data in memory. The
generator streams random bytes directly to disk, while
`fixtures/upload-data.js` owns loading and per-upload mutation for reuse by
multiple scenarios.

Add an OAuth token with `io.cozy.files` permission to the ignored `.env`:

```dotenv
COZY_ACCESS_TOKEN=replace-with-a-target-token
```

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

The scenario intentionally does not delete files during the measured run.
Use a dedicated test instance and remove `load-*.bin` files after the run.

Before each request, the scenario changes 256 bytes in the selected in-memory
fixture. The mutation includes the test ID, VU, iteration, time, and a random
value, so every uploaded file has distinct content without regenerating the
whole fixture. The base fixture on disk remains unchanged.

Run each size separately. According to the
[k6 guidance for large tests](https://grafana.com/docs/k6/latest/testing-guides/running-large-tests/#file-upload-considerations),
file uploads are copied for each VU and require significant memory. In
particular, raise the `1G` concurrency gradually while monitoring the load
generator's memory, CPU, and network utilization so the generator does not
become the bottleneck.

## Commands

```console
$ make doctor
$ make config
$ make pull
$ make up
$ make smoke
$ make fixtures
$ make upload-smoke
$ make upload BASE_URL=https://target.example FILE_SIZE=1M VUS=10
$ make run BASE_URL=https://target.example VUS=10 DURATION=1m
$ make status
$ make logs
$ make down
```

`make down` preserves the Prometheus and Grafana volumes. Every run writes a
JSON summary under `results/` and tags Prometheus metrics with a unique
`testid`.

Set `SCENARIO` to run another script below `scenarios/`:

```console
$ make run SCENARIO=http.js BASE_URL=https://target.example
```

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

- `grafana/k6:2.2.0`
- `prom/prometheus:v3.14.0`
- `grafana/grafana:13.2.0`
- `nginx:1.29-alpine` for the local mock
