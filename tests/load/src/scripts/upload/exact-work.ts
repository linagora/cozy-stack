// Runs one exact amount of upload work and owns what must not differ between targets: the k6
// deadline and interruption, content verification, cleanup of the run's own data and the point's
// classification. Profiles (targets.ts) only prepare a target; upload-campaign.ts picks the work.
import { spawn, spawnSync, type SpawnSyncReturns } from 'node:child_process'
import { readFileSync, rmSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import type { UploadDataSize } from '../../fixtures/upload-sizes.ts'
import type { PointResult } from '../capacity/search.ts'
import { classifyUploadPoint, readUploadPointMetrics, type UploadPointMetrics } from '../capacity/upload-point.ts'
import { FixtureError, request } from '../fixture-http.ts'

export const harness = fileURLToPath(new URL('../../../', import.meta.url))
const REQUEST_MS = 60_000
// Each cleanup, listing included, gets this long.
const CLEANUP_MS = 300_000
// Every test directory the upload scenario creates starts with this, then its size and execution ID.
const OWNED = 'twake-load-'

// deadline bounds cleanup requests; past it, a request throws instead of being sent.
export type Stack = (method: string, path: string, ok: number[], deadline?: number) => Promise<any>
// stack is null when the target keeps nothing this runner can list.
export interface Session { token: string | undefined, stack: Stack | null }
export interface Target {
  kind: 'local' | 'dev'
  baseUrl: string
  makeArgs: string[]
  // k6 targets this runner's lock covers: only a locked runner may stop another run's k6.
  lockedUrls: string[]
  // Longest k6 run the target allows, such as its token lifetime.
  maxPointMs: number
  session(): Promise<Session>
}
export interface Cleanup { cleaned: string[], remaining: Array<{ id: string, name: string }> }
export interface Work {
  campaignId: string
  executionId: string
  phase: string
  fileSize: UploadDataSize
  vus: number
  iterationsPerVu: number
  minimumSuccessRate: number
  p95LimitMs: number | null
  consistencyChunkSizeBytes: number
  // No k6 starts or keeps running past it.
  deadline: number
}
export interface Point {
  result: PointResult
  corrupted: boolean
  // What left the point unverified besides a timeout: an interruption, Docker or cleanup.
  problems: string[]
  reason: string
  metrics: UploadPointMetrics | null
  k6: { exit_code: number, timed_out: boolean }
  cleanup: Cleanup | null
  summary: string
}

export function check(value: unknown, message: string): asserts value {
  if (!value) throw new FixtureError(message)
}
export function parse(text: string): any {
  try { return text ? JSON.parse(text) : {} } catch { throw new FixtureError('Invalid JSON reply') }
}
export const message = (error: unknown) => error instanceof Error ? error.message : String(error)

export async function call(url: URL, method: string, ok: number[], headers: Record<string, string>, body = '',
  timeout = REQUEST_MS): Promise<any> {
  const reply = await request(url, method, { Accept: 'application/json', ...headers }, timeout, body, 8 << 20)
  check(ok.includes(reply.status), `${method} ${url.pathname}: HTTP ${reply.status} ${reply.body.slice(0, 200)}`)
  return parse(reply.body)
}

// origin is where to connect; host stays the instance the Stack routes to.
export function stackClient(origin: string, host: string, token: string): Stack {
  return (method, path, ok, deadline = Infinity) => {
    const left = Math.min(REQUEST_MS, deadline - Date.now())
    check(left > 0, `cleanup exceeded its ${CLEANUP_MS / 60_000} minute budget`)
    return call(new URL(path, origin), method, ok, { Host: host, Authorization: `Bearer ${token}` }, '', left)
  }
}

let interrupted: NodeJS.Signals | null = null
let stopK6 = (): void => {}
export const interruption = () => interrupted
// An interruption stops the running k6; cleanup and the report still follow.
export function trapInterrupts(): void {
  for (const signal of ['SIGINT', 'SIGTERM'] as const) {
    process.on(signal, () => {
      interrupted ??= signal
      stopK6()
    })
  }
}

// docker compose run neither forwards a stop to k6 nor waits for it, so k6 containers are stopped
// directly: k6 then finishes its teardown within 90 seconds. Docker keeps each container's
// environment, so a run's identity outlives a killed coordinator. An unreadable inventory throws:
// a k6 left unseen may still be writing.
function k6Containers(...env: string[]): string[] {
  const docker = (...args: string[]) => spawnSync('docker', args, { encoding: 'utf8' })
  const failed = (what: string, result: SpawnSyncReturns<string>) =>
    `docker ${what} failed: ${(result.stderr || result.error?.message || `exit ${result.status}`).trim().slice(0, 200)}`
  const list = () => {
    const result = docker('ps', '-q', '--filter', 'label=com.docker.compose.service=k6')
    check(result.status === 0, failed('ps', result))
    return result.stdout.split('\n').filter(Boolean)
  }
  return list().filter(id => {
    const result = docker('inspect', '--format', '{{json .Config.Env}}', id)
    if (result.status === 0) return env.some(entry => result.stdout.includes(JSON.stringify(entry)))
    // A container that exited since the listing no longer runs k6.
    check(!list().includes(id), failed(`inspect ${id}`, result))
    return false
  })
}
const stopContainers = (ids: string[]) => spawnSync('docker', ['stop', '--time', '90', ...ids], { stdio: 'ignore' })

// make runs in its own process group: a stop before k6 starts ends make and docker compose.
// unverified says why the run's k6 containers could not be shown stopped.
interface K6Run { code: number, timedOut: boolean, unverified: string | null }

// make runs in its own process group: a stop before k6 starts ends make and docker compose.
function runK6(args: string[], env: NodeJS.ProcessEnv, executionId: string, limitMs: number): Promise<K6Run> {
  return new Promise(resolve => {
    const child = spawn('make', args, { cwd: harness, env, detached: true, stdio: ['ignore', 'pipe', 'pipe'] })
    child.stdout.pipe(process.stdout)
    child.stderr.pipe(process.stderr)
    const signal = (name: NodeJS.Signals) => { try { process.kill(-child.pid!, name) } catch {} }
    let timedOut = false
    let unverified: string | null = null
    const inventory = (): string[] => {
      try { return k6Containers(`EXECUTION_ID=${executionId}`) } catch (error) { unverified = message(error); return [] }
    }
    stopK6 = () => {
      stopK6 = () => {}
      const ids = inventory()
      if (ids.length) spawn('docker', ['stop', '--time', '90', ...ids], { stdio: 'ignore' })
      else signal('SIGTERM')
      setTimeout(() => signal('SIGKILL'), 120_000).unref()
    }
    const deadline = Number.isFinite(limitMs)
      ? setTimeout(() => { timedOut = true; stopK6() }, Math.min(limitMs, 2 ** 31 - 1))
      : undefined
    child.on('error', () => resolve({ code: 2, timedOut, unverified }))
    child.on('close', code => {
      clearTimeout(deadline)
      stopK6 = () => {}
      // A container docker compose left behind still runs k6.
      const left = inventory()
      if (left.length && stopContainers(left).status !== 0) unverified = `docker stop ${left.join(' ')} failed`
      resolve({ code: code ?? 2, timedOut, unverified })
    })
  })
}

// Destroys every directory starting with prefix, in the root or the trash, then lists what remains.
// Every request shares the cleanup budget: running out throws, which leaves the run inconclusive.
async function reconcile(stack: Stack, prefix: string): Promise<Cleanup> {
  const deadline = Date.now() + CLEANUP_MS
  const owned = async () => {
    const found: Array<{ id: string, name: string, trashed: boolean }> = []
    for (const [path, trashed] of [['/files/io.cozy.files.root-dir', false], ['/files/trash', true]] as const) {
      for (let next: string | undefined = `${path}?page[limit]=100`; next;) {
        const page = await stack('GET', next, [200], deadline)
        // A directory lists its children in included, the trash in data.
        for (const item of (Array.isArray(page.data) ? page.data : page.included) ?? []) {
          if (item.attributes?.name?.startsWith(prefix)) found.push({ id: item.id, name: item.attributes.name, trashed })
        }
        next = page.links?.next
      }
    }
    return found
  }
  const cleaned: string[] = []
  for (const item of await owned()) {
    if (Date.now() > deadline) break
    try {
      if (!item.trashed) await stack('DELETE', `/files/${item.id}`, [200], deadline)
      await stack('DELETE', `/files/trash/${item.id}`, [204], deadline)
      cleaned.push(item.name)
    } catch (error) {
      console.error(message(error))
    }
  }
  return { cleaned, remaining: (await owned()).map(({ id, name }) => ({ id, name })) }
}

// A killed coordinator frees the lock but not its k6 container, and may have died before its
// cleanup. Under the lock, stop every k6 still loading a covered target, then delete all test data.
export async function recover(target: Target): Promise<{ k6_stopped: string[], cleaned: string[] }> {
  const urls = target.lockedUrls.map(url => `BASE_URL=${url}`)
  const stopped = k6Containers(...urls)
  if (stopped.length) {
    console.error(`Stopping k6 left running by an earlier run: ${stopped.join(' ')}`)
    stopContainers(stopped)
    check(!k6Containers(...urls).length, 'k6 from an earlier run is still running')
  }
  const { stack } = await target.session()
  const before = stack ? await reconcile(stack, OWNED) : { cleaned: [], remaining: [] }
  check(!before.remaining.length, `Earlier test data remains on ${target.baseUrl}: ${JSON.stringify(before.remaining)}`)
  return { k6_stopped: stopped, cleaned: before.cleaned }
}

export async function runExactWork(target: Target, work: Work): Promise<Point> {
  const summary = `data/results/${work.executionId}-summary.json`
  rmSync(`${harness}${summary}`, { force: true })
  const problems: string[] = []
  let k6: K6Run = { code: 2, timedOut: false, unverified: null }
  let limitMs = 0
  let started = false
  try {
    if (!interrupted) {
      const { token } = await target.session()
      // Measured after the token request, whose time also counts against the deadline.
      limitMs = Math.min(work.deadline - Date.now(), target.maxPointMs)
      // Checked again: a signal during the token request found no k6 to stop. -e: the target's
      // token must win over the local one cozy-prepare writes to the harness .env.
      if (limitMs <= 0) {
        k6.timedOut = true
      } else if (!interrupted) {
        started = true
        k6 = await runK6(['-e', '-o', 'node_modules/.load-ready', 'upload', `BASE_URL=${target.baseUrl}`,
          ...target.makeArgs, 'UPLOAD_MODE=iterations', `VUS=${work.vus}`, `ITERATIONS_PER_VU=${work.iterationsPerVu}`,
          `FILE_SIZE=${work.fileSize}`, `CONSISTENCY_CHUNK_SIZE_BYTES=${work.consistencyChunkSizeBytes}`,
          `MIN_SUCCESS_RATE=${work.minimumSuccessRate}`, `P95_LIMIT_MS=${work.p95LimitMs ?? ''}`,
          `CAPACITY_RUN_ID=${work.campaignId}`, `CAPACITY_PHASE=${work.phase}`, `EXECUTION_ID=${work.executionId}`,
        ], token ? { ...process.env, COZY_ACCESS_TOKEN: token } : process.env, work.executionId, limitMs)
      }
    }
  } catch (error) {
    problems.push(`k6 could not start: ${message(error)}`)
  }

  let parsed: unknown = null
  try { parsed = JSON.parse(readFileSync(`${harness}${summary}`, 'utf8')) } catch {}
  const metrics = readUploadPointMetrics(parsed)
  const point = classifyUploadPoint({
    expectedIterations: work.vus * work.iterationsPerVu, metrics, minimumSuccessRate: work.minimumSuccessRate,
    p95LimitMs: work.p95LimitMs, runnerExitCode: k6.code, summaryFound: parsed !== null,
  })
  const timeout = !k6.timedOut ? ''
    : limitMs > 0 ? `k6 ran past its ${Math.round(limitMs / 1000)} s limit` : 'no time was left to start k6'
  if (interrupted) problems.push(`interrupted by ${interrupted}`)
  if (k6.unverified) problems.push(`k6 containers could not be verified: ${k6.unverified}`)

  let cleanup: Cleanup | null = null
  try {
    const { stack } = await target.session()
    if (stack) {
      cleanup = await reconcile(stack, `${OWNED}${work.fileSize.toLowerCase()}-${work.executionId}-`)
      if (cleanup.remaining.length) problems.push('test data remains after cleanup')
    } else if (started && metrics?.cleanupRate !== 1) {
      // Without a listing, only k6's own teardown shows that a started run left nothing.
      problems.push('cleanup could not be verified')
    }
  } catch (error) {
    problems.push(`cleanup could not be verified: ${message(error)}`)
  }
  return {
    result: point.result,
    // A mismatch stays a correctness failure even when cleanup also failed.
    corrupted: metrics?.consistencyRate != null && metrics.consistencyRate < 1,
    problems,
    reason: [point.reason, timeout, ...problems].filter(Boolean).join('; '),
    metrics,
    k6: { exit_code: k6.code, timed_out: k6.timedOut },
    cleanup,
    summary,
  }
}
