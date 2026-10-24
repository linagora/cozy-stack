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
  classifyUploadPoint,
  hasContentMismatch,
  readUploadPointMetrics,
  type UploadPointMetrics,
} from './capacity/upload-point.ts'

const MINIMUM_FREE_DISK_BYTES = 5n * 1024n * 1024n * 1024n
const ONE_GIBIBYTE = 1024n * 1024n * 1024n
const scriptDirectory = dirname(fileURLToPath(import.meta.url))
const harnessDirectory = resolve(scriptDirectory, '..', '..')
const resultsDirectory = join(harnessDirectory, 'data', 'results')

type CampaignStatus = 'running' | 'completed' | 'infrastructure_failure'
type P95LimitMode = 'absolute' | 'none'

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
  phase: CapacityPhase
  vus: number
  iterations_per_vu: number
  p95_ms: number | null
  success_rate: number | null
  dropped_iterations: number | null
  completed_iterations: number | null
  cleanup_rate: number | null
  consistency_rate: number | null
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
  p95_limit_ms?: number | null
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

const defaultMaximumVirtualUsers: Record<UploadDataSize, number> = {
  '1K': 256,
  '100K': 256,
  '1M': 128,
  '10M': 32,
  '100M': 8,
  '1G': 2,
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
      p95_limit_mode: config.configuredP95LimitMs === null ? 'none' : 'absolute',
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

function readSummaryMetrics(summaryPath: string): UploadPointMetrics | null {
  try {
    return readUploadPointMetrics(JSON.parse(readFileSync(summaryPath, 'utf8')))
  } catch (_error: unknown) {
    return null
  }
}

function runUploadPoint(
  state: CampaignState,
  request: PointRequest,
): PointOutcome {
  state.attemptNumber += 1

  const { config } = state
  const { iterationsPerVu, p95LimitMs, phase, vus } = request
  const executionId = `${config.campaignId}-${phase}-v${vus}-a${state.attemptNumber}`
  const summaryName = `${executionId}-summary.json`
  const summaryPath = join(resultsDirectory, summaryName)

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

  const makeResult = spawnSync('make', makeArguments, { stdio: 'inherit' })
  const runnerExitCode = makeResult.status ?? 2
  const summaryFound =
    existsSync(summaryPath) && statSync(summaryPath).size > 0
  const metrics = summaryFound ? readSummaryMetrics(summaryPath) : null
  const { reason, result } = classifyUploadPoint({
    expectedIterations: vus * iterationsPerVu,
    metrics,
    minimumSuccessRate: config.minimumSuccessRate,
    p95LimitMs,
    runnerExitCode,
    summaryFound,
  })

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
    passed: result === 'passed',
    result,
    reason,
    runner_exit_code: runnerExitCode,
    summary_path: `data/results/${summaryName}`,
  })
  return { p95Ms: metrics?.p95Ms ?? null, result }
}

function finishReport(
  state: CampaignState,
  result: Extract<CapacitySearchResult, { status: 'completed' }>,
): string {
  state.report.status = 'completed'
  state.report.baseline_p95_ms = result.baselineP95Ms
  state.report.p95_limit_ms = result.p95LimitMs
  state.report.maximum_concurrent_uploads = result.maximumVus
  state.report.result_bound = result.bound
  saveJsonFile(state.temporaryReportPath, state.report)
  renameSync(state.temporaryReportPath, state.reportPath)

  const comparison = result.bound === 'lower_bound' ? '>=' : '='

  process.stdout.write(
    `\nResult for ${state.config.fileSize}: maximum concurrent uploads ${comparison} ${result.maximumVus}\n`,
  )
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
  return hasContentMismatch(state.report.attempts) ? 1 : 2
}

function runCapacitySearch(state: CampaignState): number {
  const { config } = state

  process.stdout.write(`Capacity campaign: ${config.campaignId}\n`)
  process.stdout.write(
    `File size: ${config.fileSize}; maximum search level: ${config.maxVus} VUs\n`,
  )

  const result = searchCapacity({
    baselineIterations: config.baselineIterations,
    configuredP95LimitMs: config.configuredP95LimitMs,
    confirmationIterations: config.confirmationIterations,
    log: (message: string): void => {
      process.stdout.write(message)
    },
    maxVus: config.maxVus,
    runPoint: (request: PointRequest): PointOutcome =>
      runUploadPoint(state, request),
  })

  if (result.status === 'infrastructure_failure') {
    return abortCampaign(state, result.reason)
  }

  finishReport(state, result)
  return result.maximumVus === 0 || hasContentMismatch(state.report.attempts) ? 1 : 0
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

    // Keep the attempts measured so far: abortCampaign publishes the report
    // before the temporary copy is removed below.
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
