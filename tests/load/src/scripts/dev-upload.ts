// Runs the exact-work upload scenario against one shared DEV instance from the Cloudery fixture
// manifest, within hard budgets, then verifies cleanup itself. It never provisions a local Stack.
// `make dev-upload` runs it under the runner's shared DEV lock (README, Shared DEV).
import { spawn, spawnSync } from 'node:child_process'
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { getUploadDataSizeBytes, isUploadDataSize } from '../fixtures/upload-sizes.ts'
import { buildGrafanaLink } from './campaign-summary.ts'
import { classifyUploadPoint, hasContentMismatch, readUploadPointMetrics } from './capacity/upload-point.ts'
import { FixtureError, request } from './fixture-http.ts'

// Raise these only with the DEV owner's agreement. Each phase is bounded, so one 30-minute
// instance token covers the run: cleanup before and after k6 gets 5 minutes each.
const LIMITS = {
  vus: 2, iterationsPerVu: 5, uploadBytes: 1 << 20, requestTimeout: '60s', scenarioDuration: '5m',
  k6Ms: 600_000, cleanupMs: 300_000,
} as const
const REQUEST_MS = 60_000
// Every test directory the upload scenario creates starts with this.
const OWNED = 'twake-load-'
const harness = fileURLToPath(new URL('../../', import.meta.url))

interface Owned { id: string, name: string, trashed: boolean }

function check(value: unknown, message: string): asserts value {
  if (!value) throw new FixtureError(message)
}
function setting(name: string, fallback?: string): string {
  const value = process.env[name] || fallback
  check(value, `${name} is required`)
  return value
}
function count(name: string, max: number): number {
  const value = Number(setting(name, '1'))
  check(Number.isSafeInteger(value) && value >= 1 && value <= max, `${name} must be 1 to ${max} on shared DEV`)
  return value
}
function parse(text: string): any {
  try { return text ? JSON.parse(text) : {} } catch { throw new FixtureError('Invalid JSON reply') }
}
const message = (error: unknown) => error instanceof Error ? error.message : String(error)
const output = (command: string, args: string[]) =>
  spawnSync(command, args, { cwd: harness, encoding: 'utf8' }).stdout?.trim() || 'unknown'

let interrupted: NodeJS.Signals | null = null
let stopK6 = (): void => {}
for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    interrupted ??= signal
    stopK6()
  })
}

// docker compose run neither forwards a stop to k6 nor waits for it, so k6 containers are stopped
// directly: k6 then finishes its teardown within 90 seconds. Docker keeps each container's
// environment, so a run's identity outlives a killed coordinator.
function k6Containers(...env: string[]): string[] {
  const docker = (...args: string[]) => spawnSync('docker', args, { encoding: 'utf8' }).stdout ?? ''
  return docker('ps', '-q', '--filter', 'label=com.docker.compose.service=k6').split('\n').filter(Boolean)
    .filter(id => {
      const config = docker('inspect', '--format', '{{json .Config.Env}}', id)
      return env.some(entry => config.includes(JSON.stringify(entry)))
    })
}
const stopContainers = (ids: string[]) => spawnSync('docker', ['stop', '--time', '90', ...ids], { stdio: 'ignore' })

// make runs in its own process group: a stop before k6 starts ends make and docker compose.
function runK6(args: string[], env: NodeJS.ProcessEnv, executionId: string): Promise<{ code: number, timedOut: boolean }> {
  return new Promise(resolve => {
    const child = spawn('make', args, { cwd: harness, env, detached: true, stdio: ['ignore', 'pipe', 'pipe'] })
    child.stdout.pipe(process.stdout)
    child.stderr.pipe(process.stderr)
    const signal = (name: NodeJS.Signals) => { try { process.kill(-child.pid!, name) } catch {} }
    let timedOut = false
    stopK6 = () => {
      stopK6 = () => {}
      const ids = k6Containers(`EXECUTION_ID=${executionId}`)
      if (ids.length) spawn('docker', ['stop', '--time', '90', ...ids], { stdio: 'ignore' })
      else signal('SIGTERM')
      setTimeout(() => signal('SIGKILL'), 120_000).unref()
    }
    const deadline = setTimeout(() => { timedOut = true; stopK6() }, LIMITS.k6Ms)
    child.on('error', () => resolve({ code: 2, timedOut }))
    child.on('close', code => {
      clearTimeout(deadline)
      stopK6 = () => {}
      // A container docker compose left behind still runs k6.
      const left = k6Containers(`EXECUTION_ID=${executionId}`)
      if (left.length) stopContainers(left)
      resolve({ code: code ?? 2, timedOut })
    })
  })
}

async function main(): Promise<number> {
  // Everything is validated before the first request.
  const vus = count('VUS', LIMITS.vus)
  const iterations = count('ITERATIONS_PER_VU', LIMITS.iterationsPerVu)
  const size = setting('FILE_SIZE', '1K')
  check(isUploadDataSize(size) && getUploadDataSizeBytes(size) <= LIMITS.uploadBytes,
    'FILE_SIZE must be 1K, 100K or 1M on shared DEV')
  const executionId = setting('EXECUTION_ID')
  check(/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(executionId), 'EXECUTION_ID must be a file-name-safe ID')
  const fqdn = setting('DEV_INSTANCE')
  const fixture = parse(readFileSync(setting('SEED_CLOUDERY_MANIFEST'), 'utf8'))
  const entry = fixture.instances?.find((i: any) => i.fqdn === fqdn)
  check(entry?.uuid, `${fqdn} is not in the fixture manifest`)
  const secrets = parse(readFileSync(setting('SEED_CLOUDERY_TOKEN_FILE'), 'utf8'))
  check(secrets.public && secrets.manager_url, 'The token file needs public and manager_url')

  const call = async (url: URL, method: string, ok: number[], headers: Record<string, string>, body = '',
    timeout = REQUEST_MS) => {
    const reply = await request(url, method, { Accept: 'application/json', ...headers }, timeout, body, 8 << 20)
    check(ok.includes(reply.status), `${method} ${url.pathname}: HTTP ${reply.status} ${reply.body.slice(0, 200)}`)
    return parse(reply.body)
  }
  const { token } = await call(new URL(`/api/public/instances/${fqdn}/token`, secrets.manager_url), 'POST', [200],
    { Authorization: `Bearer ${secrets.public}`, 'Content-Type': 'application/json' },
    JSON.stringify({ audience: 'cli', scope: 'io.cozy.files io.cozy.settings' }))
  check(typeof token === 'string' && token, `Invalid token for ${fqdn}`)
  // DEV_STACK_URL is a connection origin for tests; the instance stays the Host.
  const stack = (method: string, path: string, ok: number[], deadline = Infinity) => {
    const left = Math.min(REQUEST_MS, deadline - Date.now())
    check(left > 0, `cleanup exceeded its ${LIMITS.cleanupMs / 60_000} minute budget`)
    return call(new URL(path, process.env.DEV_STACK_URL || `https://${fqdn}`), method, ok,
      { Host: fqdn, Authorization: `Bearer ${token}` }, '', left)
  }

  const settings = (await stack('GET', '/settings/instance', [200])).data?.attributes
  check(settings?.uuid === entry.uuid, `${fqdn} is no longer the fixture instance in the manifest`)

  const owned = async (deadline: number): Promise<Owned[]> => {
    const found: Owned[] = []
    for (const [path, trashed] of [['/files/io.cozy.files.root-dir', false], ['/files/trash', true]] as const) {
      for (let next: string | undefined = `${path}?page[limit]=100`; next;) {
        const page = await stack('GET', next, [200], deadline)
        // A directory lists its children in included, the trash in data.
        for (const item of (Array.isArray(page.data) ? page.data : page.included) ?? []) {
          if (item.attributes?.name?.startsWith(OWNED)) found.push({ id: item.id, name: item.attributes.name, trashed })
        }
        next = page.links?.next
      }
    }
    return found
  }
  // Destroys every owned directory, in the root or the trash, then lists what remains. Every
  // request shares the cleanup budget: running out throws, which leaves the run inconclusive.
  const reconcile = async () => {
    const deadline = Date.now() + LIMITS.cleanupMs
    const cleaned: string[] = []
    for (const item of await owned(deadline)) {
      if (Date.now() > deadline) break
      try {
        if (!item.trashed) await stack('DELETE', `/files/${item.id}`, [200], deadline)
        await stack('DELETE', `/files/trash/${item.id}`, [204], deadline)
        cleaned.push(item.name)
      } catch (error) {
        console.error(message(error))
      }
    }
    return { cleaned, remaining: (await owned(deadline)).map(({ id, name }) => ({ id, name })) }
  }

  // A killed coordinator frees the lock but not its k6 container: stop any k6 still loading DEV
  // before touching its data. An earlier run may also have been killed before its cleanup.
  const devTargets = (fixture.instances as Array<{ fqdn: string }>).map(i => `BASE_URL=https://${i.fqdn}`)
  const stopped = k6Containers(...devTargets)
  if (stopped.length) {
    console.error(`Stopping k6 left running by an earlier DEV run: ${stopped.join(' ')}`)
    stopContainers(stopped)
    check(!k6Containers(...devTargets).length, 'k6 from an earlier DEV run is still running')
  }
  const before = await reconcile()
  check(before.remaining.length === 0, `Earlier test data remains on ${fqdn}: ${JSON.stringify(before.remaining)}`)
  const summaryPath = `data/results/${executionId}-summary.json`
  mkdirSync(`${harness}data/results`, { recursive: true })
  rmSync(`${harness}${summaryPath}`, { force: true })
  const startedAt = new Date().toISOString()
  // -e: the instance token must win over the local one cozy-prepare writes to the harness .env.
  const k6 = interrupted ? { code: 2, timedOut: false } : await runK6([
    '-e', '-o', 'node_modules/.load-ready', 'upload', `BASE_URL=https://${fqdn}`, 'UPLOAD_TARGET=personal',
    'UPLOAD_MODE=iterations', `VUS=${vus}`, `ITERATIONS_PER_VU=${iterations}`, `FILE_SIZE=${size}`,
    `MAX_DURATION=${LIMITS.scenarioDuration}`, `UPLOAD_TIMEOUT=${LIMITS.requestTimeout}`, 'MIN_SUCCESS_RATE=1',
    'P95_LIMIT_MS=', 'CAPACITY_RUN_ID=', 'CAPACITY_PHASE=dev', `EXECUTION_ID=${executionId}`,
  ], { ...process.env, COZY_ACCESS_TOKEN: token }, executionId)
  const endedAt = new Date().toISOString()

  let summary: unknown = null
  try { summary = JSON.parse(readFileSync(`${harness}${summaryPath}`, 'utf8')) } catch {}
  const metrics = readUploadPointMetrics(summary)
  const point = classifyUploadPoint({
    expectedIterations: vus * iterations, metrics, minimumSuccessRate: 1, p95LimitMs: null,
    runnerExitCode: k6.code, summaryFound: summary !== null,
  })
  // A proven correctness failure keeps exit 1, even when cleanup also failed; anything else
  // unfinished makes the run inconclusive.
  const corrupted = hasContentMismatch([{ consistency_rate: metrics?.consistencyRate }])
  let outcome = corrupted || point.result === 'failed' ? 1 : point.result === 'passed' ? 0 : 2
  const reasons = [point.reason]
  const inconclusive = (reason: string) => {
    reasons.push(reason)
    if (outcome === 0) outcome = 2
  }
  if (k6.timedOut) inconclusive(`k6 exceeded its ${LIMITS.k6Ms / 60_000} minute budget`)
  if (interrupted) inconclusive(`interrupted by ${interrupted}`)
  let after: Awaited<ReturnType<typeof reconcile>> | null = null
  try {
    after = await reconcile()
    if (after.remaining.length) inconclusive('test data remains after cleanup')
  } catch (error) {
    inconclusive(`cleanup could not be verified: ${message(error)}`)
  }

  const image = readFileSync(`${harness}compose.yaml`, 'utf8').match(/image: (grafana\/k6:\S+)/)?.[1]
  const report = `data/results/${executionId}-dev-upload.json`
  writeFileSync(`${harness}${report}`, `${JSON.stringify({
    outcome, reason: reasons.filter(Boolean).join('; '), instance: fqdn, started_at: startedAt, ended_at: endedAt,
    parameters: { vus, iterations_per_vu: iterations, file_size: size }, limits: LIMITS,
    versions: {
      stack: (await stack('GET', '/version', [200]).catch(() => ({}))).version ?? 'unknown',
      harness: output('git', ['describe', '--always', '--dirty']),
      node: process.version,
      k6_image: image ? output('docker', ['image', 'inspect', '--format', '{{index .RepoDigests 0}}', image]) : 'unknown',
    },
    monitoring: {
      k6: buildGrafanaLink(process.env.GRAFANA_URL || 'http://localhost:3000', {
        name: 'dev-upload', kind: 'upload-capacity', id: executionId, startedAt, endedAt,
        exitCode: k6.code, producerExitCode: k6.code,
      }),
      dev: process.env.DEV_MONITORING_URL || 'unknown',
    },
    k6: { exit_code: k6.code, timed_out: k6.timedOut, summary: summaryPath, metrics },
    cleanup: { k6_stopped: stopped, before: before.cleaned, after: after?.cleaned ?? 'unknown', remaining: after?.remaining ?? 'unknown' },
  }, null, 2)}\n`)
  console.log(`Shared DEV upload outcome ${outcome}${reasons.some(Boolean) ? ` (${reasons.filter(Boolean).join('; ')})` : ''}: ${report}`)
  return outcome
}

main().then(code => { process.exitCode = code }, (error: unknown) => {
  // Setup problems never count as correctness failures.
  console.error(message(error))
  process.exitCode = 2
})
