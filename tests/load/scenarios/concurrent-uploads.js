import http from 'k6/http'
import { check } from 'k6'
import exec from 'k6/execution'

const ROOT_DIRECTORY_ID = 'io.cozy.files.root-dir'
const FIXTURE_FILENAMES = {
  '1K': '1k.bin',
  '100K': '100k.bin',
  '1M': '1m.bin',
  '10M': '10m.bin',
  '100M': '100m.bin',
  '1G': '1g.bin'
}
const MUTATION_SIZE = 256

const fileSize = __ENV.FILE_SIZE || '1K'
const fixtureFilename = FIXTURE_FILENAMES[fileSize]

if (!fixtureFilename) {
  throw new Error(
    `Unknown FILE_SIZE ${fileSize}. Expected one of: ${Object.keys(FIXTURE_FILENAMES).join(', ')}`
  )
}

const fixture = open(`/fixtures/${fixtureFilename}`, 'b')
const fixtureBytes = new Uint8Array(fixture)

function getPositiveInteger(environmentName, fallback) {
  const value = Number(__ENV[environmentName] || fallback)

  if (!Number.isInteger(value) || value < 1) {
    throw new Error(`${environmentName} must be a positive integer`)
  }

  return value
}

export const options = {
  discardResponseBodies: true,
  scenarios: {
    concurrent_uploads: {
      executor: 'per-vu-iterations',
      vus: getPositiveInteger('UPLOAD_VUS', 1),
      iterations: getPositiveInteger('ITERATIONS_PER_VU', 1),
      maxDuration: __ENV.MAX_DURATION || '30m'
    }
  },
  thresholds: {
    checks: ['rate>0.99'],
    http_req_failed: ['rate<0.01']
  }
}

export function setup() {
  if (!__ENV.BASE_URL) {
    throw new Error('BASE_URL is required')
  }
  if (!__ENV.COZY_ACCESS_TOKEN) {
    throw new Error('COZY_ACCESS_TOKEN is required')
  }

  return null
}

function randomizeFixture(marker) {
  const mutationLength = Math.min(MUTATION_SIZE, fixtureBytes.length)
  const uniqueHeader = `twake-load:${marker}\n`
  let state = 2166136261

  for (let index = 0; index < marker.length; index += 1) {
    state ^= marker.charCodeAt(index)
    state = Math.imul(state, 16777619)
  }

  for (let index = 0; index < mutationLength; index += 1) {
    state ^= state << 13
    state ^= state >>> 17
    state ^= state << 5
    fixtureBytes[index] =
      index < uniqueHeader.length
        ? uniqueHeader.charCodeAt(index) & 0xff
        : state & 0xff
  }

  return fixture
}

function makeUploadName(marker) {
  return `load-${fileSize.toLowerCase()}-${marker}.bin`
}

// k6 requires a default export for the virtual-user entry point.
export default function runConcurrentUploadsScenario() {
  const marker = [
    exec.scenario.iterationInTest,
    exec.vu.idInTest,
    Date.now(),
    Math.floor(Math.random() * 0xffffffff),
    __ENV.TEST_ID || 'upload'
  ].join('-')
  const filename = makeUploadName(marker)

  const uploadBody = randomizeFixture(marker)

  const response = http.post(
    `${__ENV.BASE_URL}/files/${ROOT_DIRECTORY_ID}?Type=file&Name=${encodeURIComponent(filename)}`,
    uploadBody,
    {
      headers: {
        Accept: 'application/vnd.api+json',
        Authorization: `Bearer ${__ENV.COZY_ACCESS_TOKEN}`,
        'Content-Type': 'application/octet-stream'
      },
      tags: {
        filesize: fileSize,
        name: 'POST /files/:dir-id',
        operation: 'file-upload'
      },
      timeout: __ENV.UPLOAD_TIMEOUT || '30m'
    }
  )

  check(response, {
    'upload status is 201': result => result.status === 201
  })
}
