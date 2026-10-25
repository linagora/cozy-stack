# Shared DEV runbook

How to run the shared DEV upload regression and record its baseline.
[README, Upload campaigns](README.md#upload-campaigns) describes the runner, its
budgets and the lock; [SEEDING.md](SEEDING.md#shared-dev-through-the-cloudery)
describes the fixture.

## Agreement

The DEV owner agrees the budgets, stop conditions and window before each
campaign, and can raise the budgets between campaigns. Current agreement
(2026-10-25):

| | |
| --- | --- |
| Runner | The designated load VM only; the Cloudery token and fixture manifest stay there |
| Target | One member instance of the Cloudery fixture |
| Smoke budgets | `MAX_VUS=1 MAX_UPLOAD_BYTES=1K MAX_RUNTIME=5m` |
| Regression budgets | `MAX_VUS=2 MAX_UPLOAD_BYTES=10M MAX_RUNTIME=10m` |
| Criteria | `MIN_SUCCESS_RATE=1`, `UPLOAD_TIMEOUT=60s`; no latency gate yet |

## Stop conditions

- The runner ends a run on a checksum mismatch or a failed upload (exit 1), and
  on a request past `UPLOAD_TIMEOUT`, the `MAX_RUNTIME` budget, an interruption
  or unverified cleanup (exit 2).
- Stop the sequence at the first non-zero exit. Rerun only once the report's
  `failure_reason` is understood and any data it names is gone.
- Interrupt a run (Ctrl-C) when the DEV owner asks or DEV monitoring shows
  trouble: the runner stops k6 and still cleans up.

## Procedure

1. Check the exact revision in the supported container
   ([README, Checking changes](README.md#checking-changes)) and keep the log.
2. Probe the instance's public `/version` five times as a background reference.
3. Run the smoke, then the regression workload three times, five minutes apart:

```sh
export SEED_CLOUDERY_TOKEN_FILE=~/private/cloudery-token.json
export SEED_CLOUDERY_MANIFEST=~/private/<prefix>-cloudery-fixture.json
export DEV_INSTANCE=<prefix>01.<domain> MIN_SUCCESS_RATE=1 UPLOAD_TIMEOUT=60s
curl -s -o /dev/null -w '%{http_code} %{time_total}\n' "https://$DEV_INSTANCE/version"
make dev-upload MAX_VUS=1 MAX_UPLOAD_BYTES=1K MAX_RUNTIME=5m
make dev-upload VUS=2 ITERATIONS_PER_VU=5 FILE_SIZE=1M MAX_VUS=2 MAX_UPLOAD_BYTES=10M MAX_RUNTIME=10m
```

4. Keep the check log, each run's output, report
   (`data/results/<id>-upload-regression.json`) and k6 summary, and the probes in
   a private directory on the runner. They name the instance and the deployed
   Stack version, which stay out of the repository.

## Baseline, 2026-10-25

Harness `b86138d` (#5073), Node 22.23.4,
`grafana/k6@sha256:9bd01d6941fca969cb61bb57d2da5ee9b385fe2aa8881df3798c196564d6ace6`.
The deployed Stack version is in the private reports
(`dev-baseline/20261025T135027Z/` on the runner). The container checks passed
(534/534).

| Run | Workload | Exit | Uploads | Checksums | Cleanup | Upload median / p95 / max | Run time |
| --- | --- | ---: | ---: | --- | --- | --- | ---: |
| Smoke | 1 × 1 × 1 KiB | 0 | 1/1 | match | verified | 431 / 431 / 431 ms | 5.1 s |
| 1 | 2 × 5 × 1 MiB | 0 | 10/10 | match | verified | 494 / 1060 / 1083 ms | 8.7 s |
| 2 | 2 × 5 × 1 MiB | 0 | 10/10 | match | verified | 458 / 590 / 612 ms | 8.5 s |
| 3 | 2 × 5 × 1 MiB | 0 | 10/10 | match | verified | 422 / 603 / 661 ms | 8.8 s |

- No earlier test data or k6 container needed recovery, and each run's own
  directory was gone after it. Run time spans the report, cleanup included.
- DEV activity and resource use were not observed during these runs
  (`DEV_MONITORING_URL` was unset). Before each run, the public
  `/version` probe answered in 12 to 67 ms, except one first request at 237 ms.
- With ten uploads per run, p95 is close to the slowest upload. The first
  repeat's slower tail did not recur.

## Proposed regression criteria

For the DEV owner's agreement before any performance gate:

- Keep gating on correctness, completed work and verified cleanup, as now.
- Leave latency observational. Repeated or scheduled regression runs wait until
  capacity has been measured; a latency gate can then come from those results.
- Record the DEV Grafana links with each run once the runner has them.
