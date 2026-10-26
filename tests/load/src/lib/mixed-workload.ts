import type { SummaryMetrics } from './load-profile.ts'

// One workflow lists a fixture directory, reads and checks a fixture file's
// metadata, and uploads a small file into the run's own directory.
export const MIXED_OPERATIONS = ['list', 'metadata', 'upload'] as const
export type MixedOperation = (typeof MIXED_OPERATIONS)[number]

export type MixedPlan = {
  durationS: number
  maxVus: number
  observers: number
  // Workflows started per second, whatever the responses' latency.
  rate: number
}

export type MixedCriteria = {
  minimumSuccessRate: number
  p95LimitMs: number
}

export type MixedStatus = 'passed' | 'failed' | 'generator_saturated' | 'not_measured'

export type MixedReport = {
  unit: 'workflows/s'
  plan: { duration_s: number, max_vus: number, observers: number, rate: number }
  criteria: { minimum_success_rate: number, p95_limit_ms: number }
  offered_workflows: number
  achieved_workflows: number | null
  achieved_rate: number | null
  dropped_iterations: number
  operations: Record<MixedOperation, {
    error_rate: number | null
    p95_ms: number | null
    rate_per_s: number
    requests: number
  }>
  correctness: { metadata_checks: number, metadata_mismatches: number, upload_checks: number, upload_mismatches: number }
  observers: { events: number, sessions: number }
  // Wrong metadata or a wrongly recorded upload: proven, whatever the rates.
  correctness_failures: string[]
  reasons: string[]
  status: MixedStatus
}

export const operationMetric = (metric: string, operation: MixedOperation): string =>
  `${metric}{operation:${operation}}`

// Always-passing thresholds keep a submetric per operation in the summary.
export function buildMixedThresholds(): Record<string, string[]> {
  return Object.fromEntries(MIXED_OPERATIONS.flatMap((operation: MixedOperation) => [
    [operationMetric('http_reqs', operation), ['count>=0']],
    [operationMetric('http_req_duration', operation), ['p(95)>=0']],
    [operationMetric('http_req_failed', operation), ['rate>=0']],
  ]))
}

function value(metrics: SummaryMetrics, name: string, field: string): number | null {
  const reading = metrics[name]?.values?.[field]
  return typeof reading === 'number' && Number.isFinite(reading) ? reading : null
}

const round = (reading: number, digits: number): number => Number(reading.toFixed(digits))

export function evaluateMixed(metrics: SummaryMetrics, plan: MixedPlan, criteria: MixedCriteria): MixedReport {
  const achieved = value(metrics, 'iterations', 'count')
  const dropped = value(metrics, 'dropped_iterations', 'count') ?? 0
  const operations = Object.fromEntries(MIXED_OPERATIONS.map((operation: MixedOperation) => {
    const requests = value(metrics, operationMetric('http_reqs', operation), 'count') ?? 0
    const samples = value(metrics, operationMetric('http_req_duration', operation), 'count') ?? 0
    return [operation, {
      error_rate: requests ? value(metrics, operationMetric('http_req_failed', operation), 'rate') : null,
      // k6 keeps an empty submetric with a p95 of 0: only a sampled p95 counts.
      p95_ms: samples ? value(metrics, operationMetric('http_req_duration', operation), 'p(95)') : null,
      rate_per_s: round(requests / plan.durationS, 3),
      requests,
    }]
  })) as MixedReport['operations']
  const checks = (name: string) => ({
    passes: value(metrics, name, 'passes') ?? 0, fails: value(metrics, name, 'fails') ?? 0,
  })
  const metadata = checks('mixed_metadata_match')
  const upload = checks('mixed_upload_match')
  const correctnessFailures = [
    ...(metadata.fails ? [`${metadata.fails} metadata reads differed from the fixture`] : []),
    ...(upload.fails ? [`${upload.fails} uploads were recorded with another checksum or size`] : []),
  ]

  const failures: string[] = []
  for (const operation of MIXED_OPERATIONS) {
    const { error_rate: errors, p95_ms: p95 } = operations[operation]
    if (errors !== null && 1 - errors < criteria.minimumSuccessRate) {
      failures.push(`${operation} success rate ${round(1 - errors, 4)} is below ${criteria.minimumSuccessRate}`)
    }
    if (p95 !== null && p95 > criteria.p95LimitMs) {
      failures.push(`${operation} p95 ${round(p95, 1)} ms exceeds ${criteria.p95LimitMs} ms`)
    }
  }
  // Dropped work with every operation within its limits is the generator falling behind, never the
  // target; with a slow or failing target it is part of that regression.
  let status: MixedStatus = achieved === null || achieved === 0 ? 'not_measured' : 'passed'
  const reasons = [...correctnessFailures, ...failures]
  if (status === 'not_measured') {
    reasons.push('no workflow completed')
  } else if (correctnessFailures.length || failures.length) {
    status = 'failed'
    if (dropped) reasons.push(`${dropped} workflows dropped`)
  } else if (dropped) {
    status = 'generator_saturated'
    reasons.push(`${dropped} workflows dropped while every operation met its limits: the generator fell behind`)
  }

  return {
    unit: 'workflows/s',
    plan: { duration_s: plan.durationS, max_vus: plan.maxVus, observers: plan.observers, rate: plan.rate },
    criteria: { minimum_success_rate: criteria.minimumSuccessRate, p95_limit_ms: criteria.p95LimitMs },
    offered_workflows: plan.rate * plan.durationS,
    achieved_workflows: achieved,
    achieved_rate: achieved === null ? null : round(achieved / plan.durationS, 3),
    dropped_iterations: dropped,
    operations,
    correctness: {
      metadata_checks: metadata.passes + metadata.fails, metadata_mismatches: metadata.fails,
      upload_checks: upload.passes + upload.fails, upload_mismatches: upload.fails,
    },
    observers: {
      events: value(metrics, 'mixed_observer_events', 'count') ?? 0,
      sessions: value(metrics, 'mixed_observer_sessions', 'count') ?? 0,
    },
    correctness_failures: correctnessFailures,
    reasons,
    status,
  }
}
