import { check } from 'k6'
import { createHash, md5 } from 'k6/crypto'
import exec from 'k6/execution'
import { Rate } from 'k6/metrics'

import type { Options } from 'k6/options'

import { randomizeUploadData, uploadDataSize } from '../fixtures/upload-data.ts'
import {
  fetchCreatedTestDirectory,
  fetchDownloadRangeResponse,
  fetchDestroyResponse,
  fetchTrashResponse,
  fetchUploadResponse,
  type RunTags,
  type TestDirectory,
} from '../lib/cozy-files.ts'
import {
  makeByteRange,
  parseUploadedFile,
  type UploadedFile,
} from '../lib/upload-consistency.ts'
import { getUploadTarget } from '../lib/upload-target.ts'

function getPositiveInteger(environmentName: string, fallback: number): number {
  const value = Number(__ENV[environmentName] || fallback)

  if (!Number.isInteger(value) || value < 1) {
    throw new Error(`${environmentName} must be a positive integer`)
  }

  return value
}

function getSuccessRate(): number {
  const value = Number(__ENV.MIN_SUCCESS_RATE || 0.99)

  if (!Number.isFinite(value) || value <= 0 || value > 1) {
    throw new Error('MIN_SUCCESS_RATE must be greater than 0 and at most 1')
  }

  return value
}

function getOptionalP95Limit(): number | null {
  if (!__ENV.P95_LIMIT_MS) {
    return null
  }

  const value = Number(__ENV.P95_LIMIT_MS)

  if (!Number.isFinite(value) || value <= 0) {
    throw new Error('P95_LIMIT_MS must be a positive number')
  }

  return value
}

function getRequiredEnvironmentValue(environmentName: string): string {
  const value = __ENV[environmentName]

  if (!value) {
    throw new Error(`${environmentName} is required`)
  }

  return value
}

const uploadVirtualUsers = getPositiveInteger('UPLOAD_VUS', 1)
const consistencyChunkSize = getPositiveInteger(
  'CONSISTENCY_CHUNK_SIZE_BYTES',
  16 * 1024 * 1024,
)
const uploadSuccessRate = getSuccessRate()
const uploadP95Limit = getOptionalP95Limit()
const uploadTarget = getUploadTarget(__ENV)
const uploadSuccess = new Rate('upload_success')
const cleanupSuccess = new Rate('cleanup_success')

const uploadThresholdTags = '{operation:file-upload}'
const thresholds: NonNullable<Options['thresholds']> = {
  cleanup_success: ['rate==1'],
  dropped_iterations: ['count==0'],
  [`http_req_duration${uploadThresholdTags}`]: [
    uploadP95Limit === null ? 'p(95)>=0' : `p(95)<=${uploadP95Limit}`,
  ],
  [`http_req_failed${uploadThresholdTags}`]: [`rate<=${1 - uploadSuccessRate}`],
  [`upload_success${uploadThresholdTags}`]: [`rate>=${uploadSuccessRate}`],
}

export const options: Options = {
  discardResponseBodies: true,
  scenarios: {
    concurrent_uploads: {
      executor: 'per-vu-iterations',
      vus: uploadVirtualUsers,
      iterations: getPositiveInteger('ITERATIONS_PER_VU', 1),
      maxDuration: __ENV.MAX_DURATION || '30m',
    },
  },
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)'],
  thresholds,
}

function makeRunTags(): RunTags {
  return {
    campaignid: __ENV.CAPACITY_RUN_ID || __ENV.TEST_ID || 'upload',
    concurrency: String(uploadVirtualUsers),
    filesize: uploadDataSize,
    phase: __ENV.CAPACITY_PHASE || 'single-run',
    target: uploadTarget.kind,
    testid: __ENV.TEST_ID || 'upload',
  }
}

export function setup(): TestDirectory {
  const baseUrl = getRequiredEnvironmentValue('BASE_URL')
  const accessToken = getRequiredEnvironmentValue('COZY_ACCESS_TOKEN')

  const directoryName = [
    'twake-load',
    uploadDataSize.toLowerCase(),
    __ENV.TEST_ID || 'upload',
    Date.now(),
  ].join('-')

  return fetchCreatedTestDirectory({
    accessToken,
    baseUrl,
    directoryName,
    tags: makeRunTags(),
    target: uploadTarget,
    timeout: __ENV.UPLOAD_TIMEOUT || '30m',
  })
}

function makeUploadName(marker: string): string {
  return `load-${uploadDataSize.toLowerCase()}-${marker}.bin`
}

function abortUploadConsistency(message: string): never {
  const abortMessage = `Upload consistency check failed: ${message}`

  exec.test.abort(abortMessage)
  throw new Error(abortMessage)
}

interface DownloadChecksumRequest {
  accessToken: string
  baseUrl: string
  file: UploadedFile
}

function fetchDownloadedChecksum({
  accessToken,
  baseUrl,
  file,
}: DownloadChecksumRequest): string {
  const hasher = createHash('md5')
  let offset = 0

  while (offset < file.size) {
    const range = makeByteRange(offset, file.size, consistencyChunkSize)

    if (range === null) {
      abortUploadConsistency(
        `could not construct a range at offset ${offset} for ${file.id}`,
      )
    }

    const response = fetchDownloadRangeResponse({
      accessToken,
      baseUrl,
      end: range.end,
      fileId: file.id,
      start: range.start,
      tags: makeRunTags(),
      target: uploadTarget,
      timeout: __ENV.UPLOAD_TIMEOUT || '30m',
    })

    if (response.status !== 206) {
      abortUploadConsistency(
        `download range ${range.header} for ${file.id} returned status ${response.status}, expected 206`,
      )
    }
    if (response.body.byteLength !== range.length) {
      abortUploadConsistency(
        `download range ${range.header} for ${file.id} returned ${response.body.byteLength} bytes, expected ${range.length}`,
      )
    }

    const expectedContentRange = `bytes ${range.start}-${range.end}/${file.size}`

    if (response.headers['Content-Range'] !== expectedContentRange) {
      abortUploadConsistency(
        `download range ${range.header} for ${file.id} returned Content-Range ${response.headers['Content-Range'] || '<missing>'}, expected ${expectedContentRange}`,
      )
    }

    hasher.update(response.body)
    offset = range.end + 1
  }

  return hasher.digest('base64')
}

// k6 requires a default export for the virtual-user entry point.
export default function runConcurrentUploadsScenario(
  testDirectory: TestDirectory,
): null {
  const baseUrl = getRequiredEnvironmentValue('BASE_URL')
  const accessToken = getRequiredEnvironmentValue('COZY_ACCESS_TOKEN')
  const marker = [
    exec.scenario.iterationInTest,
    exec.vu.idInTest,
    Date.now(),
    Math.floor(Math.random() * 0xffffffff),
    __ENV.TEST_ID || 'upload',
  ].join('-')
  const filename = makeUploadName(marker)
  const uploadBody = randomizeUploadData(marker)
  const expectedChecksum = md5(uploadBody, 'base64')
  const response = fetchUploadResponse({
    accessToken,
    baseUrl,
    body: uploadBody,
    contentMD5: expectedChecksum,
    directoryId: testDirectory.id,
    filename,
    tags: makeRunTags(),
    target: uploadTarget,
    timeout: __ENV.UPLOAD_TIMEOUT || '30m',
  })

  const isUploadSuccessful = check(response, {
    'upload status is 201': (result): boolean => result.status === 201,
  })

  uploadSuccess.add(isUploadSuccessful, {
    ...makeRunTags(),
    operation: 'file-upload',
  })

  if (!isUploadSuccessful) {
    return null
  }

  let uploadedFile: UploadedFile

  try {
    uploadedFile = parseUploadedFile(response.body)
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error)
    abortUploadConsistency(message)
  }

  if (uploadedFile.name !== filename) {
    abortUploadConsistency(
      `upload response returned name ${uploadedFile.name}, expected ${filename}`,
    )
  }
  if (uploadedFile.size !== uploadBody.byteLength) {
    abortUploadConsistency(
      `upload response returned size ${uploadedFile.size}, expected ${uploadBody.byteLength}`,
    )
  }
  if (uploadedFile.md5sum !== expectedChecksum) {
    abortUploadConsistency(
      `upload response returned checksum ${uploadedFile.md5sum}, expected ${expectedChecksum}`,
    )
  }

  const downloadedChecksum = fetchDownloadedChecksum({
    accessToken,
    baseUrl,
    file: uploadedFile,
  })

  if (downloadedChecksum !== expectedChecksum) {
    abortUploadConsistency(
      `downloaded checksum ${downloadedChecksum} for ${uploadedFile.id} does not match uploaded checksum ${expectedChecksum}`,
    )
  }

  return null
}

export function teardown(testDirectory: TestDirectory): null {
  if (!testDirectory.id) {
    cleanupSuccess.add(false, makeRunTags())
    throw new Error('Cannot clean up load-test directory: setup returned no id')
  }

  const requestOptions = {
    accessToken: getRequiredEnvironmentValue('COZY_ACCESS_TOKEN'),
    baseUrl: getRequiredEnvironmentValue('BASE_URL'),
    directoryId: testDirectory.id,
    tags: makeRunTags(),
    target: uploadTarget,
    timeout: __ENV.UPLOAD_TIMEOUT || '30m',
  }
  const trashResponse = fetchTrashResponse(requestOptions)

  if (trashResponse.status !== 200) {
    cleanupSuccess.add(false, makeRunTags())
    throw new Error(
      `Could not trash load-test directory ${testDirectory.id}: expected status 200, got ${trashResponse.status}`,
    )
  }

  const destroyResponse = fetchDestroyResponse(requestOptions)
  const isCleanupSuccessful = destroyResponse.status === 204

  cleanupSuccess.add(isCleanupSuccessful, makeRunTags())

  if (!isCleanupSuccessful) {
    throw new Error(
      `Could not permanently delete load-test directory ${testDirectory.id}: expected status 204, got ${destroyResponse.status}`,
    )
  }

  return null
}
