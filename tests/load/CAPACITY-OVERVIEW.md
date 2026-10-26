# Load test dashboard

The native Grafana dashboard **Load test results** has three tabs. It serves
local and DEV runs, in regression and capacity mode, using the same controls.

```sh
make observability-up
make capacity-dashboard-url
```

## Choose a run

The top bar contains **Environment → Mode → Run**. File size, personal/shared
upload target and campaign describe the selected report; they are not filters.
The compact context and **Run time range** controls appear on every tab.
**Run + context** clears the point selection and restores the recorded UTC
window with a one-minute margin. **Run only** uses the recorded start/end without margins.
Producer and campaign links already include the window.

A dropdown changes the selected report, not Grafana's time picker. Click
**Run + context** after choosing a different run. Report facts use **Load
reports**, a read-only datasource evaluated independently of telemetry zoom;
the catalogue, verdicts and point evidence stay available when viewing a
window before the completed report's first scrape. Report mtime never defines test time.

If the plotted window excludes the selected workload or point, a notice opens
the recorded run with one click. Workload timelines stay hidden until the
windows overlap; an unrelated window is not labelled as unrecorded test data.

| Tab | Purpose |
| --- | --- |
| Overview | Execution, correctness, cleanup and performance; completed work, tested points and the stopping reason |
| Workload | Actual request rates, latency and selected point evidence |
| Infrastructure | Local Prometheus or optional DEV InfluxDB over the same window |

Click **Inspect** in a tested-point table. Its run-unique point identity and
recorded timestamps set the point and time range. **Run + context** clears the
selection and restores the full recorded run. On Workload and Infrastructure,
expand **Point: … · change** to choose another point directly. The controls offer
the point with context or without margins; older reports explicitly fall back to
the run window when point time is unavailable.
Concurrency, phase and execution ID follow the point; raw IDs and numeric
report fields stay under **Advanced diagnostics**. Selected-point work summaries
show counts, payload and final latency only when the necessary evidence exists;
partial evidence cannot turn an unknown failure count into zero. Upload reports record each
attempt's execution ID and start/end. Older upload reports can identify their
execution through the known summary filename; without point times, links use
the recorded whole-run window. Activity and fan-out also retain the run window
until their producers record individual point timestamps.

Native tab switches preserve the selected point and plotted time, so Workload
and Infrastructure can be compared directly. Overview's recorded result and
capacity evidence cover the whole run. Use **Run + context** to reset the plotted
window and point. These explicit native controls replace the HTML prototype's
automatic navigation resets.

## Read the evidence

Producer reports are authoritative. A valid `cozy-load-result/1` envelope owns
the four verdicts; an invalid envelope remains inconclusive. Reports predating
the envelope keep their legacy classification. UI adapters never upgrade an
unverified bound into a passing run.

- Regression checks a fixed workload. Latency is observational unless that run
  declares a gate. It never claims capacity.
- Capacity keeps exact values, lower bounds and brackets apart. `>= 8` is a
  lower bound; it does not establish maximum capacity. Confirmation attempts
  remain distinct from discovery at the same numeric load.
- Completed uploads and bytes require recorded successful work. Unknown counts
  remain unavailable. The final p95 headline is the highest final per-attempt
  p95, not a pooled percentile. Whole-attempt throughput includes setup,
  readback and cleanup; request throughput is shown separately on Workload.
- Activity levels describe writer/WS/changes roles using one identity. They
  do not measure distinct users. Missing content verification stays unverified.
- Fan-out keeps recipient access, content readback, owner observations and
  revocation separate. Intentional local fixture retention is not cleanup.

The reader currently accepts upload regression/capacity, shared-drive ramp and
organization fan-out reports. Standalone soak and fixed activity do not yet
publish the common result contract. They must not appear as completed runs
without that evidence. Fan-out access/revocation curves from the mockup remain
unavailable until its producer records timed observations; scalar proofs remain
visible. Synthetic mock values are not production measurements.

**Evidence** includes recorded work and content checks. **Download evidence**
exports `cozy-load-evidence/1`, the same allowlisted facts used by the dashboard,
without raw report errors, tokens or target URLs. Keep the original producer
report separately. Advanced diagnostics include Stack/harness revisions,
profile, topology, dataset and recorded budgets.

Upload phase bands and configured worker counts use the recorded attempt times.
They include short attempts that finish between k6 samples. They describe the
configured load, not measured busy workers. Reports without attempt times cannot
provide these bands.

Latency and request rates stop at each recorded attempt's end. The hidden
`report_at` variable pins report metrics to their latest scrape, independently
of chart time, so early-point links still work. Latency has three curves: p50,
p95 and p99. A value is displayed only when exactly one attempt supplies it;
simultaneous values remain a gap rather than being averaged. Fine query steps
preserve short attempts, but do not increase the source's collection frequency.
Legacy reports without attempt times use only k6 samples less than five seconds
old. Their exact boundaries remain unknown.

Activity summaries show completed uploads, peak activity level, delivery ratios
for the last selected level and worst upload p95. Fan-out summaries show the
largest batch, recipient-side access and revocation checks for the last selected
batch, and the number of tested batches. Missing recipient observations remain
unavailable even when the owner reports success. Overview covers the whole run;
Workload follows the selected points.

A configured p95 criterion becomes a
native threshold line; no line is inferred for observational runs. Capacity
curves distinguish cleanup-unverified/retained point evidence in amber and do
not join it into a verified-looking line. Whole-attempt throughput and latency
use their own axis units.

## Infrastructure

Local process CPU is in cores; RSS and heap remain separate. Peak cards refer
to the plotted window. Empty scrapes are unavailable. A zero error rate is
shown only when the request series exists. Zero RabbitMQ queues is distinct
from a missing scrape.

For DEV, provision a private read-only InfluxDB datasource named
**DEV InfluxDB**, with UID **dev-influxdb**, Flux organization and default bucket
set by the platform owner. One **Hosts** control scopes DEV Stack, worker and
host-exporter panels. Its All choice expands the recorded Stack host inventory;
dependency nodes retain their own scope. No token or host inventory belongs in dashboard JSON.
Without this datasource, the tab explicitly reports DEV infrastructure as
unavailable. No central Grafana change is required.

DEV panels show shared host activity, not isolated test-organization usage.
Peak process CPU/RSS combine selected Stack hosts at each sampled time before
taking the peak; host CPU and queue peaks still identify the worst selected host.
Samples are discrete at 60 seconds; gaps are not filled. HTTP p99 stays per
host and is never pooled. The source timestamp is the last sample inside the
selected window, not Grafana refresh time. DEV S3 and RabbitMQ telemetry remain
unavailable. DEV heap, Redis blocked clients and HAProxy queue are not supplied
by this mapping. Local and DEV workload telemetry still comes from runner Prometheus.
Missing datasource configuration is shown explicitly. A configured source with
query/connectivity errors shows Grafana's error state; empty series remain
unavailable rather than becoming a healthy zero.

## Operation

Grafana 13.2 provisions the v2 dashboard resource from
`config/grafana/dashboards/capacity-overview.json`; native tabs and conditional
sections require the new layouts feature. Earlier dashboard UIDs contain
compatibility links to these tabs rather than duplicate query implementations.

`capacity-metrics.ts` projects bounded, allowlisted report fields.
`serve-capacity-results.ts` reads result files read-only and exposes Prometheus
metrics, a sanitized evidence download and an allowlisted read-only Prometheus
metadata proxy. Only report queries use that proxy; k6 and infrastructure retain
their actual selected times. Neither service starts workloads nor changes Stack instances. Keep the
exporter's imported modules mounted when updating it, and verify its scrape
health after restart.

Persist producer reports and k6 summaries independently of Prometheus retention.
Transfer only intended reports; never mount credentials, private logs or fixture
snapshots into the results reader. A dashboard link is not an archive.
