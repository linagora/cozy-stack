import { existsSync, readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { isJsonRecord, type JsonRecord } from './capacity/k6-summary.ts'
import { CONTENT_MISMATCH_REASON, hasContentMismatch } from './capacity/upload-point.ts'

// Summarizes one campaign directory written by campaigns/run-campaign.sh:
// one line per step with its result, its outcome and a Grafana link for its
// time range. The exit status is the campaign outcome (README, Campaigns).

export type CampaignStepKind = 'upload-capacity' | 'shared-drive-ramp' | 'org-drive-fanout' | 'prepare'

export type CampaignStep = {
  endedAt: string
  exitCode: number
  id: string
  kind: CampaignStepKind | string
  name: string
  // The producer's own exit status, since make reports every failure as 2;
  // null when the producer never ran.
  producerExitCode: number | null
  startedAt: string
}

// 0: completed in the declared mode; 1: proven correctness failure;
// 2: inconclusive (setup, missing report, interruption, failed cleanup).
export type Outcome = 0 | 1 | 2

export type StepOutcome = { code: Outcome, reason: string }

// k6 exits 99 when a threshold fails. With cleanup verified, the only other
// failing ramp threshold is the stop on mostly failed HTTP requests: overload.
const K6_THRESHOLDS_FAILED = 99

export type CampaignSummaryInput = {
  environment: Record<string, string>
  grafanaUrl: string
  name: string
  results: Map<string, JsonRecord>
  runId: string
  steps: CampaignStep[]
}

const GRAFANA_MARGIN_MS = 60_000

const dashboards: Record<string, string> = {
  'shared-drive-ramp': 'twake-drive-shared-drive/twake-drive-shared-drive-activity',
  'upload-capacity': 'twake-drive-k6/twake-drive-k6-overview',
  'org-drive-fanout': 'twake-drive-capacity/stack-capacity-overview',
}

export function parseSteps(tsv: string): CampaignStep[] {
  return tsv
    .split('\n')
    .filter((line: string): boolean => line.trim().length > 0)
    .map((line: string): CampaignStep => {
      const [name = '', kind = '', id = '', startedAt = '', endedAt = '', exitCode = '', producerExitCode = ''] =
        line.split('\t')
      return {
        endedAt,
        exitCode: Number(exitCode),
        id,
        kind,
        name,
        producerExitCode: producerExitCode === '' ? null : Number(producerExitCode),
        startedAt,
      }
    })
}

export function parseEnvironment(text: string): Record<string, string> {
  const environment: Record<string, string> = {}

  for (const line of text.split('\n')) {
    const separator = line.indexOf('=')
    if (line.startsWith('#') || separator <= 0) {
      continue
    }
    environment[line.slice(0, separator)] = line.slice(separator + 1)
  }

  return environment
}

function describeUploadCapacity(result: JsonRecord): string {
  if (result.status !== 'completed') {
    return `stopped: ${String(result.failure_reason ?? result.status)}`
  }

  const maximum = String(result.maximum_concurrent_uploads)
  return result.result_bound === 'lower_bound'
    ? `maximum concurrent uploads ≥ ${maximum} (cap reached)`
    : `maximum concurrent uploads = ${maximum}`
}

function describeRamp(result: JsonRecord): string {
  if (result.status === 'infrastructure_failure' || result.max_sustained_level === null) {
    return `stopped: ${String(result.failure_reason ?? 'ramp capacity is unavailable')}`
  }
  const maximum = String(result.max_sustained_level)
  if (result.result_bound === 'lower_bound') {
    return `maximum sustained level ≥ ${maximum} (every level passed)`
  }

  const levels = Array.isArray(result.levels) ? result.levels.filter(isJsonRecord) : []
  const firstNotPassed = levels.find((level: JsonRecord): boolean => level.status !== 'passed')
  const reasons = Array.isArray(firstNotPassed?.reasons) ? firstNotPassed.reasons.join('; ') : ''
  const detail = firstNotPassed === undefined ? ''
    : `; level ${String(firstNotPassed.level)} ${String(firstNotPassed.status)}: ${reasons}`
  // Older reports called a failed sampled level "exact"; do not repeat that claim.
  const upper = result.first_failed_level ??
    (firstNotPassed?.status === 'failed' ? firstNotPassed.level : null)
  return typeof upper === 'number'
    ? `sustained level bracket [${maximum}, ${upper})${detail}`
    : `inconclusive; highest completed passing level ${maximum}${detail}`
}

type FanoutBound = { kind: 'lower_bound', lower: number } |
  { kind: 'bracket', lower: number, upper: number }

function fanoutBound(result: JsonRecord): FanoutBound | null {
  const capacity = isJsonRecord(result.capacity) ? result.capacity : {}
  const lower = capacity.lower
  const validLower = typeof lower === 'number' && Number.isSafeInteger(lower) && lower > 0
  if (result.status === 'passed' && capacity.kind === 'lower_bound' && validLower) {
    return { kind: 'lower_bound', lower }
  }
  const upper = capacity.upper_exclusive
  const validBracket = result.status === 'failed' && capacity.kind === 'bracket' && validLower &&
    typeof upper === 'number' && Number.isSafeInteger(upper) && upper > lower
  return validBracket ? { kind: 'bracket', lower, upper } : null
}

function describeFanout(result: JsonRecord): string {
  const bound = fanoutBound(result)
  if (bound?.kind === 'lower_bound') {
    return `member batch ≥ ${bound.lower} recipients (ready, access and revocation verified)`
  }
  if (bound?.kind === 'bracket') return `tested member batch bracket [${bound.lower}, ${bound.upper})`
  const points = Array.isArray(result.points) ? result.points.filter(isJsonRecord) : []
  const last = points.at(-1)
  if (!last) return 'inconclusive'
  const owner = isJsonRecord(last.owner) ? last.owner : {}
  const add = isJsonRecord(owner.add) ? owner.add : {}
  const recipients = Array.isArray(last.recipients) ? last.recipients.filter(isJsonRecord) : []
  const proofs = recipients.filter(r => isJsonRecord(r.add) && r.add.converged === true).length
  const count = (value: unknown): string => typeof value === 'number' &&
    Number.isSafeInteger(value) && value >= 0 ? String(value) : 'unknown'
  return `inconclusive; batch ${count(last.n)}; owner ready ${count(add.ready_recipients)}; ` +
    `recipient access proofs ${proofs}`
}

export function describeStepResult(step: CampaignStep, result: JsonRecord | null): string {
  if (step.kind === 'prepare' || step.id === '') {
    return step.exitCode === 0 ? 'done' : `failed (exit ${step.exitCode})`
  }
  if (result === null) {
    return `no result file (exit ${step.exitCode})`
  }

  if (step.kind === 'org-drive-fanout') return describeFanout(result)
  return step.kind === 'shared-drive-ramp' ? describeRamp(result) : describeUploadCapacity(result)
}

const completed: StepOutcome = { code: 0, reason: 'completed' }

// A search whose first level fails measured no capacity: like the upload search
// always has, every producer reports that as a failure (1).
function classifyUploadCapacity(result: JsonRecord): StepOutcome {
  const attempts = Array.isArray(result.attempts) ? result.attempts.filter(isJsonRecord) : []
  const stopped = result.status === 'completed' ? '' : String(result.failure_reason ?? result.status)
  if (hasContentMismatch(attempts)) {
    const reason = stopped.includes(CONTENT_MISMATCH_REASON) ? stopped
      : [CONTENT_MISMATCH_REASON, stopped].filter(Boolean).join('; ')
    return { code: 1, reason }
  }
  if (stopped !== '') return { code: 2, reason: stopped }
  return result.maximum_concurrent_uploads === 0
    ? { code: 1, reason: 'a single upload failed the criteria' }
    : completed
}

function classifyRamp(result: JsonRecord): StepOutcome {
  if (result.status !== 'completed') {
    return { code: 2, reason: String(result.failure_reason ?? result.status ?? 'ramp report has no status') }
  }
  if (result.result_bound === 'lower_bound') return completed
  if (result.result_bound !== 'bracket') return { code: 2, reason: 'ramp result is inconclusive' }
  return result.max_sustained_level === 0 ? { code: 1, reason: 'the first ramp level failed' } : completed
}

function classifyFanout(result: JsonRecord): StepOutcome {
  if (fanoutBound(result) !== null) return completed
  return result.status === 'failed'
    ? { code: 1, reason: 'the first fanout level missed its deadline' }
    : { code: 2, reason: String(result.error ?? 'fanout result is inconclusive') }
}

export function classifyStep(step: CampaignStep, result: JsonRecord | null): StepOutcome {
  const producers = ['upload-capacity', 'shared-drive-ramp', 'org-drive-fanout']
  if (!producers.includes(step.kind) || step.id === '') {
    return step.exitCode === 0 ? completed : { code: 2, reason: `step failed (exit ${step.exitCode})` }
  }
  const exit = step.producerExitCode ?? step.exitCode
  if (result === null) return { code: 2, reason: `no result file (exit ${exit})` }

  const outcome = step.kind === 'upload-capacity' ? classifyUploadCapacity(result)
    : step.kind === 'shared-drive-ramp' ? classifyRamp(result) : classifyFanout(result)
  // A completed report does not explain an unexpected producer failure.
  const explainedExit = exit === 0 || (step.kind === 'shared-drive-ramp' && exit === K6_THRESHOLDS_FAILED)
  if (outcome.code === 0 && !explainedExit) {
    return { code: 2, reason: `exit ${exit} although the report completed` }
  }
  return outcome
}

// Correctness failures take precedence; any other incomplete step, an
// interruption or an empty campaign leaves the run inconclusive.
export function campaignOutcome(steps: readonly StepOutcome[], interrupted: boolean): Outcome {
  if (steps.some(({ code }): boolean => code === 1)) return 1
  return interrupted || steps.length === 0 || steps.some(({ code }): boolean => code === 2) ? 2 : 0
}

const outcomeLabels: Record<Outcome, string> = { 0: 'completed', 1: 'correctness failure', 2: 'inconclusive' }

export function buildGrafanaLink(
  grafanaUrl: string, step: CampaignStep, campaignId = step.id,
): string | null {
  const dashboard = dashboards[step.kind]
  const from = Date.parse(step.startedAt)
  const to = Date.parse(step.endedAt)

  if (dashboard === undefined || step.id === '' || Number.isNaN(from) || Number.isNaN(to)) {
    return null
  }

  const base = grafanaUrl.replace(/\/+$/, '')
  if (step.kind === 'org-drive-fanout') {
    return `${base}/d/${dashboard}?var-campaign=${encodeURIComponent(campaignId)}` +
      `&from=${from - GRAFANA_MARGIN_MS}&to=${to + GRAFANA_MARGIN_MS}`
  }
  return `${base}/d/${dashboard}?var-campaignid=${encodeURIComponent(step.id)}&from=${from - GRAFANA_MARGIN_MS}&to=${to + GRAFANA_MARGIN_MS}`
}

export function evaluateCampaign(input: CampaignSummaryInput): { outcome: Outcome, steps: StepOutcome[] } {
  const steps = input.steps.map((step: CampaignStep): StepOutcome =>
    classifyStep(step, input.results.get(step.id) ?? null))
  return { outcome: campaignOutcome(steps, input.environment.interrupted !== undefined), steps }
}

export function formatCampaignSummary(input: CampaignSummaryInput): string {
  const { environment } = input
  const evaluation = evaluateCampaign(input)
  const rows = input.steps.map((step: CampaignStep, index: number): string => {
    const result = describeStepResult(step, input.results.get(step.id) ?? null)
    const { code, reason } = evaluation.steps[index]!
    const outcome = code === 0 ? '0' : `${code}: ${reason}`
    const link = buildGrafanaLink(input.grafanaUrl, step, step.kind === 'org-drive-fanout' ? input.runId : step.id)
    return `| ${step.name} | ${result} | ${outcome} |${link === null ? '' : ` [Grafana](${link})`} |`
  })

  return [
    `# Campaign ${input.name} (${input.runId})`,
    '',
    `- Outcome: ${evaluation.outcome} (${outcomeLabels[evaluation.outcome]})`,
    ...(environment.interrupted === undefined ? [] : [`- Interrupted by SIG${environment.interrupted}`]),
    `- Topology: ${environment.topology ?? 'unknown'}`,
    `- Commit: ${environment.commit ?? 'unknown'}`,
    `- Limits: ${environment.limits ?? 'unknown'}`,
    '',
    '| Step | Result | Outcome | Dashboard |',
    '| --- | --- | --- | --- |',
    ...rows,
    '',
  ].join('\n')
}

function resultPath(step: CampaignStep, results: string): string | null {
  if (step.kind === 'upload-capacity') {
    return join(results, `${step.id}-upload-capacity.json`)
  }
  if (step.kind === 'shared-drive-ramp') {
    return join(results, `${step.id}-shared-drive-ramp.json`)
  }
  if (step.kind === 'org-drive-fanout' && /^[a-zA-Z0-9][a-zA-Z0-9._-]*$/.test(step.id)) {
    return join(results, `${step.id}-org-drive-fanout.json`)
  }
  return null
}

function readResult(step: CampaignStep, results: string): JsonRecord | null {
  const path = resultPath(step, results)
  if (path === null || !existsSync(path)) {
    return null
  }

  try {
    const parsed: unknown = JSON.parse(readFileSync(path, 'utf8'))
    return isJsonRecord(parsed) ? parsed : null
  } catch (_error: unknown) {
    return null
  }
}

function summarizeCampaign(runDirectory: string): Outcome {
  const environment = parseEnvironment(readFileSync(join(runDirectory, 'environment.txt'), 'utf8'))
  const steps = parseSteps(readFileSync(join(runDirectory, 'steps.tsv'), 'utf8'))
  // Run directories live in data/results/campaigns/, next to the step reports.
  const resultsDirectory = resolve(runDirectory, '..', '..')
  const results = new Map<string, JsonRecord>()

  for (const step of steps) {
    const result = readResult(step, resultsDirectory)
    if (result !== null) {
      results.set(step.id, result)
    }
  }

  const input: CampaignSummaryInput = {
    environment,
    grafanaUrl: process.env.GRAFANA_URL || 'http://localhost:3000',
    name: environment.campaign ?? 'unknown',
    results,
    runId: environment.run_id ?? 'unknown',
    steps,
  }
  const summary = formatCampaignSummary(input)

  writeFileSync(join(runDirectory, 'summary.md'), summary, 'utf8')
  process.stdout.write(summary)
  return evaluateCampaign(input).outcome
}

function isMainModule(): boolean {
  const entryPath = process.argv[1]
  return entryPath !== undefined && resolve(entryPath) === fileURLToPath(import.meta.url)
}

if (isMainModule()) {
  const runDirectory = process.argv[2]
  if (runDirectory === undefined) {
    process.stderr.write('Usage: campaign-summary.ts <campaign directory>\n')
    process.exitCode = 2
  } else {
    try {
      process.exitCode = summarizeCampaign(resolve(runDirectory))
    } catch (error: unknown) {
      // Without a summary the run is unclassified, never a correctness failure.
      process.stderr.write(`Campaign summary failed: ${error instanceof Error ? error.message : String(error)}\n`)
      process.exitCode = 2
    }
  }
}
