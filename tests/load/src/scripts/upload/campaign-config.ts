// Reads an upload campaign's settings and budgets, and checks them and the load generator before
// the first request (README, Upload campaigns).
import { spawnSync } from 'node:child_process'
import { existsSync, statfsSync, statSync } from 'node:fs'
import { join } from 'node:path'

import {
  getUploadBinaryFilename,
  getUploadDataSizeBytes,
  isUploadDataSize,
  uploadDataSizes,
  type UploadDataSize,
} from '../../fixtures/upload-sizes.ts'
import { readCapacitySteps } from '../../lib/capacity-steps.ts'
import { parseDurationToMs } from '../../lib/duration.ts'
import { getUploadTarget, type UploadTargetKind } from '../../lib/upload-target.ts'
import { harness as harnessDirectory, type Target } from './exact-work.ts'

const MINIMUM_FREE_DISK_BYTES = 5n * 1024n * 1024n * 1024n
const ONE_GIBIBYTE = 1024n * 1024n * 1024n

export type Mode = 'regression' | 'capacity'
// The budget behind a lower bound.
// levels: the search reached CAPACITY_MAX, below what the budgets allow.
export type Budget = 'concurrency' | 'upload bytes' | 'runtime' | 'levels'

export type CampaignConfig = {
  mode: Mode
  loadTarget: Target['kind']
  baselineIterations: number
  budgets: {
    maxRuntime: string | null
    maxRuntimeMs: number
    maxUploadBytes: number | null
    maxVus: number | null
  }
  campaignId: string
  // The budget that set maxVus in capacity mode.
  capBudget: Budget
  confirmationIterations: number
  configuredP95LimitMs: number | null
  consistencyChunkSizeBytes: number
  fileSize: UploadDataSize
  fileSizeBytes: number
  forceCapacityRun: boolean
  // Regression only.
  iterationsPerVu: number
  // The highest search level in capacity mode, the fixed workers in regression mode.
  maxVus: number
  // The capacity search's levels (lib/capacity-steps.ts), up to maxVus.
  levels: number[]
  minimumSuccessRate: number
  uploadTargetKind: UploadTargetKind
}

const defaultMaximumVirtualUsers: Record<UploadDataSize, number> = {
  '1K': 256,
  '100K': 256,
  '1M': 128,
  '10M': 32,
  '100M': 8,
  '1G': 2,
}

export function getPositiveInteger(environmentName: string, value: string): number {
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

// 1024-based, like FILE_SIZE: 10M is 10 MiB.
export function getBytes(environmentName: string, value: string): number {
  const match = /^([1-9][0-9]*)([KMG]?)$/.exec(value)

  if (match === null) {
    throw new Error(`${environmentName} must be a byte count such as 10M`)
  }

  return Number(match[1]) * 1024 ** ' KMG'.indexOf(match[2] || ' ')
}

export function getDurationMs(environmentName: string, value: string): number {
  try {
    return parseDurationToMs(value)
  } catch (_error: unknown) {
    throw new Error(`${environmentName} must be a duration such as 30m`)
  }
}

function makeDefaultCampaignId(mode: Mode): string {
  const timestamp = new Date()
    .toISOString()
    .replace(/[-:]/g, '')
    .replace(/\.\d{3}/, '')

  return `${timestamp}-upload-${mode}-${process.pid}`
}

// Every setting and budget is checked here, before the first request.
export function getCampaignConfig(): CampaignConfig {
  const environment = process.env
  const mode = environment.MODE || 'capacity'
  const loadTarget = environment.LOAD_TARGET || 'local'

  if (mode !== 'capacity' && mode !== 'regression') {
    throw new Error('MODE must be capacity or regression')
  }
  if (loadTarget !== 'local' && loadTarget !== 'dev') {
    throw new Error('LOAD_TARGET must be local or dev')
  }

  const configuredFileSize = environment.FILE_SIZE ?? '1K'

  if (!isUploadDataSize(configuredFileSize)) {
    throw new Error(
      `Unknown FILE_SIZE ${configuredFileSize}. Expected one of: ${uploadDataSizes.join(' ')}`,
    )
  }

  const fileSizeBytes = getUploadDataSizeBytes(configuredFileSize)
  const budgets = {
    maxRuntime: environment.MAX_RUNTIME || null,
    maxRuntimeMs: environment.MAX_RUNTIME
      ? getDurationMs('MAX_RUNTIME', environment.MAX_RUNTIME)
      : Infinity,
    maxUploadBytes: environment.MAX_UPLOAD_BYTES
      ? getBytes('MAX_UPLOAD_BYTES', environment.MAX_UPLOAD_BYTES)
      : null,
    maxVus: environment.MAX_VUS
      ? getPositiveInteger('MAX_VUS', environment.MAX_VUS)
      : null,
  }
  const baselineIterations = getPositiveInteger(
    'BASELINE_ITERATIONS',
    environment.BASELINE_ITERATIONS || '50',
  )
  const confirmationIterations = getPositiveInteger(
    'CONFIRMATION_ITERATIONS',
    environment.CONFIRMATION_ITERATIONS || '20',
  )
  const fitsBytes = (vus: number, iterations: number): boolean =>
    budgets.maxUploadBytes === null ||
    vus * iterations * fileSizeBytes <= budgets.maxUploadBytes
  let maxVus: number
  let levels: number[] = []
  let capBudget: Budget = 'concurrency'
  let iterationsPerVu = 1

  if (mode === 'regression') {
    maxVus = getPositiveInteger('VUS', environment.VUS || '1')
    iterationsPerVu = getPositiveInteger(
      'ITERATIONS_PER_VU',
      environment.ITERATIONS_PER_VU || '1',
    )

    if (budgets.maxVus !== null && maxVus > budgets.maxVus) {
      throw new Error('VUS exceeds the MAX_VUS budget')
    }
    if (!fitsBytes(maxVus, iterationsPerVu)) {
      throw new Error(
        'VUS x ITERATIONS_PER_VU x FILE_SIZE exceeds the MAX_UPLOAD_BYTES budget',
      )
    }
  } else {
    const vusCap = budgets.maxVus ?? defaultMaximumVirtualUsers[configuredFileSize]
    // A confirmation point is the most a level uploads at once.
    const bytesCap =
      budgets.maxUploadBytes === null
        ? Infinity
        : Math.floor(budgets.maxUploadBytes / (fileSizeBytes * confirmationIterations))

    if (!fitsBytes(1, baselineIterations) || bytesCap < 1) {
      throw new Error(
        'MAX_UPLOAD_BYTES is below the baseline or a 1-VU confirmation point',
      )
    }

    maxVus = Math.min(vusCap, bytesCap)
    capBudget = bytesCap < vusCap ? 'upload bytes' : 'concurrency'
    levels = readCapacitySteps(environment, { max: maxVus }).levels
    if (levels.at(-1)! > maxVus) {
      throw new Error(`CAPACITY_MAX exceeds the ${capBudget} budget of ${maxVus} VUs`)
    }
    if (levels.at(-1)! < maxVus) capBudget = 'levels'
    maxVus = levels.at(-1)!
  }

  const campaignId =
    (mode === 'capacity' ? environment.CAPACITY_RUN_ID : environment.EXECUTION_ID) ||
    makeDefaultCampaignId(mode)

  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(campaignId)) {
    throw new Error('CAPACITY_RUN_ID and EXECUTION_ID must be file-name-safe IDs')
  }

  return {
    mode,
    loadTarget,
    baselineIterations,
    budgets,
    campaignId,
    capBudget,
    confirmationIterations,
    configuredP95LimitMs: getOptionalPositiveNumber(
      'P95_LIMIT_MS',
      environment.P95_LIMIT_MS,
    ),
    consistencyChunkSizeBytes: getPositiveInteger(
      'CONSISTENCY_CHUNK_SIZE_BYTES',
      environment.CONSISTENCY_CHUNK_SIZE_BYTES || String(16 * 1024 * 1024),
    ),
    fileSize: configuredFileSize,
    fileSizeBytes,
    forceCapacityRun: environment.FORCE_CAPACITY_RUN === '1',
    iterationsPerVu,
    maxVus,
    levels,
    minimumSuccessRate: getMinimumSuccessRate(
      environment.MIN_SUCCESS_RATE || '0.99',
    ),
    uploadTargetKind: getUploadTarget(environment).kind,
  }
}

export function ensureResourceCapacity(config: CampaignConfig): true {
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
  // The largest point: the regression run, or the baseline or a confirmation at the cap.
  const largestPointBytes =
    BigInt(config.fileSizeBytes) *
    BigInt(
      config.mode === 'regression'
        ? config.maxVus * config.iterationsPerVu
        : Math.max(config.baselineIterations, config.maxVus * config.confirmationIterations),
    )
  const projectedDiskBytes = uploadBinaryBytes + largestPointBytes

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
