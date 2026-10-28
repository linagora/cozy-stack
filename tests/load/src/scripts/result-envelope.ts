// The versioned result envelope each producer adds to its report as `result` (README, Results). It
// records the producer's verdict once, with each outcome dimension apart; summaries and exporters
// read it and never recompute or upgrade it. Reports without one keep their legacy classification.
import { isJsonRecord, type JsonRecord } from './capacity/k6-summary.ts'
import { hasContentMismatch } from './capacity/upload-point.ts'

export const RESULT_SCHEMA = 'cozy-load-result/1'
const PRODUCERS = [
  'upload-capacity', 'upload-regression', 'shared-drive-ramp', 'org-drive-fanout', 'mixed-workload', 'mixed-capacity',
] as const
// Fixed-load producers judge a rate, never a capacity.
const FIXED_LOAD: readonly string[] = ['upload-regression', 'mixed-workload']
const VERIFICATIONS = ['verified', 'failed', 'not_verified'] as const

type Verification = typeof VERIFICATIONS[number]
export interface Capacity {
  kind: 'exact' | 'lower_bound' | 'bracket'
  lower: number
  upper_exclusive: number | null
  unit: string
  // False keeps a measurement the run could not verify as evidence only.
  verified: boolean
  // Why the search stopped: its first failing level and why, the budget it reached, or that every level
  // passed. Envelopes written before it have none.
  stop?: string
}
export interface ResultEnvelope {
  schema: typeof RESULT_SCHEMA
  producer: typeof PRODUCERS[number]
  outcome: 0 | 1 | 2
  reason: string
  execution: 'completed' | 'interrupted' | 'incomplete'
  correctness: Verification
  performance: 'passed' | 'failed' | 'not_measured'
  capacity: Capacity | null
  cleanup: Verification
  versions: Record<string, string>
  workload: string
  // The seeded dataset a workload ran on: only the mixed capacity search records its tree fixture.
  dataset: { profile: string, version: number } | null
  load_target: 'local' | 'dev'
  topology: string
  criteria: JsonRecord
  parameters: JsonRecord
  time: { start: string, end: string }
}
// What the producer knows beyond its report.
export interface RunContext {
  interrupted: boolean
  versions: Record<string, string>
  topology: string
  start: string
  end: string
}

export const topologyOf = (loadTarget: string) =>
  loadTarget === 'dev' ? 'shared-dev' : process.env.LOAD_TOPOLOGY || 'single-host'

const text = (value: unknown): value is string => typeof value === 'string' && value.length > 0
const count = (value: unknown): value is number => Number.isSafeInteger(value) && (value as number) >= 0
const oneOf = (value: unknown, choices: readonly unknown[]) => choices.includes(value)

export function validateEnvelope(value: unknown): ResultEnvelope | null {
  if (!isJsonRecord(value)) return null
  const e = value as unknown as ResultEnvelope
  const c = e.capacity
  const capacity = c === null || (isJsonRecord(c) && oneOf(c.kind, ['exact', 'lower_bound', 'bracket']) &&
    count(c.lower) && text(c.unit) && typeof c.verified === 'boolean' && (c.stop === undefined || text(c.stop)) &&
    (c.kind === 'bracket' ? count(c.upper_exclusive) && c.upper_exclusive > c.lower : c.upper_exclusive === null))
  const start = Date.parse(e.time?.start)
  const end = Date.parse(e.time?.end)
  const shape = e.schema === RESULT_SCHEMA && oneOf(e.producer, PRODUCERS) && oneOf(e.outcome, [0, 1, 2]) &&
    typeof e.reason === 'string' && oneOf(e.execution, ['completed', 'interrupted', 'incomplete']) &&
    oneOf(e.correctness, VERIFICATIONS) && oneOf(e.performance, ['passed', 'failed', 'not_measured']) &&
    oneOf(e.cleanup, VERIFICATIONS) && capacity && isJsonRecord(e.versions) &&
    Object.values(e.versions).every(text) && text(e.versions.harness) && text(e.versions.stack) && text(e.workload) &&
    (e.dataset === null || (isJsonRecord(e.dataset) && text(e.dataset.profile) && count(e.dataset.version))) &&
    oneOf(e.load_target, ['local', 'dev']) && text(e.topology) && isJsonRecord(e.criteria) &&
    isJsonRecord(e.parameters) && start <= end
  if (!shape) return null
  // The verdict must agree with its own evidence, so an incomplete envelope cannot pass.
  const passing = e.execution === 'completed' && e.correctness !== 'failed' && e.cleanup !== 'failed' &&
    (FIXED_LOAD.includes(e.producer) ? e.performance === 'passed' && c === null : c?.verified === true)
  const consistent = (e.outcome !== 0 || passing) &&
    (e.outcome !== 1 || e.correctness === 'failed' || e.performance === 'failed') &&
    (!c?.verified || e.outcome === 0)
  return consistent ? e : null
}

// The producer's verdict: null for a legacy report without an envelope, inconclusive for an invalid one.
export function declaredOutcome(report: JsonRecord): { code: 0 | 1 | 2, reason: string } | null {
  if (report.result === undefined) return null
  const envelope = validateEnvelope(report.result)
  return envelope ? { code: envelope.outcome, reason: envelope.reason } : { code: 2, reason: 'invalid result envelope' }
}

// The budget that ended a search while its levels still passed, by its setting.
const BUDGETS: Record<string, string> = {
  concurrency: 'MAX_VUS', 'upload bytes': 'MAX_UPLOAD_BYTES', runtime: 'MAX_RUNTIME',
}

function stopOf(unit: string, lower: number, failed: { level: number, reasons: unknown } | null,
  budget?: unknown): string {
  if (failed) {
    const reasons = [failed.reasons].flat().filter(text).join('; ')
    return `level ${failed.level} (${unit}): ${reasons || 'failed'}`
  }
  // The step loop (capacity/step-ramp.ts) names the budget's setting itself.
  const setting = BUDGETS[String(budget)] ?? (/^MAX_[A-Z_]+$/.test(String(budget)) ? String(budget) : undefined)
  return setting ? `${setting} reached after level ${lower} (${unit})` : `every level passed, up to ${lower} (${unit})`
}

function cleanupOf(cleanups: Array<{ remaining: unknown[] } | null | undefined>): Verification {
  if (cleanups.some(c => c && c.remaining.length > 0)) return 'failed'
  return cleanups.length > 0 && cleanups.every(Boolean) ? 'verified' : 'not_verified'
}

function envelope(producer: ResultEnvelope['producer'], report: Record<string, any>, context: RunContext,
  reason: unknown, fields: Pick<ResultEnvelope, 'execution' | 'correctness' | 'performance' | 'capacity' |
    'cleanup' | 'workload' | 'criteria' | 'parameters'>): ResultEnvelope {
  return {
    schema: RESULT_SCHEMA, producer, outcome: report.outcome,
    reason: report.outcome === 0 ? '' : String(reason || 'inconclusive'),
    ...fields, versions: context.versions, dataset: null, load_target: report.load_target,
    topology: context.topology, time: { start: context.start, end: context.end },
  }
}

export function uploadEnvelope(report: Record<string, any>, context: RunContext): ResultEnvelope {
  const regression = report.mode === 'regression'
  const attempts: Record<string, any>[] = report.attempts ?? []
  const maximum = report.maximum_concurrent_uploads
  const measured = !regression && count(maximum) && oneOf(report.result_bound, ['exact', 'lower_bound', 'bracket'])
  // An exact capacity stopped at the level above it, an unrefined bracket at its first failing level.
  const failing = report.result_bound === 'exact' ? maximum + 1 : report.first_failed_vus
  const finished = oneOf(report.status, ['completed', 'regression_failure', 'correctness_failure'])
  return envelope(regression ? 'upload-regression' : 'upload-capacity', report, context, report.failure_reason, {
    execution: context.interrupted ? 'interrupted' : finished ? 'completed' : 'incomplete',
    correctness: report.status === 'correctness_failure' || hasContentMismatch(attempts) ? 'failed'
      : attempts.some(a => a.consistency_rate === 1) ? 'verified' : 'not_verified',
    performance: regression
      ? report.status === 'regression_failure' ? 'failed' : report.status === 'completed' ? 'passed' : 'not_measured'
      : !measured ? 'not_measured' : maximum === 0 ? 'failed' : 'passed',
    capacity: measured ? {
      kind: report.result_bound, lower: maximum,
      upper_exclusive: report.result_bound === 'bracket' ? report.first_failed_vus : null, unit: 'concurrent uploads',
      verified: report.outcome === 0,
      stop: stopOf('concurrent uploads', maximum, report.result_bound === 'lower_bound' ? null : {
        level: failing, reasons: attempts.filter(a => a.vus === failing && a.result === 'failed').at(-1)?.reason,
      }, report.budget_reached),
    } : null,
    cleanup: cleanupOf(attempts.map(a => a.cleanup)),
    workload: `upload-${report.mode}`,
    criteria: { ...report.criteria, p95_limit_ms: report.p95_limit_ms ?? null },
    parameters: {
      file_size: report.file_size, upload_target: report.upload_target,
      configured_max_vus: report.configured_max_vus, budgets: report.budgets,
    },
  })
}

export function rampEnvelope(report: Record<string, any>, context: RunContext): ResultEnvelope {
  const lower = report.max_sustained_level
  const upper = report.first_failed_level
  const bracket = count(lower) && count(upper) && upper > lower
  const k6Cleanup = report.cleanup_success_rate
  return envelope('shared-drive-ramp', report, context, report.failure_reason, {
    execution: context.interrupted ? 'interrupted' : report.status === 'completed' ? 'completed' : 'incomplete',
    // Readers download a sample of the uploads (content_checks_per_level); member actors prove their access.
    correctness: report.access_failure || report.correctness_failures?.length ? 'failed'
      : (report.levels ?? []).some((l: Record<string, any>) => l.content_verified > 0) ? 'verified' : 'not_verified',
    performance: !count(lower) ? 'not_measured' : lower > 0 ? 'passed' : bracket ? 'failed' : 'not_measured',
    capacity: count(lower) && (lower > 0 || bracket) ? {
      kind: bracket ? 'bracket' : 'lower_bound', lower, upper_exclusive: bracket ? upper : null,
      unit: 'activity level', verified: report.outcome === 0,
      stop: stopOf('activity level', lower, bracket ? {
        level: upper, reasons: report.levels?.find((l: Record<string, any>) => l.level === upper)?.reasons,
      } : null, report.budget_reached),
    } : null,
    cleanup: typeof k6Cleanup === 'number' && k6Cleanup < 1 ? 'failed'
      : k6Cleanup === 1 ? cleanupOf([report.cleanup]) : 'not_verified',
    workload: 'shared-drive-ramp',
    criteria: report.criteria ?? {},
    parameters: { plan: report.plan ?? null, file_size: report.file_size, budgets: report.budgets },
  })
}

export function fanoutEnvelope(report: Record<string, any>, context: RunContext): ResultEnvelope {
  const passed = (report.points ?? []).some((p: Record<string, any>) => p.status === 'passed')
  const { kind, lower, upper_exclusive: upper } = report.capacity ?? {}
  const settings = report.settings ?? {}
  const retained = report.cleanup?.retained
  return envelope('org-drive-fanout', report, context, report.error, {
    execution: context.interrupted ? 'interrupted' : report.status === 'inconclusive' ? 'incomplete' : 'completed',
    // A passed level read the marker bytes back on every recipient.
    correctness: report.content_mismatch ? 'failed' : passed ? 'verified' : 'not_verified',
    performance: passed ? 'passed' : report.status === 'failed' ? 'failed' : 'not_measured',
    capacity: oneOf(kind, ['lower_bound', 'bracket']) && count(lower) ? {
      kind, lower, upper_exclusive: kind === 'bracket' ? upper : null, unit: 'recipients',
      verified: report.outcome === 0,
      stop: stopOf('recipients', lower, kind === 'bracket' ? {
        level: upper, reasons: report.points?.at(-1)?.error ?? report.error,
      } : null, report.budget_reached),
    } : null,
    // Locally the organization fixture is retained by design: nothing claims its cleanup.
    cleanup: !Array.isArray(retained) ? 'not_verified' : retained.length ? 'failed' : 'verified',
    workload: 'org-drive-fanout',
    criteria: { timeout_ms: settings.timeout_ms ?? null, stable_ms: settings.stable_ms ?? null },
    parameters: { mode: report.mode ?? null, ...settings },
  })
}

export function mixedEnvelope(report: Record<string, any>, context: RunContext): ResultEnvelope {
  const checks = report.correctness ?? {}
  return envelope('mixed-workload', report, context, report.failure_reason, {
    execution: context.interrupted ? 'interrupted' : report.status && report.status !== 'not_measured' &&
      !report.problems?.length ? 'completed' : 'incomplete',
    correctness: report.correctness_failures?.length ? 'failed'
      : checks.complete === true ? 'verified' : 'not_verified',
    // A regression fails performance; a correctness failure alone says nothing about it.
    performance: report.status === 'failed' && report.reasons?.length > (report.correctness_failures?.length ?? 0)
      ? 'failed' : report.status === 'passed' ? 'passed' : 'not_measured',
    capacity: null,
    cleanup: cleanupOf([report.cleanup]),
    workload: 'mixed-workload',
    criteria: report.criteria ?? {},
    parameters: { plan: report.plan ?? null, budgets: report.budgets ?? null, fixture: report.fixture ?? null },
  })
}

export function mixedCapacityEnvelope(report: Record<string, any>, context: RunContext): ResultEnvelope {
  const levels: Record<string, any>[] = report.levels ?? []
  const lower = report.max_sustained_rate
  const upper = report.first_failed_rate
  const bracket = report.result_bound === 'bracket' && count(lower) && count(upper) && upper > lower
  // Rates that ran to a verdict; every one must have checked all its successful responses.
  const judged = levels.filter(l => l.status === 'passed' || l.status === 'failed')
  return {
    ...envelope('mixed-capacity', report, context, report.failure_reason, {
      execution: context.interrupted ? 'interrupted'
        : oneOf(report.status, ['completed', 'correctness_failure']) ? 'completed' : 'incomplete',
      correctness: report.correctness_failures?.length ? 'failed'
        : judged.length && judged.every(l => l.correctness?.complete === true) ? 'verified' : 'not_verified',
      performance: !count(lower) ? 'not_measured' : lower > 0 ? 'passed' : bracket ? 'failed' : 'not_measured',
      capacity: count(lower) && (lower > 0 || bracket) ? {
        kind: bracket ? 'bracket' : 'lower_bound', lower, upper_exclusive: bracket ? upper : null,
        unit: 'workflows/s', verified: report.outcome === 0,
        stop: stopOf('workflows/s', lower, bracket
          ? { level: upper, reasons: levels.find(l => l.rate === upper)?.reasons } : null, report.budget_reached),
      } : null,
      cleanup: cleanupOf(levels.map(l => l.cleanup)),
      workload: 'mixed-capacity',
      criteria: report.criteria ?? {},
      parameters: { plan: report.plan ?? null, budgets: report.budgets ?? null, fixture: report.fixture ?? null },
    }),
    dataset: report.dataset ?? null,
  }
}
