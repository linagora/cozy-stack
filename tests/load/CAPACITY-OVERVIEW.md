# Stack Capacity Overview

A native Grafana dashboard in **Twake Drive Load Tests**, alongside the existing
k6 and shared-drive diagnostic dashboards. No separate frontend or iframe.

```sh
npm ci
make observability-up
make capacity-dashboard-url
```

The internal Node service reads `data/results` read-only and exposes Prometheus
metrics. Two scripts handle this: `capacity-metrics.ts` maps producer fields;
`serve-capacity-results.ts` handles bounded file reads and GET-only HTTP.
Neither script runs workloads or modifies Stack instances.

## Producer results are authoritative

The upload, activity and fanout scenarios decide completeness, thresholds,
confirmation, cleanup and final capacity. The adapter copies their classifications
and allowlisted numeric diagnostics; it does not replay those decisions.

- **Uploads:** recorded upload-worker capacity for one size, target and criterion;
  not measured simultaneous HTTP requests or unique users.
- **Activity:** the recorded mix level. Writers, WS readers and changes readers
  share one identity, so distinct shared-drive user capacity is **not measured**.
- **Member batch:** distinct recipients from one group-add operation. The producer
  requires all ready/access checks and verified access denial after removal.

`= N`, `>= N` and `[N, M)` are different conclusions. A bracket does not prove
untested intermediate levels pass. An inconclusive producer result stays
inconclusive even if an earlier point passed. Explicit zero differs from absent.
Legacy activity reports with `exact` or no final status remain inconclusive;
the separate scenario-correctness changes provide explicit brackets and status.

Each result also carries the producer's own `load_target` (`local`, `dev`, or
`unknown` for older reports), `mode` and `outcome` (0, 1 or 2). A regression run
shows passed, failed or inconclusive and never a capacity. When a runner could not
verify a run, for example because cleanup failed, the result stays inconclusive
and any bound it recorded appears only as `evidence` (and
`capacity_evidence_lower`/`_upper`), never as the capacity.

Criteria, owner readiness, recipient-side access and removal remain separate
measurements. Unknown values are omitted, not invented. File mtime is the report
update time, not scrape freshness. Conditions absent from reports stay not recorded.

## Publishing reports

Upload capacity, upload regression and activity ramp already write JSON to
`data/results`. Outside a campaign, a run's time range comes from the times its
report records.
`make org-drive-fanout` defaults `FANOUT_REPORT` to
`data/results/<EXECUTION_ID>-org-drive-fanout.json`; explicit paths still work.
Configure the disposable organization and routing per [ORG-DRIVE-FANOUT.md](ORG-DRIVE-FANOUT.md).

Campaign definitions can use `org_drive_fanout org-members FANOUT_LEVELS=10,50,75`.
The default quick campaign does not create a trust context or enable fanout.
Campaign `steps.tsv` supplies exact run membership and diagnostic links;
standalone reports stay separate. Select one campaign rather than aggregating
incompatible workloads. A single run does not establish repeatability.

For remote targets, transfer only the intended completed reports, preserving mtime.
Do not mount private logs, environment secrets or fixture snapshots as result data.

## Operation

The reader bounds files, bytes, entries and points, rejects unsafe links and
ambiguous identities, and never exposes raw JSON or private fields over HTTP.
The container uses `HOST_UID`/`HOST_GID`, read-only mounts and no host-published
port. Keep file permissions readable by that identity.

Prometheus scrapes `capacity-results:9465`. After changing its configuration,
verify the loaded scrape target; provisioning source alone does not prove it ran.
Existing Stack/CouchDB/RabbitMQ diagnostics remain unchanged.
