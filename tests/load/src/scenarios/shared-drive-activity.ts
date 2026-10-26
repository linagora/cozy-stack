import { check, sleep } from 'k6'
import { md5 } from 'k6/crypto'
import exec from 'k6/execution'
import http from 'k6/http'
import { Counter, Rate, Trend } from 'k6/metrics'
import type { Options } from 'k6/options'
import ws from 'k6/ws'

import { randomizeUploadData } from '../fixtures/upload-data.ts'
import {
  buildChangesFeedUrl,
  parseChangesFeedResponse,
  type ChangesFeedItem,
} from '../lib/cozy-changes.ts'
import {
  fetchCreatedTestDirectory,
  fetchDestroyResponse,
  fetchTrashResponse,
  fetchUploadResponse,
  type TestDirectory,
} from '../lib/cozy-files.ts'
import {
  formatAuthMessage,
  getSharedDriveWsUrl,
  parseRealtimeMessage,
  type RealtimeEventPayload,
} from '../lib/cozy-realtime.ts'
import {
  getWsSessionDurationMs,
  parseDurationToMs,
} from '../lib/duration.ts'
import { parsePositiveIntegerEnv } from '../lib/env.ts'
import {
  getCompletedRampLevels,
  getRampPhaseAt,
  getRampTotalDurationMs,
  getRampUploadPrefix,
  getServedRampPhase,
  parseRampCriteria,
  parseRampUploadLevel,
  RAMP_EVENT_GRACE_MS,
  type RampPhase,
  type RampPlan,
} from '../lib/load-profile.ts'
import {
  LevelObservations,
  observedWholeHold,
  parseUploadMarker,
} from '../lib/reader-observations.ts'
import { parseUploadedFile } from '../lib/upload-consistency.ts'
import {
  getUploadTarget,
  makeUploadTargetDownloadUrl,
  type SharedDriveUploadTarget,
} from '../lib/upload-target.ts'

// --- Metrics ---
export const cleanupSuccess = new Rate('cleanup_success')
export const rampHoldCompleted = new Counter('shared_drive_ramp_hold_completed')
let completedRampLevelCount = 0

export const uploadSuccessRate = new Rate('shared_drive_upload_success_rate')

export const wsHandshakeSuccessRate = new Rate('shared_drive_ws_handshake_rate')
export const wsAuthSuccessRate = new Rate('shared_drive_ws_auth_rate')
export const wsMessagesReceived = new Counter('shared_drive_ws_messages_total')
export const wsEventsReceived = new Counter('shared_drive_ws_events_total')
export const wsErrorsReceived = new Counter('shared_drive_ws_errors_total')

export const changesFeedDuration = new Trend('shared_drive_changes_duration_ms', true)
export const changesFeedSuccessRate = new Rate('shared_drive_changes_success_rate')
export const changesItemsReceived = new Counter('shared_drive_changes_items_total')
export const changesPending = new Trend('shared_drive_changes_pending')
export const changesMalformed = new Counter('shared_drive_changes_malformed_total')

// Per reader and level: the unique uploads it saw, judged against the level's uploads.
export const wsReaderUnique = new Trend('shared_drive_ws_reader_unique')
export const changesReaderUnique = new Trend('shared_drive_changes_reader_unique')
export const contentVerified = new Counter('shared_drive_content_verified_total')
export const contentMismatch = new Counter('shared_drive_content_mismatch_total')
export const contentDenied = new Counter('shared_drive_content_denied_total')

// --- Configuration ---
const rawDuration = __ENV.DURATION || '30s'
const rawWritersVus = parsePositiveIntegerEnv(__ENV, 'WRITER_VUS', 3)
const rawWsReadersVus = parsePositiveIntegerEnv(__ENV, 'WS_READER_VUS', 5)
const rawChangesReadersVus = parsePositiveIntegerEnv(__ENV, 'CHANGES_READER_VUS', 3)
const { contentChecksPerLevel } = parseRampCriteria(__ENV)

// Member actors (drive-actors.ts): k6 reaches their domains, which only the Stack serves, at its address.
export const actorHosts: Record<string, string> = Object.fromEntries(
  (__ENV.ACTOR_HOSTS || '').split(',').filter(Boolean).map((entry: string) => entry.split('=')),
)

export const options: Options = {
  hosts: actorHosts,
  scenarios: {
    writers: {
      executor: 'constant-vus',
      vus: rawWritersVus,
      duration: rawDuration,
      exec: 'writerLoop',
    },
    ws_readers: {
      executor: 'constant-vus',
      vus: rawWsReadersVus,
      duration: rawDuration,
      exec: 'wsReaderLoop',
    },
    changes_readers: {
      executor: 'constant-vus',
      vus: rawChangesReadersVus,
      duration: rawDuration,
      exec: 'changesReaderLoop',
    },
  },
  thresholds: {
    cleanup_success: ['rate==1'],
    shared_drive_upload_success_rate: ['rate>0.95'],
    shared_drive_ws_handshake_rate: ['rate>0.95'],
    shared_drive_ws_auth_rate: ['rate>0.95'],
    shared_drive_changes_success_rate: ['rate>0.95'],
    shared_drive_ws_events_total: ['count>0'],
    shared_drive_changes_items_total: ['count>0'],
  },
}

interface Credentials {
  accessToken: string
  baseUrl: string
}

// The owner creates and removes the test directory; with member actors, the writer recipient uploads
// and the read-only recipient observes. Without them, the owner plays every role.
export interface SetupData extends Credentials {
  writer: Credentials
  reader: Credentials
  driveId: string
  rampPlan: RampPlan | null
  rootDirectoryId: string
  testDir: TestDirectory
  testDurationMs: number
  wsSessionDurationMs: number
}

// shared-drive-ramp.ts reuses these loops and passes its plan here; a null
// plan keeps the constant-VU behaviour of this entry point.
export function createSetupData(rampPlan: RampPlan | null): SetupData {
  const accessToken = __ENV.COZY_ACCESS_TOKEN
  if (!accessToken) {
    throw new Error('COZY_ACCESS_TOKEN environment variable is required')
  }

  const baseUrl = __ENV.COZY_BASE_URL || __ENV.BASE_URL || 'http://localhost:8080'
  const uploadTarget = getUploadTarget(__ENV)

  if (uploadTarget.kind !== 'shared-drive') {
    throw new Error('Expected shared-drive upload target')
  }

  // The owned prefix and execution ID let the runner clean up only this run's data.
  const directoryName = `twake-load-sd-${__ENV.EXECUTION_ID || 'shared-drive'}-${Date.now()}`
  const testDir = fetchCreatedTestDirectory({
    accessToken,
    baseUrl,
    directoryName,
    tags: { suite: 'shared-drive-load' },
    target: uploadTarget,
    timeout: '30s',
  })

  const testDurationMs =
    rampPlan === null
      ? parseDurationToMs(rawDuration)
      : getRampTotalDurationMs(rampPlan)
  // Ramp sessions end with the phase they started in, so by default they
  // may last a whole transition plus hold.
  const wsSessionDurationMs =
    rampPlan !== null && !__ENV.WS_SESSION_DURATION
      ? rampPlan.transitionMs + rampPlan.holdMs
      : getWsSessionDurationMs(rawDuration, __ENV.WS_SESSION_DURATION)

  const member = (role: string): Credentials => {
    const token = __ENV[`${role}_ACCESS_TOKEN`]
    const url = __ENV[`${role}_BASE_URL`]
    if (Boolean(token) !== Boolean(url)) {
      throw new Error(`${role}_ACCESS_TOKEN and ${role}_BASE_URL go together`)
    }
    return token && url ? { accessToken: token, baseUrl: url } : { accessToken, baseUrl }
  }

  return {
    accessToken,
    baseUrl,
    writer: member('WRITER'),
    reader: member('READER'),
    driveId: uploadTarget.driveId,
    rampPlan,
    rootDirectoryId: uploadTarget.rootDirectoryId,
    testDir,
    testDurationMs,
    wsSessionDurationMs,
  }
}

export function setup(): SetupData {
  return createSetupData(null)
}

// In ramp mode every sample carries the level being held, so the summary can
// judge each level on its own. Samples taken during a transition are tagged
// apart and never judged.
function tagRampLevel(
  data: SetupData,
  elapsedMs: number,
  resolvePhase: (plan: RampPlan, elapsedMs: number) => RampPhase = getRampPhaseAt,
): RampPhase | null {
  if (data.rampPlan === null) {
    return null
  }

  // Only a newly scheduled iteration may confirm a hold. Never infer this in
  // teardown/handleSummary: setup, teardown and an interrupted request can all
  // outlast a partial hold. Changes readers already drain past the final hold.
  const completed = getCompletedRampLevels(data.rampPlan, elapsedMs)
  for (const level of completed.slice(completedRampLevelCount)) {
    rampHoldCompleted.add(1, { level: String(level) })
  }
  completedRampLevelCount = completed.length

  const phase = resolvePhase(data.rampPlan, elapsedMs)
  exec.vu.metrics.tags.level =
    phase.level === null ? 'transition' : String(phase.level)
  return phase
}

function getElapsedMs(): number {
  return Date.now() - exec.scenario.startTime
}

// --- Scenario 1: Writers ---
export function writerLoop(data: SetupData): void {
  const phase = tagRampLevel(data, getElapsedMs())

  const vu = exec.vu.idInTest
  const iter = exec.vu.iterationInInstance
  const marker = [iter, vu, Date.now(), Math.floor(Math.random() * 0xffffffff)].join('-')
  const prefix = phase === null ? 'sd-' : getRampUploadPrefix(phase)
  const filename = `${prefix}vu${vu}-iter${iter}-${marker}.bin`
  const uploadBody = randomizeUploadData(marker)
  const expectedChecksum = md5(uploadBody, 'base64')

  const target: SharedDriveUploadTarget = {
    kind: 'shared-drive',
    driveId: data.driveId,
    rootDirectoryId: data.testDir.id,
  }

  const res = fetchUploadResponse({
    accessToken: data.writer.accessToken,
    baseUrl: data.writer.baseUrl,
    body: uploadBody,
    contentMD5: expectedChecksum,
    directoryId: data.testDir.id,
    filename,
    tags: { scenario: 'writers' },
    target,
    timeout: '30s',
  })

  let success = check(res, {
    'writer uploaded file successfully': (r) => r.status === 201,
  })
  // The Stack's record of the upload must match what was sent; an unreadable
  // reply is a failed upload, a different checksum or size is wrong content.
  if (success) {
    try {
      const file = parseUploadedFile(String(res.body))
      success = file.md5sum === expectedChecksum && file.size === uploadBody.byteLength && file.name === filename
      if (!success) contentMismatch.add(1)
    } catch {
      success = false
    }
  }
  uploadSuccessRate.add(success ? 1 : 0)

  sleep(1)
}

// --- Scenario 2: WebSocket Readers ---
export function wsReaderLoop(data: SetupData): void {
  const elapsedMs = getElapsedMs()
  // A ramp session serves one level: started during a transition, it stays
  // connected until the upcoming hold ends, so no event of the hold is missed.
  const phase = tagRampLevel(data, elapsedMs, getServedRampPhase)
  const sessionEndsAtMs =
    phase === null
      ? data.testDurationMs
      : phase.endsAtMs + (phase.level === null ? 0 : RAMP_EVENT_GRACE_MS)
  const timeRemainingMs = sessionEndsAtMs - elapsedMs
  if (timeRemainingMs < 1000) {
    sleep(1)
    return
  }

  const sessionDurationMs = Math.min(data.wsSessionDurationMs, timeRemainingMs)
  const wsUrl = getSharedDriveWsUrl(data.reader.baseUrl, data.driveId)
  const params = {
    tags: { scenario: 'ws_readers' },
  }

  let hadAuthError = false
  const observations = new LevelObservations()
  let openedAtMs: number | null = null

  const res = ws.connect(wsUrl, params, (socket) => {
    socket.on('open', () => {
      openedAtMs = getElapsedMs()
      socket.send(formatAuthMessage(data.reader.accessToken))
    })

    socket.on('message', (raw: string) => {
      wsMessagesReceived.add(1)
      const parsed = parseRealtimeMessage(raw)
      if (parsed) {
        if (parsed.event === 'error') {
          hadAuthError = true
          wsErrorsReceived.add(1)
          wsAuthSuccessRate.add(0)
        } else if (parsed.event === 'CREATED' || parsed.event === 'UPDATED') {
          const payload = parsed.payload as RealtimeEventPayload | undefined
          observations.record(payload?.doc?.name)
          // Ramp events count toward the level of the upload that caused them.
          wsEventsReceived.add(
            1,
            data.rampPlan === null
              ? { event: parsed.event }
              : { event: parsed.event, level: parseRampUploadLevel(payload?.doc?.name) },
          )
          check(parsed, {
            'ws received valid file event': (p) =>
              Boolean(p.payload && p.payload.type === 'io.cozy.files' && p.payload.id),
          })
        }
      }
    })

    socket.on('close', () => {
      if (!hadAuthError) {
        wsAuthSuccessRate.add(1)
      }
    })

    socket.setTimeout(() => {
      socket.close()
    }, sessionDurationMs)
  })

  const handshakeOk = check(res, {
    'ws handshake switching protocols': (r) => Boolean(r && r.status === 101),
  })
  wsHandshakeSuccessRate.add(handshakeOk ? 1 : 0)

  check(hadAuthError, {
    'ws session completed without auth error': (err) => !err,
  })
  // A session that was connected through the whole hold reports what it saw.
  const level = phase?.level ?? null
  if (data.rampPlan !== null && level !== null && handshakeOk && !hadAuthError &&
      observedWholeHold(data.rampPlan, level, openedAtMs)) {
    wsReaderUnique.add(observations.unique(level), { level: String(level) })
  }

  sleep(1)
}

// --- Scenario 3: Changes Feed Readers ---
let vuSince = 'now'
const changesObservations = new LevelObservations()
// When this reader's feed first answered: changes since then are its to see.
let changesReadyAtMs: number | null = null
const reportedLevels = new Set<number>()
const contentChecks = new Map<string, number>()

// Downloads a bounded sample of new files as the reader and compares them
// with the bytes their writer derived from the name.
function verifyContent(data: SetupData, id: string, name: unknown): void {
  const level = parseRampUploadLevel(name)
  const marker = parseUploadMarker(name)
  const done = contentChecks.get(level) ?? 0
  if (marker === null || done >= contentChecksPerLevel) {
    return
  }
  contentChecks.set(level, done + 1)
  const target: SharedDriveUploadTarget = {
    kind: 'shared-drive', driveId: data.driveId, rootDirectoryId: data.testDir.id,
  }
  const res = http.get(makeUploadTargetDownloadUrl(data.reader.baseUrl, id, target), {
    headers: { Authorization: `Bearer ${data.reader.accessToken}` },
    responseType: 'binary',
    tags: { name: 'GET /sharings/drives/:id/download/:file-id', scenario: 'changes_readers' },
  })
  const tags = { level }
  if (res.status === 403) {
    contentDenied.add(1, tags)
  } else if (res.status === 200 && res.body instanceof ArrayBuffer) {
    const matches = md5(res.body, 'base64') === md5(randomizeUploadData(marker), 'base64')
    ;(matches ? contentVerified : contentMismatch).add(1, tags)
  }
}

// Once a level's hold and drain are over, a reader ready before its hold
// reports the unique uploads it saw.
function reportCompletedLevels(data: SetupData): void {
  if (data.rampPlan === null) {
    return
  }
  for (const level of getCompletedRampLevels(data.rampPlan, getElapsedMs() - RAMP_EVENT_GRACE_MS)) {
    if (reportedLevels.has(level)) continue
    reportedLevels.add(level)
    if (observedWholeHold(data.rampPlan, level, changesReadyAtMs)) {
      changesReaderUnique.add(changesObservations.unique(level), { level: String(level) })
    }
  }
}

// Ramp changes count toward the level of the upload that caused them, even
// when they are polled after that level's hold.
function addChangesItems(data: SetupData, results: ChangesFeedItem[]): void {
  if (data.rampPlan === null) {
    if (results.length > 0) {
      changesItemsReceived.add(results.length)
    }
    return
  }

  const countsByLevel: Record<string, number> = {}
  for (const item of results) {
    const level = parseRampUploadLevel(item.doc?.name)
    countsByLevel[level] = (countsByLevel[level] ?? 0) + 1
  }
  for (const [level, count] of Object.entries(countsByLevel)) {
    changesItemsReceived.add(count, { level })
  }
}

export function changesReaderLoop(data: SetupData): void {
  tagRampLevel(data, getElapsedMs())

  const headers = {
    Authorization: `Bearer ${data.reader.accessToken}`,
    Accept: 'application/json',
  }

  const url = buildChangesFeedUrl({
    baseUrl: data.reader.baseUrl,
    sharingId: data.driveId,
    since: vuSince,
    limit: 100,
    includeDocs: false,
  })

  const startTime = Date.now()
  // A fixed name keeps the since token out of the metric labels.
  const res = http.get(url, {
    headers,
    tags: { name: 'GET /sharings/drives/:id/_changes', scenario: 'changes_readers' },
  })
  const duration = Date.now() - startTime
  changesFeedDuration.add(duration)

  const ok = check(res, {
    'changes feed returned 200': (r) => r.status === 200,
  })
  // A reply that cannot be read is a failed poll, never an empty one.
  let parsed = null
  if (ok) {
    try {
      parsed = parseChangesFeedResponse(String(res.body))
    } catch {
      changesMalformed.add(1)
    }
  }
  changesFeedSuccessRate.add(parsed === null ? 0 : 1)

  if (parsed !== null) {
    changesReadyAtMs ??= getElapsedMs()
    vuSince = parsed.last_seq
    addChangesItems(data, parsed.results)
    for (const item of parsed.results) {
      if (!item.deleted && changesObservations.record(item.doc?.name)) {
        verifyContent(data, item.id, item.doc?.name)
      }
    }
    changesPending.add(parsed.pending)
    reportCompletedLevels(data)
    if (parsed.pending > 0) {
      return
    }
  }

  reportCompletedLevels(data)
  sleep(1)
}

// --- Teardown ---
export function teardown(data: SetupData): void {
  if (!data?.testDir?.id) {
    cleanupSuccess.add(false, { scenario: 'teardown' })
    return
  }

  const target: SharedDriveUploadTarget = {
    kind: 'shared-drive',
    driveId: data.driveId,
    rootDirectoryId: data.rootDirectoryId,
  }

  const requestOptions = {
    accessToken: data.accessToken,
    baseUrl: data.baseUrl,
    directoryId: data.testDir.id,
    tags: { scenario: 'teardown' },
    target,
    timeout: '30s',
  }

  let ok = true
  const trashRes = fetchTrashResponse(requestOptions)
  if (trashRes.status !== 200) {
    ok = false
  }

  const destroyRes = fetchDestroyResponse(requestOptions)
  if (destroyRes.status !== 204) {
    ok = false
  }

  cleanupSuccess.add(ok, { scenario: 'teardown' })
}
