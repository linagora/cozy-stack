import http from 'k6/http'
import { check } from 'k6'
import exec from 'k6/execution'

import {
  randomizeUploadData,
  uploadDataSize
} from '../fixtures/upload-data.js'

const ROOT_DIRECTORY_ID = 'io.cozy.files.root-dir'

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

function makeUploadName(marker) {
  return `load-${uploadDataSize.toLowerCase()}-${marker}.bin`
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

  const uploadBody = randomizeUploadData(marker)

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
        filesize: uploadDataSize,
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
