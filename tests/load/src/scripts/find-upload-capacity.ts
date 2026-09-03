import { spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import {
  existsSync,
  mkdirSync,
  readFileSync,
  renameSync,
  statfsSync,
  statSync,
  unlinkSync,
  writeFileSync,
} from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  getUploadBinaryFilename,
  getUploadDataSizeBytes,
  isUploadDataSize,
  uploadDataSizes,
  type UploadDataSize,
} from '../fixtures/upload-sizes.ts'
import {
  getUploadTarget,
  type UploadTarget,
  type UploadTargetKind,
} from '../lib/upload-target.ts'

const MINIMUM_FREE_DISK_BYTES = 5n * 1024n * 1024n * 1024n
const ONE_GIBIBYTE = 1024n * 1024n * 1024n
const scriptDirectory = dirname(fileURLToPath(import.meta.url))
const harnessDirectory = resolve(scriptDirectory, '..', '..')
const resultsDirectory = join(harnessDirectory, 'data', 'results')

type PointResult = 'passed' | 'failed' | 'infrastructure_failure'
export type ResultBound = 'exact' | 'lower_bound'
type CampaignStatus = 'running' | 'completed' | 'infrastructure_failure'
type P95LimitMode = 'absolute' | 'baseline_multiplier'
type CapacityPhase =
  | 'baseline'
  | 'discovery'
  | 'discovery-retry'
  | 'discovery-decider'
  | 'confirmation'

type CampaignCriteria = {
  baseline_iterations: number
  consistency_chunk_size_bytes: number
  maximum_p95_ms: number | null
  minimum_success_rate: number
  maximum_dropped_iterations: number
  p95_baseline_multiplier: number | null
  p95_limit_mode: P95LimitMode
  confirmation_iterations_per_vu: number
}

type CapacityAttempt = {
  phase: CapacityPhase
  vus: number
  iterations_per_vu: number
  p95_ms: number | null
  success_rate: number | null
  dropped_iterations: number | null
  completed_iterations: number | null
  cleanup_rate: number | null
  passed: boolean
  result: PointResult
  reason: string
  runner_exit_code: number
  summary_path: string
}

type CapacityReport = {
  campaign_id: string
  target: string
  upload_target: UploadTargetKind
  file_size: UploadDataSize
  file_size_bytes: number
  configured_max_vus: number
  criteria: CampaignCriteria
  status: CampaignStatus
  attempts: CapacityAttempt[]
  baseline_p95_ms?: number
  p95_limit_ms?: number
  maximum_concurrent_uploads?: number
  result_bound?: ResultBound
  failure_reason?: string
}

type CampaignConfig = {
  baselineIterations: number
  baseUrl: string
  campaignId: string
  confirmationIterations: number
  configuredP95LimitMs: number | null
  consistencyChunkSizeBytes: number
  fileSize: UploadDataSize
  fileSizeBytes: number
  forceCapacityRun: boolean
  latencyMultiplier: number
  maxVus: number
  minimumSuccessRate: number
  uploadTarget: UploadTarget
}

type CampaignState = {
  attemptNumber: number
  config: CampaignConfig
  report: CapacityReport
  reportPath: string
  temporaryReportPath: string
}

type PointMetrics = {
  cleanupRate: number
  completedIterations: number
  droppedIterations: number
  p95Ms: number
  successRate: number
}

type PointOutcome = {
  p95Ms: number | null
  result: PointResult
}

type JsonRecord = Record<string, unknown>

const defaultMaximumVirtualUsers: Record<UploadDataSize, number> = {
  '1K': 256,
  '100K': 256,
  '1M': 128,
  '10M': 32,
  '100M': 8,
  '1G': 2,
}

function isJsonRecord(value: unknown): value is JsonRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function getMetricNumber(
  summary: JsonRecord,
  metricNames: readonly string[],
  fieldName: string,
): number | null {
  const metrics = summary.metrics

  if (!isJsonRecord(metrics)) {
    return null
  }

  for (const metricName of metricNames) {
    const metric = metrics[metricName]

    if (!isJsonRecord(metric)) {
      continue
    }

    const value = metric[fieldName]

    if (typeof value === 'number' && Number.isFinite(value)) {
      return value
    }
  }

  return null
}

function getPointMetrics(summaryPath: string): PointMetrics | null {
  try {
    const parsedSummary: unknown = JSON.parse(readFileSync(summaryPath, 'utf8'))

    if (!isJsonRecord(parsedSummary)) {
      return null
    }

    const p95Ms = getMetricNumber(
      parsedSummary,
      ['http_req_duration{operation:file-upload}', 'http_req_duration'],
      'p(95)',
    )
    const successRate = getMetricNumber(
      parsedSummary,
      ['upload_success{operation:file-upload}', 'upload_success'],
      'value',
    )
    const completedIterations = getMetricNumber(
      parsedSummary,
      ['iterations'],
      'count',
    )
    const cleanupRate = getMetricNumber(
      parsedSummary,
      ['cleanup_success'],
      'value',
    )
    const droppedIterations =
      getMetricNumber(parsedSummary, ['dropped_iterations'], 'count') ?? 0

    if (
      p95Ms === null ||
      successRate === null ||
      completedIterations === null ||
      cleanupRate === null
    ) {
      return null
    }

    return {
      cleanupRate,
      completedIterations,
      droppedIterations,
      p95Ms,
      successRate,
    }
  } catch (_error: unknown) {
    return null
  }
}

function getPositiveInteger(environmentName: string, value: string): number {
  if (!/^[1-9][0-9]*$/.test(value)) {
    throw new Error(`${environmentName} must be a positive integer`)
  }

  const parsedValue = Number(value)

  if (!Number.isSafeInteger(parsedValue)) {
    throw new Error(`${environmentName} must be a safe positive integer`)
  }

  return parsedValue
}

function getPositiveNumber(environmentName: string, value: string): number {
  const parsedValue = Number(value)

  if (!Number.isFinite(parsedValue) || parsedValue <= 0) {
    throw new Error(`${environmentName} must be a positive number`)
  }

  return parsedValue
}

function getOptionalPositiveNumber(
  environmentName: string,
  value: string | undefined,
): number | null {
  if (!value) {
    return null
  }

  return getPositiveNumber(environmentName, value)
}

function getMinimumSuccessRate(value: string): number {
  const parsedValue = Number(value)

  if (!Number.isFinite(parsedValue) || parsedValue <= 0 || parsedValue > 1) {
    throw new Error('MIN_SUCCESS_RATE must be greater than 0 and at most 1')
  }

  return parsedValue
}

function makeDefaultCampaignId(): string {
  const timestamp = new Date()
    .toISOString()
    .replace(/[-:]/g, '')
    .replace(/\.\d{3}/, '')

  return `${timestamp}-upload-capacity-${process.pid}`
}

export function computeP95LimitMs(
  baselineP95Ms: number,
  latencyMultiplier: number,
  configuredP95LimitMs: number | null,
): number {
  return configuredP95LimitMs ?? baselineP95Ms * latencyMultiplier
}

function getCampaignConfig(): CampaignConfig {
  const configuredFileSize = process.env.FILE_SIZE ?? '1K'

  if (!isUploadDataSize(configuredFileSize)) {
    throw new Error(
      `Unknown FILE_SIZE ${configuredFileSize}. Expected one of: ${uploadDataSizes.join(' ')}`,
    )
  }

  const configuredMaxVus = process.env.MAX_VUS
  const maxVus = getPositiveInteger(
    'MAX_VUS',
    configuredMaxVus && configuredMaxVus.length > 0
      ? configuredMaxVus
      : String(defaultMaximumVirtualUsers[configuredFileSize]),
  )
  const confirmationIterations = getPositiveInteger(
    'CONFIRMATION_ITERATIONS',
    process.env.CONFIRMATION_ITERATIONS || '20',
  )

  return {
    baselineIterations: getPositiveInteger(
      'BASELINE_ITERATIONS',
      process.env.BASELINE_ITERATIONS || '50',
    ),
    baseUrl: process.env.BASE_URL || 'http://mock',
    campaignId: process.env.CAPACITY_RUN_ID || makeDefaultCampaignId(),
    confirmationIterations,
    configuredP95LimitMs: getOptionalPositiveNumber(
      'P95_LIMIT_MS',
      process.env.P95_LIMIT_MS,
    ),
    consistencyChunkSizeBytes: getPositiveInteger(
      'CONSISTENCY_CHUNK_SIZE_BYTES',
      process.env.CONSISTENCY_CHUNK_SIZE_BYTES || String(16 * 1024 * 1024),
    ),
    fileSize: configuredFileSize,
    fileSizeBytes: getUploadDataSizeBytes(configuredFileSize),
    forceCapacityRun: process.env.FORCE_CAPACITY_RUN === '1',
    latencyMultiplier: getPositiveNumber(
      'LATENCY_MULTIPLIER',
      process.env.LATENCY_MULTIPLIER || '2',
    ),
    maxVus,
    minimumSuccessRate: getMinimumSuccessRate(
      process.env.MIN_SUCCESS_RATE || '0.99',
    ),
    uploadTarget: getUploadTarget(process.env),
  }
}

function ensureResourceCapacity(config: CampaignConfig): true {
  const uploadBinaryPath = join(
    harnessDirectory,
    'data',
    'upload-binaries',
    getUploadBinaryFilename(config.fileSize),
  )
  const availableDiskStats = statfsSync(harnessDirectory, { bigint: true })
  const availableDiskBytes =
    availableDiskStats.bavail * availableDiskStats.bsize
  const uploadBinaryBytes =
    existsSync(uploadBinaryPath) &&
    statSync(uploadBinaryPath).size === config.fileSizeBytes
      ? 0n
      : BigInt(config.fileSizeBytes)
  const baselineBytes =
    BigInt(config.fileSizeBytes) * BigInt(config.baselineIterations)
  const confirmationBytes =
    BigInt(config.fileSizeBytes) *
    BigInt(config.maxVus) *
    BigInt(config.confirmationIterations)
  const projectedDiskBytes =
    uploadBinaryBytes +
    (baselineBytes > confirmationBytes ? baselineBytes : confirmationBytes)

  if (
    !config.forceCapacityRun &&
    availableDiskBytes - projectedDiskBytes < MINIMUM_FREE_DISK_BYTES
  ) {
    throw new Error(
      [
        'Projected campaign data would leave less than 5 GiB free.',
        `Available bytes: ${availableDiskBytes}; projected bytes: ${projectedDiskBytes}`,
        'Set FORCE_CAPACITY_RUN=1 only after verifying load-generator and Cozy storage.',
      ].join('\n'),
    )
  }

  const dockerInfo = spawnSync(
    'docker',
    ['info', '--format', '{{.MemTotal}}'],
    { encoding: 'utf8' },
  )
  const dockerMemoryOutput = dockerInfo.stdout.trim()

  if (dockerInfo.status !== 0 || !/^[1-9][0-9]*$/.test(dockerMemoryOutput)) {
    throw new Error('Could not determine Docker memory. Is Docker running?')
  }

  const dockerMemoryBytes = BigInt(dockerMemoryOutput)
  const consistencyBufferBytes = BigInt(
    Math.min(config.fileSizeBytes, config.consistencyChunkSizeBytes),
  )
  const projectedMemoryBytes =
    ONE_GIBIBYTE +
    (3n * BigInt(config.fileSizeBytes) + consistencyBufferBytes) *
      BigInt(config.maxVus)

  if (!config.forceCapacityRun && projectedMemoryBytes > dockerMemoryBytes) {
    throw new Error(
      [
        'Projected k6 memory exceeds the memory available to Docker.',
        `Docker bytes: ${dockerMemoryBytes}; projected bytes: ${projectedMemoryBytes}`,
        'Set FORCE_CAPACITY_RUN=1 only after verifying the load generator.',
      ].join('\n'),
    )
  }

  return true
}

function makeCapacityReport(config: CampaignConfig): CapacityReport {
  return {
    campaign_id: config.campaignId,
    target: config.baseUrl,
    upload_target: config.uploadTarget.kind,
    file_size: config.fileSize,
    file_size_bytes: config.fileSizeBytes,
    configured_max_vus: config.maxVus,
    criteria: {
      baseline_iterations: config.baselineIterations,
      consistency_chunk_size_bytes: config.consistencyChunkSizeBytes,
      maximum_p95_ms: config.configuredP95LimitMs,
      minimum_success_rate: config.minimumSuccessRate,
      maximum_dropped_iterations: 0,
      p95_baseline_multiplier:
        config.configuredP95LimitMs === null ? config.latencyMultiplier : null,
      p95_limit_mode:
        config.configuredP95LimitMs === null
          ? 'baseline_multiplier'
          : 'absolute',
      confirmation_iterations_per_vu: config.confirmationIterations,
    },
    status: 'running',
    attempts: [],
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

function makeCampaignState(config: CampaignConfig): CampaignState {
  mkdirSync(resultsDirectory, { recursive: true })

  const state: CampaignState = {
    attemptNumber: 0,
    config,
    report: makeCapacityReport(config),
    reportPath: join(
      resultsDirectory,
      `${config.campaignId}-upload-capacity.json`,
    ),
    temporaryReportPath: join(
      resultsDirectory,
      `.upload-capacity-${randomUUID()}`,
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

function makeAttempt(
  phase: CapacityPhase,
  vus: number,
  iterationsPerVu: number,
  summaryName: string,
  runnerExitCode: number,
  result: PointResult,
  reason: string,
  metrics: PointMetrics | null,
): CapacityAttempt {
  return {
    phase,
    vus,
    iterations_per_vu: iterationsPerVu,
    p95_ms: metrics?.p95Ms ?? null,
    success_rate: metrics?.successRate ?? null,
    dropped_iterations: metrics?.droppedIterations ?? null,
    completed_iterations: metrics?.completedIterations ?? null,
    cleanup_rate: metrics?.cleanupRate ?? null,
    passed: result === 'passed',
    result,
    reason,
    runner_exit_code: runnerExitCode,
    summary_path: `data/results/${summaryName}`,
  }
}

function runCapacityPoint(
  state: CampaignState,
  phase: CapacityPhase,
  vus: number,
  iterationsPerVu: number,
  p95LimitMs: number | null,
): PointOutcome {
  state.attemptNumber += 1

  const { config } = state
  const executionId = `${config.campaignId}-${phase}-v${vus}-a${state.attemptNumber}`
  const summaryName = `${executionId}-summary.json`
  const summaryPath = join(resultsDirectory, summaryName)
  const expectedIterations = vus * iterationsPerVu

  process.stdout.write(
    `\nRunning ${phase} point: ${vus} VUs x ${iterationsPerVu} upload(s)\n`,
  )

  const makeArguments = [
    '-C',
    harnessDirectory,
    'upload',
    `BASE_URL=${config.baseUrl}`,
    `FILE_SIZE=${config.fileSize}`,
    `VUS=${vus}`,
    'UPLOAD_MODE=iterations',
    `ITERATIONS_PER_VU=${iterationsPerVu}`,
    `CONSISTENCY_CHUNK_SIZE_BYTES=${config.consistencyChunkSizeBytes}`,
    `CAPACITY_RUN_ID=${config.campaignId}`,
    `CAPACITY_PHASE=${phase}`,
    `MIN_SUCCESS_RATE=${config.minimumSuccessRate}`,
    `P95_LIMIT_MS=${p95LimitMs ?? ''}`,
    `EXECUTION_ID=${executionId}`,
    `UPLOAD_TARGET=${config.uploadTarget.kind}`,
  ]

  if (config.uploadTarget.kind === 'shared-drive') {
    makeArguments.push(
      `SHARED_DRIVE_ID=${config.uploadTarget.driveId}`,
      `SHARED_DRIVE_ROOT_ID=${config.uploadTarget.rootDirectoryId}`,
    )
  }

  const makeResult = spawnSync(
    'make',
    makeArguments,
    { stdio: 'inherit' },
  )
  const runnerExitCode = makeResult.status ?? 2

  if (!existsSync(summaryPath) || statSync(summaryPath).size === 0) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'infrastructure_failure',
        'k6 produced no summary',
        null,
      ),
    )
    return { p95Ms: null, result: 'infrastructure_failure' }
  }

  const metrics = getPointMetrics(summaryPath)

  if (metrics === null) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'infrastructure_failure',
        'summary is missing required metrics',
        null,
      ),
    )
    return { p95Ms: null, result: 'infrastructure_failure' }
  }

  if (metrics.cleanupRate !== 1) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'infrastructure_failure',
        'test-directory cleanup failed',
        metrics,
      ),
    )
    return { p95Ms: metrics.p95Ms, result: 'infrastructure_failure' }
  }

  if (
    isInterruptedCapacityPoint(
      runnerExitCode,
      metrics.completedIterations,
      expectedIterations,
    )
  ) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'infrastructure_failure',
        'k6 stopped before completing every upload consistency check',
        metrics,
      ),
    )
    return { p95Ms: metrics.p95Ms, result: 'infrastructure_failure' }
  }

  if (metrics.completedIterations === 0) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'infrastructure_failure',
        'k6 completed no upload iterations',
        metrics,
      ),
    )
    return { p95Ms: metrics.p95Ms, result: 'infrastructure_failure' }
  }

  let failureReason = ''

  if (
    metrics.completedIterations !== expectedIterations ||
    metrics.droppedIterations !== 0
  ) {
    failureReason = 'iterations were dropped or incomplete'
  } else if (metrics.successRate < config.minimumSuccessRate) {
    failureReason = 'upload success rate is below the minimum'
  } else if (p95LimitMs !== null && metrics.p95Ms > p95LimitMs) {
    failureReason = 'upload p95 exceeds the configured limit'
  }

  if (failureReason.length > 0) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'failed',
        failureReason,
        metrics,
      ),
    )
    return { p95Ms: metrics.p95Ms, result: 'failed' }
  }

  if (runnerExitCode !== 0) {
    appendAttempt(
      state,
      makeAttempt(
        phase,
        vus,
        iterationsPerVu,
        summaryName,
        runnerExitCode,
        'infrastructure_failure',
        'k6 exited unsuccessfully even though measured criteria passed',
        metrics,
      ),
    )
    return { p95Ms: metrics.p95Ms, result: 'infrastructure_failure' }
  }

  appendAttempt(
    state,
    makeAttempt(
      phase,
      vus,
      iterationsPerVu,
      summaryName,
      runnerExitCode,
      'passed',
      '',
      metrics,
    ),
  )
  return { p95Ms: metrics.p95Ms, result: 'passed' }
}

export function isInterruptedCapacityPoint(
  runnerExitCode: number,
  completedIterations: number,
  expectedIterations: number,
): boolean {
  return runnerExitCode !== 0 && completedIterations !== expectedIterations
}

function evaluateBurstLevel(
  state: CampaignState,
  vus: number,
  p95LimitMs: number,
): PointOutcome {
  const firstOutcome = runCapacityPoint(state, 'discovery', vus, 1, p95LimitMs)

  if (
    firstOutcome.result === 'passed' ||
    firstOutcome.result === 'infrastructure_failure'
  ) {
    return firstOutcome
  }

  process.stdout.write(`Retrying failing discovery point at ${vus} VUs\n`)

  const secondOutcome = runCapacityPoint(
    state,
    'discovery-retry',
    vus,
    1,
    p95LimitMs,
  )

  if (secondOutcome.result === 'infrastructure_failure') {
    return secondOutcome
  }
  if (secondOutcome.result === firstOutcome.result) {
    return firstOutcome
  }

  process.stdout.write(`Running deciding discovery point at ${vus} VUs\n`)
  return runCapacityPoint(state, 'discovery-decider', vus, 1, p95LimitMs)
}

function evaluateConfirmationLevel(
  state: CampaignState,
  vus: number,
  p95LimitMs: number,
): PointOutcome {
  return runCapacityPoint(
    state,
    'confirmation',
    vus,
    state.config.confirmationIterations,
    p95LimitMs,
  )
}

export function getCapacityResultBound(
  candidateVus: number,
  configuredMaxVus: number,
  firstFailingVus: number,
): ResultBound {
  return candidateVus === configuredMaxVus && firstFailingVus === 0
    ? 'lower_bound'
    : 'exact'
}

function finishReport(
  state: CampaignState,
  baselineP95Ms: number,
  p95LimitMs: number,
  maximumConcurrency: number,
  bound: ResultBound,
): string {
  state.report.status = 'completed'
  state.report.baseline_p95_ms = baselineP95Ms
  state.report.p95_limit_ms = p95LimitMs
  state.report.maximum_concurrent_uploads = maximumConcurrency
  state.report.result_bound = bound
  saveJsonFile(state.temporaryReportPath, state.report)
  renameSync(state.temporaryReportPath, state.reportPath)

  if (bound === 'lower_bound') {
    process.stdout.write(
      `\nResult for ${state.config.fileSize}: maximum concurrent uploads >= ${maximumConcurrency}\n`,
    )
  } else {
    process.stdout.write(
      `\nResult for ${state.config.fileSize}: maximum concurrent uploads = ${maximumConcurrency}\n`,
    )
  }
  process.stdout.write(`Capacity report: ${state.reportPath}\n`)
  return state.reportPath
}

function abortCampaign(state: CampaignState, reason: string): number {
  state.report.status = 'infrastructure_failure'
  state.report.failure_reason = reason
  saveJsonFile(state.temporaryReportPath, state.report)
  renameSync(state.temporaryReportPath, state.reportPath)
  process.stderr.write(`Capacity report: ${state.reportPath}\n`)
  process.stderr.write(`Capacity campaign stopped: ${reason}\n`)
  return 2
}

function runCapacitySearch(state: CampaignState): number {
  const { config } = state

  process.stdout.write(`Capacity campaign: ${config.campaignId}\n`)
  process.stdout.write(
    `File size: ${config.fileSize}; maximum search level: ${config.maxVus} VUs\n`,
  )

  const baselineOutcome = runCapacityPoint(
    state,
    'baseline',
    1,
    config.baselineIterations,
    null,
  )

  if (baselineOutcome.result === 'infrastructure_failure') {
    return abortCampaign(
      state,
      'baseline failed because of test infrastructure',
    )
  }
  if (baselineOutcome.p95Ms === null) {
    return abortCampaign(state, 'baseline produced no p95 measurement')
  }

  const baselineP95Ms = baselineOutcome.p95Ms
  const p95LimitMs = computeP95LimitMs(
    baselineP95Ms,
    config.latencyMultiplier,
    config.configuredP95LimitMs,
  )

  if (baselineOutcome.result === 'failed') {
    finishReport(state, baselineP95Ms, p95LimitMs, 0, 'exact')
    return 1
  }

  let lastPassingVus = 1
  let firstFailingVus = 0
  let currentVus = 2

  while (currentVus <= config.maxVus) {
    const burstOutcome = evaluateBurstLevel(state, currentVus, p95LimitMs)

    if (burstOutcome.result === 'infrastructure_failure') {
      return abortCampaign(
        state,
        `discovery failed because of test infrastructure at ${currentVus} VUs`,
      )
    }

    if (burstOutcome.result === 'passed') {
      lastPassingVus = currentVus

      if (currentVus === config.maxVus) {
        break
      }

      currentVus = Math.min(currentVus * 2, config.maxVus)
    } else {
      firstFailingVus = currentVus
      break
    }
  }

  if (firstFailingVus > 0) {
    let searchLow = lastPassingVus + 1
    let searchHigh = firstFailingVus - 1

    while (searchLow <= searchHigh) {
      const searchMid = Math.floor((searchLow + searchHigh) / 2)
      const burstOutcome = evaluateBurstLevel(state, searchMid, p95LimitMs)

      if (burstOutcome.result === 'infrastructure_failure') {
        return abortCampaign(
          state,
          `binary discovery failed because of test infrastructure at ${searchMid} VUs`,
        )
      }

      if (burstOutcome.result === 'passed') {
        lastPassingVus = searchMid
        searchLow = searchMid + 1
      } else {
        searchHigh = searchMid - 1
      }
    }
  }

  const candidateVus = lastPassingVus
  const confirmationOutcome = evaluateConfirmationLevel(
    state,
    candidateVus,
    p95LimitMs,
  )

  if (confirmationOutcome.result === 'infrastructure_failure') {
    return abortCampaign(
      state,
      `confirmation failed because of test infrastructure at ${candidateVus} VUs`,
    )
  }

  if (confirmationOutcome.result === 'passed') {
    const resultBound = getCapacityResultBound(
      candidateVus,
      config.maxVus,
      firstFailingVus,
    )

    finishReport(state, baselineP95Ms, p95LimitMs, candidateVus, resultBound)
    return 0
  }

  let confirmedVus = 0
  let searchLow = 1
  let searchHigh = candidateVus - 1

  while (searchLow <= searchHigh) {
    const searchMid = Math.floor((searchLow + searchHigh) / 2)
    const searchOutcome = evaluateConfirmationLevel(
      state,
      searchMid,
      p95LimitMs,
    )

    if (searchOutcome.result === 'infrastructure_failure') {
      return abortCampaign(
        state,
        `confirmation search failed because of test infrastructure at ${searchMid} VUs`,
      )
    }

    if (searchOutcome.result === 'passed') {
      confirmedVus = searchMid
      searchLow = searchMid + 1
    } else {
      searchHigh = searchMid - 1
    }
  }

  finishReport(state, baselineP95Ms, p95LimitMs, confirmedVus, 'exact')
  return confirmedVus === 0 ? 1 : 0
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

function runCapacityCampaign(): number {
  let state: CampaignState | null = null

  try {
    const config = getCampaignConfig()

    ensureResourceCapacity(config)
    state = makeCampaignState(config)
    return runCapacitySearch(state)
  } catch (error: unknown) {
    const reason = error instanceof Error ? error.message : String(error)

    if (state !== null) {
      return abortCampaign(state, reason)
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
  process.exitCode = runCapacityCampaign()
}
