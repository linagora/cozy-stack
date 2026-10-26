import { parseDurationToMs } from './duration.ts'
import { parsePositiveIntegerEnv } from './env.ts'

// A ramp holds the shared-drive mix at increasing levels. Level L runs
// L x vusPerLevel VUs in each worker group. Every sample is tagged with the
// level being held, so one k6 run can judge each level separately.

const RAMP_TRANSITION_MS = 10_000
// Changes readers keep polling this long after the last hold, so the changes
// of the last uploads are still counted.
const RAMP_DRAIN_MS = 10_000
// WebSocket sessions stay open this long after their hold, so events of
// uploads that finish at the boundary still arrive.
export const RAMP_EVENT_GRACE_MS = 2_000
const MINIMUM_HOLD_MS = 5_000
const DEFAULT_RAMP_LEVELS = '1,2,4,8,16'

export type RampVusPerLevel = {
  changesReaders: number
  writers: number
  wsReaders: number
}

export type RampPlan = {
  holdMs: number
  levels: number[]
  transitionMs: number
  vusPerLevel: RampVusPerLevel
}

export type RampPhase = {
  endsAtMs: number
  level: number | null
}

// A level fails on errors or missing events. Latency is judged only when an
// absolute limit is configured; otherwise the p95 is reported for reference.
export type RampCriteria = {
  // Downloads each changes reader verifies per level: the content verification scope.
  contentChecksPerLevel: number
  minimumDeliveryRatio: number
  minimumSuccessRate: number
  p95LimitMs: number | null
}

export type RampingVuStage = {
  duration: string
  target: number
}

export type ThresholdDefinition =
  | string
  | { abortOnFail?: boolean; delayAbortEval?: string; threshold: string }

export type SummaryMetrics = Record<
  string,
  { values?: Record<string, number> } | undefined
>

export type RampLevelStatus = 'passed' | 'failed' | 'not_measured' | 'not_run'
export type RampResultBound = 'bracket' | 'lower_bound' | 'inconclusive'

export type RampLevelResult = {
  changes_delivery_ratio: number | null
  changes_items: number | null
  changes_malformed: number
  changes_p95_ms: number | null
  changes_readers: number | null
  changes_success_rate: number | null
  content_denied: number
  content_mismatches: number
  content_unverified: number
  content_verified: number
  hold_completed: boolean
  level: number
  reasons: string[]
  status: RampLevelStatus
  upload_p95_ms: number | null
  upload_success_rate: number | null
  uploads: number | null
  ws_auth_rate: number | null
  ws_delivery_ratio: number | null
  ws_events: number | null
  ws_handshake_rate: number | null
  ws_readers: number | null
}

export type RampReport = {
  cleanup_success_rate: number | null
  // Wrong content or a refused download at any level: proven, whatever the levels measured.
  correctness_failures: string[]
  criteria: {
    content_checks_per_level: number
    minimum_delivery_ratio: number
    minimum_success_rate: number
    p95_limit_mode: 'absolute' | 'none'
    p95_limit_ms: number | null
  }
  failure_reason: string | null
  first_failed_level: number | null
  levels: RampLevelResult[]
  max_sustained_level: number | null
  plan: {
    hold_ms: number
    levels: number[]
    transition_ms: number
    vus_per_level: RampVusPerLevel
  }
  result_bound: RampResultBound
  status: 'completed' | 'infrastructure_failure'
}

type Environment = Record<string, string | undefined>

type RateKey =
  | 'changes_success_rate'
  | 'upload_success_rate'
  | 'ws_auth_rate'
  | 'ws_handshake_rate'

type TrendKey = 'changes_p95_ms' | 'upload_p95_ms'
type DeliveryKey = 'changes_delivery_ratio' | 'ws_delivery_ratio'
type CountKey = 'changes_items' | 'ws_events'
type ReadersKey = 'changes_readers' | 'ws_readers'

type JudgedRate = { key: RateKey; label: string; metric: string }
type JudgedTrend = {
  key: TrendKey
  label: string
  metric: string
  tags: Record<string, string>
}

// Every reader ready before a hold should see each successful upload of the level once. Each reader
// reports the unique uploads it saw, so a repeated event or a healthy reader never stands in for a
// missing one: every expected reader must report, and the worst must reach the minimum ratio. The
// totals stay as diagnostics.
type JudgedDelivery = {
  countKey: CountKey
  key: DeliveryKey
  label: string
  metric: string
  readersKey: ReadersKey
  readersMetric: string
  readersPerLevel: (vusPerLevel: RampVusPerLevel) => number
  tags: Record<string, string>
}

const judgedRates: readonly JudgedRate[] = [
  {
    key: 'upload_success_rate',
    label: 'upload success rate',
    metric: 'shared_drive_upload_success_rate',
  },
  {
    key: 'changes_success_rate',
    label: 'changes success rate',
    metric: 'shared_drive_changes_success_rate',
  },
  {
    key: 'ws_handshake_rate',
    label: 'ws handshake rate',
    metric: 'shared_drive_ws_handshake_rate',
  },
  { key: 'ws_auth_rate', label: 'ws auth rate', metric: 'shared_drive_ws_auth_rate' },
]

const judgedTrends: readonly JudgedTrend[] = [
  {
    key: 'upload_p95_ms',
    label: 'upload p95',
    metric: 'http_req_duration',
    tags: { operation: 'file-upload' },
  },
  {
    key: 'changes_p95_ms',
    label: 'changes p95',
    metric: 'shared_drive_changes_duration_ms',
    tags: {},
  },
]

const judgedDeliveries: readonly JudgedDelivery[] = [
  {
    countKey: 'ws_events',
    key: 'ws_delivery_ratio',
    label: 'ws event delivery',
    metric: 'shared_drive_ws_events_total',
    readersKey: 'ws_readers',
    readersMetric: 'shared_drive_ws_reader_unique',
    readersPerLevel: ({ wsReaders }: RampVusPerLevel): number => wsReaders,
    tags: { event: 'CREATED' },
  },
  {
    countKey: 'changes_items',
    key: 'changes_delivery_ratio',
    label: 'changes delivery',
    metric: 'shared_drive_changes_items_total',
    readersKey: 'changes_readers',
    readersMetric: 'shared_drive_changes_reader_unique',
    readersPerLevel: ({ changesReaders }: RampVusPerLevel): number =>
      changesReaders,
    tags: {},
  },
]

export function parseRampLevels(raw: string): number[] {
  const parts = raw.split(',').map((part: string): string => part.trim())
  const levels = parts.map(Number)
  const isValid =
    raw.trim().length > 0 &&
    parts.every((part: string): boolean => /^[1-9][0-9]*$/.test(part)) &&
    levels.every(
      (level: number, index: number): boolean =>
        index === 0 || level > (levels[index - 1] ?? 0),
    )

  if (!isValid) {
    throw new Error(
      `RAMP_LEVELS must be strictly increasing positive integers, got "${raw}"`,
    )
  }

  return levels
}

export function parseRampPlan(env: Environment): RampPlan {
  const rawStepDuration = env.RAMP_STEP_DURATION || '1m'
  const holdMs = parseDurationToMs(rawStepDuration)

  if (holdMs < MINIMUM_HOLD_MS) {
    throw new Error(
      `RAMP_STEP_DURATION must be at least 5s, got "${rawStepDuration}"`,
    )
  }

  return {
    holdMs,
    levels: parseRampLevels(env.RAMP_LEVELS || DEFAULT_RAMP_LEVELS),
    transitionMs: RAMP_TRANSITION_MS,
    vusPerLevel: {
      changesReaders: parsePositiveIntegerEnv(
        env,
        'RAMP_CHANGES_READERS_PER_LEVEL',
        2,
      ),
      writers: parsePositiveIntegerEnv(env, 'RAMP_WRITERS_PER_LEVEL', 1),
      wsReaders: parsePositiveIntegerEnv(env, 'RAMP_WS_READERS_PER_LEVEL', 3),
    },
  }
}

function parsePositiveNumber(name: string, raw: string): number {
  const value = Number(raw)

  if (!Number.isFinite(value) || value <= 0) {
    throw new Error(`${name} must be a positive number, got "${raw}"`)
  }

  return value
}

function parseRatio(name: string, raw: string): number {
  const value = Number(raw)

  if (!Number.isFinite(value) || value <= 0 || value > 1) {
    throw new Error(`${name} must be greater than 0 and at most 1, got "${raw}"`)
  }

  return value
}

export function parseRampCriteria(env: Environment): RampCriteria {
  return {
    minimumDeliveryRatio: parseRatio(
      'MIN_DELIVERY_RATIO',
      env.MIN_DELIVERY_RATIO || '0.9',
    ),
    minimumSuccessRate: parseRatio(
      'MIN_SUCCESS_RATE',
      env.MIN_SUCCESS_RATE || '0.99',
    ),
    p95LimitMs: env.P95_LIMIT_MS
      ? parsePositiveNumber('P95_LIMIT_MS', env.P95_LIMIT_MS)
      : null,
    contentChecksPerLevel: parseCount('CONTENT_CHECKS_PER_LEVEL', env.CONTENT_CHECKS_PER_LEVEL || '2'),
  }
}

function parseCount(name: string, raw: string): number {
  if (!/^[0-9]+$/.test(raw) || !Number.isSafeInteger(Number(raw))) {
    throw new Error(`${name} must be a non-negative integer, got "${raw}"`)
  }
  return Number(raw)
}

function getLevelSpanMs(plan: RampPlan): number {
  return plan.transitionMs + plan.holdMs
}

export function getRampTotalDurationMs(plan: RampPlan): number {
  return plan.levels.length * getLevelSpanMs(plan)
}

export function getRampHoldStartMs(plan: RampPlan, level: number): number {
  return plan.levels.indexOf(level) * getLevelSpanMs(plan) + plan.transitionMs
}

export function getCompletedRampLevels(plan: RampPlan, elapsedMs: number): number[] {
  if (!Number.isFinite(elapsedMs)) {
    return []
  }
  return plan.levels.filter((_, index) => elapsedMs >= (index + 1) * getLevelSpanMs(plan))
}

export function buildRampingVuStages(
  plan: RampPlan,
  vusPerLevel: number,
  { drain = false }: { drain?: boolean } = {},
): RampingVuStage[] {
  // k6 starts VUs evenly over a stage. Ramping over the first half of the
  // transition leaves the last reader the second half to connect, so every
  // reader observes the whole hold.
  const rampMs = Math.floor(plan.transitionMs / 2)
  const stages = plan.levels.flatMap((level: number): RampingVuStage[] => {
    const target = level * vusPerLevel
    return [
      { duration: `${rampMs}ms`, target },
      { duration: `${plan.transitionMs - rampMs}ms`, target },
      { duration: `${plan.holdMs}ms`, target },
    ]
  })
  const lastTarget = stages[stages.length - 1]?.target ?? 0

  return drain
    ? [...stages, { duration: `${RAMP_DRAIN_MS}ms`, target: lastTarget }]
    : stages
}

export function getRampPhaseAt(plan: RampPlan, elapsedMs: number): RampPhase {
  const spanMs = getLevelSpanMs(plan)
  const index = Math.floor(elapsedMs / spanMs)
  const level = plan.levels[index]

  if (elapsedMs < 0 || level === undefined) {
    return { endsAtMs: getRampTotalDurationMs(plan), level: null }
  }

  const transitionEndsAtMs = index * spanMs + plan.transitionMs

  return elapsedMs < transitionEndsAtMs
    ? { endsAtMs: transitionEndsAtMs, level: null }
    : { endsAtMs: (index + 1) * spanMs, level }
}

// A WebSocket session started during a transition serves the upcoming level
// and lasts until that level's hold ends, so readers are connected for the
// whole hold and reconnect only during transitions.
export function getServedRampPhase(plan: RampPlan, elapsedMs: number): RampPhase {
  const phase = getRampPhaseAt(plan, elapsedMs)

  if (phase.level !== null) {
    return phase
  }

  const upcoming = getRampPhaseAt(plan, phase.endsAtMs)
  return upcoming.level === null ? phase : upcoming
}

const RAMP_UPLOAD_LEVEL_PATTERN = /^sd-L([1-9][0-9]*)-/

// Ramp uploads carry the level that produced them in their name. Events and
// changes include the file name, so readers attribute what they receive to
// the right level whenever it arrives.
export function getRampUploadPrefix(phase: RampPhase): string {
  return phase.level === null ? 'sd-transition-' : `sd-L${phase.level}-`
}

export function parseRampUploadLevel(name: unknown): string {
  if (typeof name !== 'string') {
    return 'unattributed'
  }

  return RAMP_UPLOAD_LEVEL_PATTERN.exec(name)?.[1] ?? 'unattributed'
}

export function rampLevelMetricName(
  metric: string,
  level: number,
  tags: Record<string, string> = {},
): string {
  const selectors = [
    ...Object.entries(tags).map(
      ([name, value]: [string, string]): string => `${name}:${value}`,
    ),
    `level:${level}`,
  ]
  return `${metric}{${selectors.join(',')}}`
}

// The per-level thresholds always pass. They exist so k6 keeps a submetric
// for each level; the summary judges those submetrics after the run.
export function buildRampThresholds(
  plan: RampPlan,
): Record<string, ThresholdDefinition[]> {
  const thresholds: Record<string, ThresholdDefinition[]> = {
    cleanup_success: ['rate==1'],
    // Stop hammering a stack that is clearly down.
    http_req_failed: [
      { abortOnFail: true, delayAbortEval: '30s', threshold: 'rate<0.5' },
    ],
  }

  for (const level of plan.levels) {
    thresholds[rampLevelMetricName('shared_drive_ramp_hold_completed', level)] = ['count>=0']
    for (const { metric } of judgedRates) {
      thresholds[rampLevelMetricName(metric, level)] = ['rate>=0']
    }
    for (const { metric, tags } of judgedTrends) {
      thresholds[rampLevelMetricName(metric, level, tags)] = ['p(95)>=0']
    }
    for (const { metric, readersMetric, tags } of judgedDeliveries) {
      thresholds[rampLevelMetricName(metric, level, tags)] = ['count>=0']
      // A trend threshold cannot use count; min keeps the submetric all the same.
      thresholds[rampLevelMetricName(readersMetric, level)] = ['min>=0']
    }
    for (const metric of contentCounters) thresholds[rampLevelMetricName(metric, level)] = ['count>=0']
  }

  return thresholds
}

type RateReading = { passes: number; rate: number }

function readRate(metrics: SummaryMetrics, name: string): RateReading | null {
  const values = metrics[name]?.values
  const passes = values?.passes ?? 0
  const samples = passes + (values?.fails ?? 0)
  const rate = values?.rate

  return samples > 0 && typeof rate === 'number' && Number.isFinite(rate)
    ? { passes, rate }
    : null
}

// k6 reports an empty submetric with a p95 of 0, so a p95 only counts when
// the trend also has samples. The ramp asks k6 for `count` for that reason.
function readP95(metrics: SummaryMetrics, name: string): number | null {
  const values = metrics[name]?.values
  const value = values?.['p(95)']

  return (values?.count ?? 0) > 0 &&
    typeof value === 'number' &&
    Number.isFinite(value)
    ? value
    : null
}

export const contentCounters = [
  'shared_drive_content_verified_total', 'shared_drive_content_mismatch_total',
  'shared_drive_content_denied_total', 'shared_drive_changes_malformed_total',
  'shared_drive_content_unverified_total',
] as const

// The worst reader's unique observations, and how many readers reported.
function readReaders(metrics: SummaryMetrics, name: string): { min: number, count: number } | null {
  const values = metrics[name]?.values
  const count = values?.count ?? 0
  return count > 0 && typeof values?.min === 'number' ? { min: values.min, count } : null
}

function readCount(metrics: SummaryMetrics, name: string): number | null {
  const value = metrics[name]?.values?.count
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

function round(value: number, digits: number): number {
  return Number(value.toFixed(digits))
}

type Verdicts = { failures: string[]; missing: string[] }

function judgeRates(
  rates: Record<RateKey, RateReading | null>,
  criteria: RampCriteria,
  verdicts: Verdicts,
): void {
  for (const { key, label } of judgedRates) {
    const reading = rates[key]
    if (reading === null) {
      verdicts.missing.push(`${label} has no samples`)
    } else if (reading.rate < criteria.minimumSuccessRate) {
      verdicts.failures.push(
        `${label} ${round(reading.rate, 4)} is below ${criteria.minimumSuccessRate}`,
      )
    }
  }
}

function judgeDeliveries(
  ratios: Record<DeliveryKey, number | null>,
  readers: Record<ReadersKey, number | null>,
  expected: Record<ReadersKey, number>,
  criteria: RampCriteria,
  verdicts: Verdicts,
): void {
  for (const { key, label, readersKey } of judgedDeliveries) {
    const ratio = ratios[key]
    const reported = readers[readersKey]
    if (reported === null) {
      verdicts.missing.push(`${label} has no samples`)
    } else if (reported < expected[readersKey]) {
      verdicts.failures.push(`${label}: ${reported} of ${expected[readersKey]} readers reported`)
    } else if (ratio === null) {
      verdicts.missing.push(`${label} has no uploads to compare with`)
    } else if (ratio < criteria.minimumDeliveryRatio) {
      verdicts.failures.push(
        `${label} ${round(ratio, 4)} is below ${criteria.minimumDeliveryRatio}`,
      )
    }
  }
}

function judgeTrends(
  trends: Record<TrendKey, number | null>,
  criteria: RampCriteria,
  verdicts: Verdicts,
): void {
  const limit = criteria.p95LimitMs
  if (limit === null) {
    return
  }

  for (const { key, label } of judgedTrends) {
    const value = trends[key]
    if (value === null) {
      verdicts.missing.push(`${label} has no samples`)
    } else if (value > limit) {
      verdicts.failures.push(`${label} ${round(value, 1)} ms exceeds ${limit} ms`)
    }
  }
}

function contentFailures({ content_denied: denied, content_mismatches: mismatches }: RampLevelResult): string[] {
  return [
    ...(mismatches ? [`${mismatches} uploads read back different content`] : []),
    ...(denied ? [`${denied} reader downloads were denied`] : []),
  ]
}

function evaluateLevel(
  metrics: SummaryMetrics,
  level: number,
  plan: RampPlan,
  criteria: RampCriteria,
): RampLevelResult {
  const rates = Object.fromEntries(
    judgedRates.map(({ key, metric }) => [
      key,
      readRate(metrics, rampLevelMetricName(metric, level)),
    ]),
  ) as Record<RateKey, RateReading | null>
  const trends = Object.fromEntries(
    judgedTrends.map(({ key, metric, tags }) => [
      key,
      readP95(metrics, rampLevelMetricName(metric, level, tags)),
    ]),
  ) as Record<TrendKey, number | null>
  const uploads = rates.upload_success_rate?.passes ?? null
  const counts = Object.fromEntries(
    judgedDeliveries.map(({ countKey, metric, tags }) => [
      countKey,
      readCount(metrics, rampLevelMetricName(metric, level, tags)),
    ]),
  ) as Record<CountKey, number | null>
  const readings = Object.fromEntries(judgedDeliveries.map(({ readersKey, readersMetric }) =>
    [readersKey, readReaders(metrics, rampLevelMetricName(readersMetric, level))]))
  const readers = Object.fromEntries(judgedDeliveries.map(({ readersKey }) =>
    [readersKey, readings[readersKey]?.count ?? null])) as Record<ReadersKey, number | null>
  const expected = Object.fromEntries(judgedDeliveries.map(({ readersKey, readersPerLevel }) =>
    [readersKey, level * readersPerLevel(plan.vusPerLevel)])) as Record<ReadersKey, number>
  const ratios = Object.fromEntries(judgedDeliveries.map(({ key, readersKey }) => {
    const worst = readings[readersKey]?.min
    return [key, worst === undefined || !uploads ? null : worst / uploads]
  })) as Record<DeliveryKey, number | null>
  const [verified, mismatches, denied, malformed, unverified] = contentCounters.map(metric =>
    readCount(metrics, rampLevelMetricName(metric, level)) ?? 0)
  const result: RampLevelResult = {
    ...counts,
    ...ratios,
    ...readers,
    ...trends,
    changes_malformed: malformed!,
    changes_success_rate: rates.changes_success_rate?.rate ?? null,
    content_denied: denied!,
    content_mismatches: mismatches!,
    content_unverified: unverified!,
    content_verified: verified!,
    hold_completed: (readCount(metrics, rampLevelMetricName('shared_drive_ramp_hold_completed', level)) ?? 0) > 0,
    level,
    reasons: [],
    status: 'passed',
    upload_success_rate: rates.upload_success_rate?.rate ?? null,
    uploads,
    ws_auth_rate: rates.ws_auth_rate?.rate ?? null,
    ws_handshake_rate: rates.ws_handshake_rate?.rate ?? null,
  }

  if (rates.upload_success_rate === null && rates.changes_success_rate === null) {
    return { ...result, status: 'not_run' }
  }

  // A check that read nothing, or no check at all, leaves the level's content unverified.
  const contentMissing = [
    ...(result.content_unverified ? [`${result.content_unverified} content checks could not read the file`] : []),
    ...(criteria.contentChecksPerLevel && uploads && !result.content_verified
      ? ['no upload content was verified'] : []),
  ]
  const verdicts: Verdicts = { failures: [...contentFailures(result)], missing: contentMissing }
  judgeRates(rates, criteria, verdicts)
  judgeDeliveries(ratios, readers, expected, criteria, verdicts)
  judgeTrends(trends, criteria, verdicts)

  const { failures, missing } = verdicts
  if (!result.hold_completed) {
    return { ...result, reasons: ['hold did not complete', ...failures, ...missing], status: 'not_measured' }
  }
  const status: RampLevelStatus =
    failures.length > 0 ? 'failed' : missing.length > 0 ? 'not_measured' : 'passed'

  return { ...result, reasons: [...failures, ...missing], status }
}

export function evaluateRampLevels(
  metrics: SummaryMetrics,
  plan: RampPlan,
  criteria: RampCriteria,
): RampReport {
  const levels = plan.levels.map(
    (level: number): RampLevelResult =>
      evaluateLevel(metrics, level, plan, criteria),
  )
  const firstNotPassed = levels.find(
    ({ status }: RampLevelResult): boolean => status !== 'passed',
  )
  const sustainedLevels =
    firstNotPassed === undefined
      ? levels
      : levels.slice(0, levels.indexOf(firstNotPassed))
  const maxSustainedLevel =
    sustainedLevels[sustainedLevels.length - 1]?.level ?? 0
  const resultBound: RampResultBound =
    firstNotPassed === undefined
      ? 'lower_bound'
      : firstNotPassed.status === 'failed'
        ? 'bracket'
        : 'inconclusive'

  const cleanupRate = readRate(metrics, 'cleanup_success')?.rate ?? null
  const cleanupPassed = cleanupRate === 1
  const failureReason = cleanupPassed ? null : cleanupRate === null
    ? 'test-directory cleanup was not measured' : 'test-directory cleanup failed'

  return {
    cleanup_success_rate: cleanupRate,
    correctness_failures: levels.flatMap(level =>
      contentFailures(level).map(reason => `level ${level.level}: ${reason}`)),
    criteria: {
      content_checks_per_level: criteria.contentChecksPerLevel,
      minimum_delivery_ratio: criteria.minimumDeliveryRatio,
      minimum_success_rate: criteria.minimumSuccessRate,
      p95_limit_mode: criteria.p95LimitMs === null ? 'none' : 'absolute',
      p95_limit_ms: criteria.p95LimitMs,
    },
    failure_reason: failureReason,
    first_failed_level: cleanupPassed && firstNotPassed?.status === 'failed' ? firstNotPassed.level : null,
    levels,
    max_sustained_level: cleanupPassed ? maxSustainedLevel : null,
    plan: {
      hold_ms: plan.holdMs,
      levels: plan.levels,
      transition_ms: plan.transitionMs,
      vus_per_level: plan.vusPerLevel,
    },
    result_bound: cleanupPassed ? resultBound : 'inconclusive',
    status: cleanupPassed ? 'completed' : 'infrastructure_failure',
  }
}

function formatRampBound(report: RampReport): string {
  if (report.status === 'infrastructure_failure') {
    return `Capacity result invalid: ${report.failure_reason}`
  }
  if (report.result_bound === 'bracket') {
    return `Sustained level bracket: [${report.max_sustained_level}, ${report.first_failed_level})`
  }
  if (report.result_bound === 'lower_bound') {
    return `Maximum sustained level: ≥ ${report.max_sustained_level} (lower_bound)`
  }
  return `Highest completed passing level: ${report.max_sustained_level} (inconclusive)`
}

export function formatRampReport(report: RampReport): string {
  const { changesReaders, writers, wsReaders } = report.plan.vus_per_level
  const lines = [
    '',
    `Shared drive ramp (writers:ws readers:changes readers per level = ${writers}:${wsReaders}:${changesReaders})`,
    ...report.levels.map(({ level, reasons, status }: RampLevelResult): string =>
      reasons.length > 0
        ? `  level ${level}: ${status} (${reasons.join('; ')})`
        : `  level ${level}: ${status}`,
    ),
    formatRampBound(report),
    '',
  ]

  return lines.join('\n')
}
