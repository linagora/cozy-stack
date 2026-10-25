// Runs one upload campaign through the exact-work runner (upload/exact-work.ts) against a local or
// shared DEV target (upload/targets.ts). MODE=regression runs one fixed workload; MODE=capacity
// searches levels (capacity/search.ts). Both stay within the campaign's budgets (README, Upload campaigns).
import { spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import type { UploadDataSize } from '../fixtures/upload-sizes.ts'
import type { UploadTargetKind } from '../lib/upload-target.ts'
import { buildGrafanaLink } from './campaign-summary.ts'
import {
  searchCapacity,
  type CapacityPhase,
  type CapacitySearchResult,
  type PointOutcome,
  type PointRequest,
  type PointResult,
  type ResultBound,
} from './capacity/search.ts'
import {
  ensureResourceCapacity,
  getCampaignConfig,
  type Budget,
  type CampaignConfig,
  type Mode,
} from './upload/campaign-config.ts'
import {
  harness as harnessDirectory,
  message,
  recover,
  runExactWork,
  trapInterrupts,
  type Cleanup,
  type Point,
  type Target,
} from './upload/exact-work.ts'
import { devTarget, localTarget } from './upload/targets.ts'

const resultsDirectory = join(harnessDirectory, 'data', 'results')

type CampaignStatus =
  | 'running'
  | 'completed'
  | 'regression_failure'
  | 'correctness_failure'
  | 'infrastructure_failure'
type Outcome = 0 | 1 | 2
type P95LimitMode = 'absolute' | 'none'
type Phase = CapacityPhase | 'regression'
type UploadRequest = Omit<PointRequest, 'phase'> & { phase: Phase }

type CampaignCriteria = {
  baseline_iterations: number
  consistency_chunk_size_bytes: number
  maximum_p95_ms: number | null
  minimum_success_rate: number
  maximum_dropped_iterations: number
  p95_limit_mode: P95LimitMode
  confirmation_iterations_per_vu: number
}

type CapacityAttempt = {
  phase: Phase
  vus: number
  iterations_per_vu: number
  p95_ms: number | null
  success_rate: number | null
  dropped_iterations: number | null
  completed_iterations: number | null
  cleanup_rate: number | null
  consistency_rate: number | null
  passed: boolean
  // stopped: the runtime budget ended the point, which then measured nothing.
  result: PointResult | 'stopped'
  reason: string
  runner_exit_code: number
  summary_path: string
  // The run's own data the runner deleted after k6, and what remained; null when unlistable.
  cleanup: Cleanup | null
}

type CapacityReport = {
  mode: Mode
  campaign_id: string
  load_target: Target['kind']
  target: string
  upload_target: UploadTargetKind
  file_size: UploadDataSize
  file_size_bytes: number
  budgets: {
    max_vus: number | null
    max_upload_bytes: number | null
    max_runtime: string | null
    upload_timeout: string | null
  }
  configured_max_vus: number
  criteria: CampaignCriteria
  status: CampaignStatus
  attempts: CapacityAttempt[]
  recovery: { k6_stopped: string[], cleaned: string[] } | null
  started_at: string
  ended_at?: string
  outcome?: Outcome
  versions?: Record<string, string>
  monitoring?: Record<string, string | null>
  baseline_p95_ms?: number
  p95_limit_ms?: number | null
  maximum_concurrent_uploads?: number
  result_bound?: ResultBound
  budget_reached?: Budget
  failure_reason?: string
}

type CampaignState = {
  attemptNumber: number
  config: CampaignConfig
  // No point starts after it, and a point running past it is stopped.
  deadline: number
  report: CapacityReport
  reportPath: string
  target: Target
  temporaryReportPath: string
}

// The runtime budget ended the campaign.
class BudgetReached extends Error {}
class CorrectnessFailure extends Error {}

function makeCapacityReport(
  config: CampaignConfig,
  target: Target,
  recovery: CapacityReport['recovery'],
): CapacityReport {
  return {
    mode: config.mode,
    campaign_id: config.campaignId,
    load_target: target.kind,
    target: target.baseUrl,
    upload_target: config.uploadTargetKind,
    file_size: config.fileSize,
    file_size_bytes: config.fileSizeBytes,
    budgets: {
      max_vus: config.budgets.maxVus,
      max_upload_bytes: config.budgets.maxUploadBytes,
      max_runtime: config.budgets.maxRuntime,
      upload_timeout: process.env.UPLOAD_TIMEOUT || null,
    },
    configured_max_vus: config.maxVus,
    criteria: {
      baseline_iterations: config.baselineIterations,
      consistency_chunk_size_bytes: config.consistencyChunkSizeBytes,
      maximum_p95_ms: config.configuredP95LimitMs,
      minimum_success_rate: config.minimumSuccessRate,
      maximum_dropped_iterations: 0,
      p95_limit_mode: config.configuredP95LimitMs === null ? 'none' : 'absolute',
      confirmation_iterations_per_vu: config.confirmationIterations,
    },
    status: 'running',
    attempts: [],
    recovery,
    started_at: new Date().toISOString(),
  }
}

function saveJsonFile(filePath: string, value: unknown): string {
  const nextFilePath = `${filePath}.next`

  writeFileSync(nextFilePath, `${JSON.stringify(value, null, 2)}\n`, {
    encoding: 'utf8',
    mode: 0o600,
  })
  renameSync(nextFilePath, filePath)
  return filePath
}

function makeCampaignState(
  config: CampaignConfig,
  target: Target,
  recovery: CapacityReport['recovery'],
): CampaignState {
  mkdirSync(resultsDirectory, { recursive: true })

  const state: CampaignState = {
    attemptNumber: 0,
    config,
    deadline: Date.now() + config.budgets.maxRuntimeMs,
    report: makeCapacityReport(config, target, recovery),
    reportPath: join(
      resultsDirectory,
      `${config.campaignId}-upload-${config.mode}.json`,
    ),
    target,
    temporaryReportPath: join(
      resultsDirectory,
      `.upload-${config.mode}-${randomUUID()}`,
    ),
  }

  saveJsonFile(state.temporaryReportPath, state.report)
  return state
}

function appendAttempt(
  state: CampaignState,
  attempt: CapacityAttempt,
): CapacityAttempt {
  state.report.attempts.push(attempt)
  saveJsonFile(state.temporaryReportPath, state.report)
  return attempt
}

// What a point proves on its own: a mismatch fails it, anything unverified leaves it inconclusive.
function settle(point: Point): PointResult {
  if (point.corrupted) {
    return 'failed'
  }

  return point.problems.length > 0 ? 'infrastructure_failure' : point.result
}

async function runUploadPoint(
  state: CampaignState,
  request: UploadRequest,
): Promise<Point> {
  if (Date.now() >= state.deadline) {
    throw new BudgetReached()
  }

  state.attemptNumber += 1

  const { config } = state
  const { iterationsPerVu, p95LimitMs, phase, vus } = request
  const executionId = `${config.campaignId}-${phase}-v${vus}-a${state.attemptNumber}`

  process.stdout.write(
    `\nRunning ${phase} point: ${vus} VUs x ${iterationsPerVu} upload(s)\n`,
  )

  const point = await runExactWork(state.target, {
    campaignId: config.campaignId,
    consistencyChunkSizeBytes: config.consistencyChunkSizeBytes,
    executionId,
    fileSize: config.fileSize,
    iterationsPerVu,
    limitMs: state.deadline - Date.now(),
    minimumSuccessRate: config.minimumSuccessRate,
    p95LimitMs,
    phase,
    vus,
  })
  // A correctness failure outweighs the budget that stopped its point.
  const stopped =
    !point.corrupted && point.k6.timed_out && Date.now() >= state.deadline
  const { metrics } = point

  appendAttempt(state, {
    phase,
    vus,
    iterations_per_vu: iterationsPerVu,
    p95_ms: metrics?.p95Ms ?? null,
    success_rate: metrics?.successRate ?? null,
    dropped_iterations: metrics?.droppedIterations ?? null,
    completed_iterations: metrics?.completedIterations ?? null,
    cleanup_rate: metrics?.cleanupRate ?? null,
    consistency_rate: metrics?.consistencyRate ?? null,
    passed: !stopped && settle(point) === 'passed',
    result: stopped ? 'stopped' : settle(point),
    reason: point.reason,
    runner_exit_code: point.k6.exit_code,
    summary_path: point.summary,
    cleanup: point.cleanup,
  })

  if (stopped) {
    throw new BudgetReached()
  }

  return point
}

async function readVersions(target: Target): Promise<Record<string, string>> {
  const output = (command: string, args: string[]): string =>
    spawnSync(command, args, { cwd: harnessDirectory, encoding: 'utf8' }).stdout?.trim() || 'unknown'
  const image = readFileSync(join(harnessDirectory, 'compose.yaml'), 'utf8')
    .match(/image: (grafana\/k6:\S+)/)?.[1]
  let stack = 'unknown'

  try {
    const { stack: client } = await target.session()
    stack = (await client?.('GET', '/version', [200]))?.version ?? 'unknown'
  } catch (_error: unknown) {
    // Unknown metadata stays explicit.
  }

  return {
    stack,
    harness: output('git', ['describe', '--always', '--dirty']),
    node: process.version,
    k6_image: image
      ? output('docker', ['image', 'inspect', '--format', '{{index .RepoDigests 0}}', image])
      : 'unknown',
  }
}

async function finishReport(
  state: CampaignState,
  status: CampaignStatus,
  outcome: Outcome,
  reason: string,
): Promise<Outcome> {
  const { report } = state

  report.status = status
  report.outcome = outcome
  report.ended_at = new Date().toISOString()

  if (reason.length > 0) {
    report.failure_reason = reason
  }

  report.versions = await readVersions(state.target)
  report.monitoring = {
    k6: buildGrafanaLink(process.env.GRAFANA_URL || 'http://localhost:3000', {
      name: `upload-${report.mode}`,
      kind: 'upload-capacity',
      id: report.campaign_id,
      startedAt: report.started_at,
      endedAt: report.ended_at,
      exitCode: outcome,
      producerExitCode: outcome,
    }),
    ...(report.load_target === 'dev'
      ? { dev: process.env.DEV_MONITORING_URL || 'unknown' }
      : {}),
  }
  saveJsonFile(state.temporaryReportPath, report)
  renameSync(state.temporaryReportPath, state.reportPath)

  const write = outcome === 0 ? process.stdout : process.stderr

  write.write(
    `Upload ${report.mode} outcome ${outcome}${reason ? ` (${reason})` : ''}: ${state.reportPath}\n`,
  )
  return outcome
}

async function runRegression(state: CampaignState): Promise<Outcome> {
  const { config } = state

  process.stdout.write(`Regression campaign: ${config.campaignId}\n`)

  let point: Point

  try {
    point = await runUploadPoint(state, {
      iterationsPerVu: config.iterationsPerVu,
      p95LimitMs: config.configuredP95LimitMs,
      phase: 'regression',
      vus: config.maxVus,
    })
  } catch (error: unknown) {
    if (error instanceof BudgetReached) {
      return finishReport(state, 'infrastructure_failure', 2, 'k6 ran past the MAX_RUNTIME budget')
    }

    throw error
  }

  // A proven failure keeps exit 1 even when cleanup also failed.
  if (point.corrupted) {
    return finishReport(state, 'correctness_failure', 1, point.reason)
  }
  if (point.result === 'failed') {
    return finishReport(state, 'regression_failure', 1, point.reason)
  }

  return settle(point) === 'passed'
    ? finishReport(state, 'completed', 0, '')
    : finishReport(state, 'infrastructure_failure', 2, point.reason)
}

// The runtime budget ended the search: only a level that passed confirmation measured capacity.
export function getBudgetResult(
  attempts: readonly Pick<CapacityAttempt, 'passed' | 'p95_ms' | 'phase' | 'vus'>[],
  p95LimitMs: number | null,
): CapacitySearchResult {
  const confirmed = attempts
    .filter(({ passed, phase }): boolean => passed && phase === 'confirmation')
    .map(({ vus }): number => vus)
  const baselineP95Ms = attempts.find(({ phase }): boolean => phase === 'baseline')?.p95_ms

  if (confirmed.length === 0 || baselineP95Ms === null || baselineP95Ms === undefined) {
    return {
      reason: 'the MAX_RUNTIME budget ran out before a level was confirmed',
      status: 'infrastructure_failure',
    }
  }

  return {
    baselineP95Ms,
    bound: 'lower_bound',
    maximumVus: Math.max(...confirmed),
    p95LimitMs,
    status: 'completed',
  }
}

async function runCapacitySearch(state: CampaignState): Promise<Outcome> {
  const { config, report } = state

  process.stdout.write(`Capacity campaign: ${config.campaignId}\n`)
  process.stdout.write(
    `File size: ${config.fileSize}; maximum search level: ${config.maxVus} VUs (${config.capBudget} budget)\n`,
  )

  let result: CapacitySearchResult
  let budget = config.capBudget

  try {
    result = await searchCapacity({
      baselineIterations: config.baselineIterations,
      configuredP95LimitMs: config.configuredP95LimitMs,
      confirmationIterations: config.confirmationIterations,
      log: (line: string): void => {
        process.stdout.write(line)
      },
      maxVus: config.maxVus,
      runPoint: async (request: PointRequest): Promise<PointOutcome> => {
        const point = await runUploadPoint(state, request)

        // A correctness failure ends the experiment at once: no level outweighs it.
        if (point.corrupted) {
          throw new CorrectnessFailure(point.reason)
        }

        return { p95Ms: point.metrics?.p95Ms ?? null, result: settle(point) }
      },
    })
  } catch (error: unknown) {
    if (error instanceof CorrectnessFailure) {
      return finishReport(state, 'correctness_failure', 1, error.message)
    }
    if (!(error instanceof BudgetReached)) {
      throw error
    }

    result = getBudgetResult(report.attempts, config.configuredP95LimitMs)
    budget = 'runtime'
  }

  if (result.status === 'infrastructure_failure') {
    return finishReport(state, 'infrastructure_failure', 2, result.reason)
  }

  report.baseline_p95_ms = result.baselineP95Ms
  report.p95_limit_ms = result.p95LimitMs
  report.maximum_concurrent_uploads = result.maximumVus
  report.result_bound = result.bound

  if (result.bound === 'lower_bound') {
    report.budget_reached = budget
  }

  const comparison = result.bound === 'lower_bound' ? '>=' : '='

  process.stdout.write(
    `\nResult for ${config.fileSize}: maximum concurrent uploads ${comparison} ${result.maximumVus}\n`,
  )
  return finishReport(state, 'completed', result.maximumVus === 0 ? 1 : 0, '')
}

function cleanupTemporaryReport(state: CampaignState | null): boolean {
  if (state === null) {
    return false
  }

  let removedFile = false

  for (const filePath of [
    state.temporaryReportPath,
    `${state.temporaryReportPath}.next`,
  ]) {
    if (existsSync(filePath)) {
      unlinkSync(filePath)
      removedFile = true
    }
  }

  return removedFile
}

async function runCampaign(): Promise<Outcome> {
  trapInterrupts()

  let state: CampaignState | null = null

  try {
    const config = getCampaignConfig()
    const target =
      config.loadTarget === 'dev' ? await devTarget(process.env) : localTarget(process.env)

    ensureResourceCapacity(config)

    const recovery = target.lockedUrls.length > 0 ? await recover(target) : null

    state = makeCampaignState(config, target, recovery)
    return config.mode === 'capacity'
      ? await runCapacitySearch(state)
      : await runRegression(state)
  } catch (error: unknown) {
    const reason = message(error)

    // Keep the attempts measured so far: the report is published before the
    // temporary copy is removed below. Setup problems are never correctness failures.
    if (state !== null) {
      return finishReport(state, 'infrastructure_failure', 2, reason)
    }

    process.stderr.write(`${reason}\n`)
    return 2
  } finally {
    cleanupTemporaryReport(state)
  }
}

function isMainModule(): boolean {
  const entryPath = process.argv[1]
  return entryPath !== undefined && resolve(entryPath) === fileURLToPath(import.meta.url)
}

if (isMainModule()) {
  runCampaign().then(
    (code: Outcome): void => {
      process.exitCode = code
    },
    (error: unknown): void => {
      process.stderr.write(`${message(error)}\n`)
      process.exitCode = 2
    },
  )
}
