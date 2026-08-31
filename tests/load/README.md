# Twake Drive load-test harness

This directory contains the first infrastructure milestone for
[issue #4864](https://github.com/linagora/cozy-stack/issues/4864). It runs k6
as an on-demand container, stores metrics in Prometheus, and provisions a
Grafana dashboard.

The current scenarios establish the hello-world baseline. Authentication,
organization membership, sharing, and file workloads will be added as the
capacity model is implemented.

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

## Commands

```console
$ make doctor
$ make config
$ make pull
$ make up
$ make smoke
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
