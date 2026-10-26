import { check, sleep } from 'k6'
import { md5 } from 'k6/crypto'
import exec from 'k6/execution'
import http from 'k6/http'
import { Counter, Rate } from 'k6/metrics'
import type { Options } from 'k6/options'
import ws from 'k6/ws'

import { randomizeUploadData } from '../fixtures/upload-data.ts'
import { fetchCreatedTestDirectory, type TestDirectory } from '../lib/cozy-files.ts'
import { formatAuthMessage, formatSubscribeMessage, parseRealtimeMessage } from '../lib/cozy-realtime.ts'
import { parseDurationToMs } from '../lib/duration.ts'
import { parseHosts, parsePositiveIntegerEnv } from '../lib/env.ts'
import { buildMixedThresholds, evaluateMixed, type MixedReport, WARMUP_GRACE_MS } from '../lib/mixed-workload.ts'
import { parseUploadedFile } from '../lib/upload-consistency.ts'

// A fixed arrival rate of workflows against a verified tree fixture (mixed-campaign.ts writes its IDs
// and expected metadata). Each workflow lists a directory, reads and checks a file's metadata and
// uploads a small file into the run's own directory; arrivals do not wait for slow responses.
// WebSocket observers stay connected apart from the arrival rate.

type Fixture = { dirs: string[], files: Array<{ id: string, md5: string, size: number }> }

const executionId = __ENV.EXECUTION_ID || 'mixed'
const fixture: Fixture = JSON.parse(open(`/results/${executionId}-mixed-fixture.json`))
const baseUrl = (__ENV.BASE_URL || '').replace(/\/+$/, '')
const headers = { Authorization: `Bearer ${__ENV.COZY_ACCESS_TOKEN}` }
const rate = parsePositiveIntegerEnv(__ENV, 'MIXED_RATE', 1)
const durationMs = parseDurationToMs(__ENV.MIXED_DURATION || '2m')
// A soak warms up at the same rate before its judged steady phase.
const warmupMs = parseDurationToMs(__ENV.MIXED_WARMUP || '0s')
// The steady phase starts once the warm-up's in-flight workflows are done or interrupted.
const graceMs = warmupMs ? WARMUP_GRACE_MS : 0
const maxVus = parsePositiveIntegerEnv(__ENV, 'MIXED_MAX_VUS', 10)
const observers = Number(__ENV.MIXED_OBSERVERS || '0')
const criteria = {
  minimumSuccessRate: Number(__ENV.MIN_SUCCESS_RATE || '0.99'),
  p95LimitMs: Number(__ENV.P95_LIMIT_MS || '2000'),
}
// An unreadable criterion would pass every comparison: refuse it before any request.
if (!(criteria.minimumSuccessRate > 0 && criteria.minimumSuccessRate <= 1) || !(criteria.p95LimitMs > 0)) {
  throw new Error('MIN_SUCCESS_RATE must be a ratio in (0, 1] and P95_LIMIT_MS a positive number')
}

export const metadataMatch = new Rate('mixed_metadata_match')
export const uploadMatch = new Rate('mixed_upload_match')
// Successful responses that could not be checked: never counted as verified.
export const unvalidated = new Counter('mixed_unvalidated_responses')
export const observerEvents = new Counter('mixed_observer_events')
export const observerSessions = new Counter('mixed_observer_sessions')

function arrivals(startMs: number, lengthMs: number) {
  return {
    executor: 'constant-arrival-rate',
    rate,
    timeUnit: '1s',
    startTime: `${startMs}ms`,
    duration: `${lengthMs}ms`,
    preAllocatedVUs: Math.min(maxVus, rate * 2),
    maxVUs: maxVus,
    exec: 'workflow',
  } as const
}

export const options: Options = {
  hosts: parseHosts(__ENV.ACTOR_HOSTS),
  scenarios: {
    ...(warmupMs > 0 ? { warmup: { ...arrivals(0, warmupMs), gracefulStop: `${graceMs}ms` } } : {}),
    steady: arrivals(warmupMs + graceMs, durationMs),
    ...(observers > 0 ? { observers: {
      executor: 'constant-vus', vus: observers, duration: `${warmupMs + graceMs + durationMs}ms`, exec: 'observe',
    } } : {}),
  },
  thresholds: buildMixedThresholds(),
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)', 'count'],
}

export function setup(): TestDirectory {
  return fetchCreatedTestDirectory({
    accessToken: __ENV.COZY_ACCESS_TOKEN || '',
    baseUrl,
    directoryName: `twake-load-mixed-${executionId}-${Date.now()}`,
    tags: { suite: 'mixed-workload' },
    target: { kind: 'personal', rootDirectoryId: 'io.cozy.files.root-dir' },
    timeout: '30s',
  })
}

function json(body: unknown): any {
  try {
    return JSON.parse(String(body))
  } catch {
    return null
  }
}

export function workflow(uploads: TestDirectory): void {
  const n = exec.scenario.iterationInTest
  const dir = fixture.dirs[n % fixture.dirs.length]!
  const file = fixture.files[n % fixture.files.length]!
  const listed = http.get(`${baseUrl}/files/${dir}`, {
    headers, tags: { name: 'GET /files/:dir-id', operation: 'list' },
  })
  check(listed, { 'directory listed': (r) => r.status === 200 })

  const read = http.get(`${baseUrl}/files/${file.id}`, {
    headers, tags: { name: 'GET /files/:file-id', operation: 'metadata' },
  })
  if (read.status === 200) {
    const attributes = json(read.body)?.data?.attributes
    if (attributes) metadataMatch.add(attributes.md5sum === file.md5 && Number(attributes.size) === file.size)
    else unvalidated.add(1, { operation: 'metadata' })
  }

  const marker = `${n}-${exec.vu.idInTest}-${Date.now()}-${Math.floor(Math.random() * 0xffffffff)}`
  const body = randomizeUploadData(marker)
  const checksum = md5(body, 'base64')
  const uploaded = http.post(`${baseUrl}/files/${uploads.id}?Type=file&Name=${marker}.bin`, body, {
    headers: { ...headers, 'Content-MD5': checksum, 'Content-Type': 'application/octet-stream' },
    tags: { name: 'POST /files/:dir-id', operation: 'upload' },
  })
  if (uploaded.status === 201) {
    let recorded = null
    try {
      recorded = parseUploadedFile(String(uploaded.body))
    } catch {
      unvalidated.add(1, { operation: 'upload' })
    }
    if (recorded) uploadMatch.add(recorded.md5sum === checksum && recorded.size === body.byteLength)
  }
}

// Each observer opens one session: a dropped one is recorded, never retried, so observer traffic stays
// within its budget.
let observed = false

export function observe(): void {
  const remainingMs = warmupMs + graceMs + durationMs - (Date.now() - exec.scenario.startTime)
  if (observed || remainingMs < 1000) {
    sleep(1)
    return
  }
  observed = true
  const res = ws.connect(`${baseUrl.replace(/^http/, 'ws')}/realtime/`, {}, (socket) => {
    socket.on('open', () => {
      observerSessions.add(1)
      socket.send(formatAuthMessage(__ENV.COZY_ACCESS_TOKEN || ''))
      socket.send(formatSubscribeMessage('io.cozy.files'))
    })
    socket.on('message', (raw: string) => {
      if (parseRealtimeMessage(raw)?.event === 'CREATED') observerEvents.add(1)
    })
    socket.setTimeout(() => socket.close(), remainingMs)
  })
  check(res, { 'observer connected': (r) => Boolean(r && r.status === 101) })
}

function format(report: MixedReport): string {
  const operations = Object.entries(report.operations).map(([name, op]) =>
    `  ${name}: ${op.rate_per_s} req/s, p95 ${op.p95_ms ?? 'n/a'} ms, errors ${op.error_rate ?? 'n/a'}`)
  return ['', `Mixed workload at ${report.plan.rate} workflows/s: ${report.status}`,
    `  achieved ${report.achieved_rate ?? 0} workflows/s, ${report.dropped_iterations} dropped`, ...operations,
    ...report.reasons.map((reason) => `  ${reason}`), ''].join('\n')
}

export function handleSummary(data: { metrics: Record<string, { values?: Record<string, number> }> }):
  Record<string, string> {
  const report = evaluateMixed(data.metrics, { durationS: durationMs / 1000, maxVus, observers, rate }, criteria)
  return {
    stdout: format(report),
    [`/results/${executionId}-mixed-workload.json`]: `${JSON.stringify(report, null, 2)}\n`,
  }
}
