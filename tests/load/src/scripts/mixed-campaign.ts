// Runs the mixed arrival-rate workload (README, Mixed workload) against a verified tree fixture
// (SEEDING.md, Profiles) on a local Stack, through the exact-work runner (upload/exact-work.ts).
// Budgets are checked before any request, the fixture is verified before and after the run, and the
// run's own uploads are removed. k6 judges the rates; this runner adds what it verified.
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { WARMUP_GRACE_MS } from '../lib/mixed-workload.ts'
import { TREE_VERSION, treeFiles } from './dataset-profile.ts'
import { stackAddress } from './drive-actors.ts'
import { request } from './fixture-http.ts'
import { manifestPath, readManifest } from './fixture-manifest.ts'
import { mixedEnvelope, topologyOf } from './result-envelope.ts'
import { getBytes, getDurationMs, getPositiveInteger } from './upload/campaign-config.ts'
import {
  check, harness, interruption, message, readVersions, runK6Work, stackClient, trapInterrupts, type Target,
} from './upload/exact-work.ts'

type Env = Record<string, string | undefined>
const ROOT = 'io.cozy.files.root-dir'
// Each workflow lists, reads and uploads once; each observer opens one session.
const REQUESTS_PER_WORKFLOW = 3
const UPLOAD_BYTES = 1024
// k6 setup and teardown, the runner's verification and cleanup, on top of the arrivals.
const OVERHEAD_MS = 180_000
// A soak longer than this needs a passed shorter trial at its rate first.
const TRIAL_MAX_MS = 10 * 60_000

// Every setting and budget is checked here, before the first request.
export function readConfig(env: Env) {
  const executionId = env.EXECUTION_ID || ''
  check(/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(executionId), 'EXECUTION_ID must be a file-name-safe ID')
  // shortcut: the Cloudery seeder has no tree profile yet, so shared DEV has no fixture to run against.
  check((env.LOAD_TARGET || 'local') === 'local', 'The mixed workload runs on a local tree fixture only')
  const domain = env.MIXED_DOMAIN
  check(domain, 'MIXED_DOMAIN must name a tree fixture (make seed-scale SEED_PROFILE=tree)')
  const manifest = readManifest(env.SEED_MANIFEST || manifestPath(domain), domain)
  const entry = manifest.instances[0]!
  // One instance: verifying and cleaning it then costs a known number of requests.
  check(manifest.status === 'complete' && manifest.profile === 'tree' && manifest.profile_version === TREE_VERSION &&
    !manifest.organization && entry.files.length,
  `${domain} is not a complete single-instance tree fixture, version ${TREE_VERSION} ` +
    '(make seed-scale SEED_PROFILE=tree)')
  const profile = env.MIXED_PROFILE || 'fixed'
  check(profile === 'fixed' || profile === 'soak', 'MIXED_PROFILE must be fixed or soak')
  const soak = profile === 'soak'
  const rate = getPositiveInteger('MIXED_RATE', env.MIXED_RATE || '1')
  const durationMs = getDurationMs('MIXED_DURATION', env.MIXED_DURATION || (soak ? '10m' : '2m'))
  const warmupMs = getDurationMs('MIXED_WARMUP', env.MIXED_WARMUP || (soak ? '1m' : '0s'))
  const drainMs = getDurationMs('MIXED_DRAIN', env.MIXED_DRAIN || (soak ? '1m' : '0s'))
  const maxVus = getPositiveInteger('MIXED_MAX_VUS', env.MIXED_MAX_VUS || '10')
  // Make passes unset settings as empty strings.
  const observers = Number(env.MIXED_OBSERVERS || '1')
  check(Number.isSafeInteger(observers) && observers >= 0, 'MIXED_OBSERVERS must be a non-negative integer')
  const graceMs = warmupMs ? WARMUP_GRACE_MS : 0
  // The caps cover the warm-up, the observers, both fixture verifications and cleanup too. k6 starts one
  // more arrival at the very start of each arrival scenario than rate × seconds.
  const workflows = rate * Math.ceil((warmupMs + durationMs) / 1000) + (warmupMs ? 2 : 1)
  // A verification reads the instance, mints a token, then reads every directory, file and drive root and
  // the root's entry count.
  const verifyRequests = 3 + (entry.dirs?.length ?? 0) + entry.files.length + entry.drives.length
  // Cleanup lists the root (100 entries a page) and the trash before and after deleting the run's
  // directory, in two requests. shortcut: the trash is taken to fit one page, as on a fixture only these
  // runs write to; count its pages too once fixtures are shared.
  const cleanupRequests = 2 * (Math.ceil(((entry.root_entries ?? 0) + 1) / 100) + 1) + 2
  // k6 setup creates the run's directory; the runner mints a token, reads the Stack version and, in a
  // soak, samples the Stack's metrics before and after.
  const needs = {
    vus: maxVus + observers, bytes: workflows * UPLOAD_BYTES,
    runtimeMs: warmupMs + graceMs + durationMs + drainMs + OVERHEAD_MS,
    requests: workflows * REQUESTS_PER_WORKFLOW + observers + 1 + 2 * verifyRequests + cleanupRequests + 2 +
      (soak ? 2 : 0),
  }
  const budgets = {
    max_vus: env.MAX_VUS ? getPositiveInteger('MAX_VUS', env.MAX_VUS) : null,
    max_requests: env.MAX_REQUESTS ? getPositiveInteger('MAX_REQUESTS', env.MAX_REQUESTS) : null,
    max_upload_bytes: env.MAX_UPLOAD_BYTES ? getBytes('MAX_UPLOAD_BYTES', env.MAX_UPLOAD_BYTES) : null,
    max_runtime: env.MAX_RUNTIME || null,
  }
  const runtimeMs = budgets.max_runtime ? getDurationMs('MAX_RUNTIME', budgets.max_runtime) : Infinity
  check(budgets.max_vus === null || needs.vus <= budgets.max_vus,
    `The workload runs up to ${needs.vus} VUs, beyond MAX_VUS`)
  check(budgets.max_requests === null || needs.requests <= budgets.max_requests,
    `The workload sends up to ${needs.requests} requests, beyond MAX_REQUESTS`)
  check(budgets.max_upload_bytes === null || needs.bytes <= budgets.max_upload_bytes,
    `The workload uploads up to ${needs.bytes} bytes, beyond MAX_UPLOAD_BYTES`)
  check(needs.runtimeMs <= runtimeMs,
    `The workload needs ${Math.ceil(needs.runtimeMs / 60_000)} minutes, beyond MAX_RUNTIME`)
  if (soak && durationMs > TRIAL_MAX_MS) checkTrial(env.SOAK_TRIAL, domain, rate)
  const recovery = {
    max_goroutine_growth: Number(env.SOAK_MAX_GOROUTINE_GROWTH || '1.25'),
    max_heap_growth: Number(env.SOAK_MAX_HEAP_GROWTH || '1.5'),
  }
  check(Object.values(recovery).every(v => Number.isFinite(v) && v >= 1), 'Recovery growth limits must be at least 1')
  return {
    executionId, domain, manifest, entry, profile, soak, rate, durationMs, warmupMs, graceMs, drainMs, maxVus,
    observers, needs, budgets, recovery, deadlineMs: runtimeMs, ...criteria(env),
  }
}

// A long soak starts only after a short run of the same fixture passed at its rate or higher.
function checkTrial(path: string | undefined, domain: string, rate: number): void {
  check(path, `A soak longer than ${TRIAL_MAX_MS / 60_000} minutes needs SOAK_TRIAL: a passed shorter run's report`)
  let trial: Record<string, any> | null = null
  try { trial = JSON.parse(readFileSync(path, 'utf8')) } catch {}
  check(trial?.result?.producer === 'mixed-workload' && trial.result.outcome === 0 && trial.fixture === domain &&
    trial.plan?.rate >= rate, `${path} is not a passed trial of ${domain} at ${rate} workflows/s or more`)
}

// The Stack's goroutines and memory, from its admin metrics.
export function parseResources(text: string): Record<string, number> {
  const read = (name: string) => Number(new RegExp(`^${name} (\\S+)$`, 'm').exec(text)?.[1])
  const sample = { goroutines: read('go_goroutines'), heap_inuse_bytes: read('go_memstats_heap_inuse_bytes'),
    resident_bytes: read('process_resident_memory_bytes') }
  check(Object.values(sample).every(Number.isFinite), 'The Stack metrics lack goroutines or memory')
  return sample
}

// After the drain, goroutines and heap must come back within their growth limits of the sample before
// the run; small absolute slack keeps a quiet Stack's noise from failing it.
export function judgeRecovery(before: Record<string, number>, after: Record<string, number>,
  limits: { max_goroutine_growth: number, max_heap_growth: number }): string[] {
  return [
    ...(after.goroutines! > before.goroutines! * limits.max_goroutine_growth + 20
      ? [`goroutines ${after.goroutines} after the drain exceed ${limits.max_goroutine_growth} × ${before.goroutines}`]
      : []),
    ...(after.heap_inuse_bytes! > before.heap_inuse_bytes! * limits.max_heap_growth + (16 << 20)
      ? [`heap ${after.heap_inuse_bytes} bytes after the drain exceeds ${limits.max_heap_growth} × ` +
        `${before.heap_inuse_bytes}`] : []),
  ]
}

// k6 containers still running a mixed workload, such as one a killed coordinator left behind. An
// unreadable inventory throws: a workload left unseen may still be running.
export function runningMixedWorkloads(): string[] {
  const docker = (...args: string[]) => spawnSync('docker', args, { encoding: 'utf8' })
  const listed = docker('ps', '-q', '--filter', 'label=com.docker.compose.service=k6')
  check(listed.status === 0, `docker ps failed, so a running workload cannot be ruled out: ${listed.stderr?.trim()}`)
  return listed.stdout.split('\n').filter(Boolean).filter(id => {
    const inspected = docker('inspect', '--format', '{{json .Config.Env}}', id)
    if (inspected.status === 0) return /"MIXED_RATE=/.test(inspected.stdout)
    // A container gone since the listing runs nothing; one still listed but unreadable may be a survivor.
    const still = docker('ps', '-q', '--filter', `id=${id}`)
    check(still.status === 0 && !still.stdout.trim(),
      `docker inspect ${id} failed, so it may still run a mixed workload`)
    return false
  })
}

// One mixed or soak run per local Stack: a second one exits 2 instead of overlapping. A lock whose
// process is gone is taken over.
export function holdLock(path: string): () => void {
  try {
    writeFileSync(path, String(process.pid), { flag: 'wx' })
  } catch {
    const pid = Number(existsSync(path) ? readFileSync(path, 'utf8') : '')
    const alive = (() => { try { return process.kill(pid, 0) } catch { return false } })()
    check(!alive, `Another mixed workload (pid ${pid}) holds ${path}`)
    writeFileSync(path, String(process.pid))
  }
  return () => rmSync(path, { force: true })
}

// k6 reads the criteria as numbers: an unreadable one would silently pass every comparison.
function criteria(env: Env) {
  const minSuccessRate = Number(env.MIN_SUCCESS_RATE || '0.99')
  const p95LimitMs = Number(env.P95_LIMIT_MS || '2000')
  check(minSuccessRate > 0 && minSuccessRate <= 1, 'MIN_SUCCESS_RATE must be a ratio in (0, 1]')
  check(Number.isFinite(p95LimitMs) && p95LimitMs > 0, 'P95_LIMIT_MS must be a positive number of milliseconds')
  return { minSuccessRate, p95LimitMs }
}

// The fixture as seeded, or the reason it is not.
function verify(domain: string): string {
  const seeder = fileURLToPath(new URL('./seed-scale-data.ts', import.meta.url))
  const run = spawnSync(process.execPath, ['--import', 'tsx', seeder, 'verify', domain], { encoding: 'utf8' })
  return run.status === 0 ? '' : (run.stderr || run.stdout).trim().split('\n').at(-1) || `verify exited ${run.status}`
}

// Proven wrong content outweighs everything; anything unverified leaves the rates as evidence.
export function classifyMixed(report: Record<string, any>): { code: 0 | 1 | 2, reason: string } {
  if (report.correctness_failures?.length) return { code: 1, reason: report.correctness_failures.join('; ') }
  if (report.problems?.length) return { code: 2, reason: report.problems.join('; ') }
  if (report.status === 'failed') return { code: 1, reason: report.reasons.join('; ') }
  if (report.status !== 'passed') {
    return { code: 2, reason: report.reasons?.join('; ') || 'the workload measured nothing' }
  }
  return { code: 0, reason: '' }
}

async function main(): Promise<number> {
  trapInterrupts()
  const env: Env = process.env
  const config = readConfig(env)
  mkdirSync(`${harness}data/results`, { recursive: true })
  const release = holdLock(`${harness}data/results/.mixed-workload.lock`)
  try {
    // A stale lock can hide a k6 a killed coordinator left running: that run is not over.
    const orphans = runningMixedWorkloads()
    check(!orphans.length, `A mixed workload is still running in k6 container ${orphans.join(' ')}; stop it first`)
    return await run(env, config)
  } finally {
    release()
  }
}

async function run(env: Env, config: ReturnType<typeof readConfig>): Promise<number> {
  const startedAt = new Date().toISOString()
  const deadline = Date.now() + config.deadlineMs
  const { manifest, entry } = config
  const before = verify(config.domain)
  check(!before, `The fixture did not verify before the run: ${before}`)

  const stackOrigin = env.SEED_STACK_URL
  const adminURL = env.SEED_ADMIN_URL
  check(stackOrigin && adminURL && env.SEED_ADMIN_PASSWORD,
    'SEED_ADMIN_URL, SEED_ADMIN_PASSWORD and SEED_STACK_URL are required')
  const credentials = `${env.SEED_ADMIN_USERNAME || 'admin'}:${env.SEED_ADMIN_PASSWORD}`
  const query = new URLSearchParams({ Domain: entry.domain, Audience: 'cli', Scope: 'io.cozy.files' })
  const minted = await request(new URL(`/instances/token?${query}`, adminURL), 'POST',
    { Authorization: `Basic ${Buffer.from(credentials).toString('base64')}` }, 60_000)
  check(minted.status === 200 && minted.body && !/[\s\x00-\x1f\x7f]/.test(minted.body), 'Invalid instance token')
  const token = minted.body
  const resources = async () => parseResources((await request(new URL('/metrics', adminURL), 'GET',
    { Authorization: `Basic ${Buffer.from(credentials).toString('base64')}` }, 60_000)).body)
  const resourcesBefore = config.soak ? await resources() : null
  const files = treeFiles(manifest.generation_seed!)
    .map((f, index) => ({ id: entry.files[index]!, md5: f.md5, size: f.size }))
  const fixturePath = `data/results/${config.executionId}-mixed-fixture.json`
  writeFileSync(`${harness}${fixturePath}`, JSON.stringify({ dirs: [ROOT, ...entry.dirs ?? []], files }),
    { mode: 0o600 })
  const scheme = new URL(stackOrigin).protocol
  const target: Target = {
    kind: 'local', baseUrl: `${scheme}//${entry.domain}`, lockedUrls: [], maxPointMs: Infinity,
    makeArgs: [`ACTOR_HOSTS=${new URL(`${scheme}//${entry.domain}`).hostname}=${stackAddress()}`],
    session: async () => ({ token, stack: stackClient(stackOrigin, entry.domain, token) }),
  }

  const reportPath = `data/results/${config.executionId}-mixed-workload.json`
  rmSync(`${harness}${reportPath}`, { force: true })
  const workStartedAt = new Date().toISOString()
  // An interruption stops k6; the run's directory, including writes still landing, is removed after it.
  const run = await runK6Work(target, {
    executionId: config.executionId,
    makeTarget: 'mixed-run',
    args: [`MIXED_RATE=${config.rate}`, `MIXED_DURATION=${config.durationMs}ms`, `MIXED_MAX_VUS=${config.maxVus}`,
      `MIXED_WARMUP=${config.warmupMs}ms`, `MIXED_OBSERVERS=${config.observers}`,
      `MIN_SUCCESS_RATE=${config.minSuccessRate}`, `P95_LIMIT_MS=${config.p95LimitMs}`],
    deadline,
    cleanup: { prefix: `twake-load-mixed-${config.executionId}-`, dirs: [ROOT] },
  })
  rmSync(`${harness}${fixturePath}`, { force: true })
  const workEndedAt = new Date().toISOString()
  // The Stack settles before the final state is read; an interrupted run does not wait.
  if (interruption() === null) await new Promise(resolve => setTimeout(resolve, config.drainMs))
  const drainEndedAt = new Date().toISOString()
  // The run's own directory is gone by now, so the fixture must verify as seeded.
  const after = verify(config.domain)
  let resourcesAfter: Record<string, number> | null = null
  let resourceProblem = ''
  try { resourcesAfter = resourcesBefore && await resources() } catch (error) { resourceProblem = message(error) }
  const endedAt = new Date().toISOString()

  let verdict: Record<string, any> | null = null
  try { verdict = JSON.parse(readFileSync(`${harness}${reportPath}`, 'utf8')) } catch {}
  const problems = [run.timeout, ...run.problems, ...(verdict ? [] : ['k6 wrote no mixed workload report']),
    ...(after ? [`the fixture did not verify after the run: ${after}`] : []), resourceProblem,
    ...(run.k6.exit_code === 0 ? [] : [`k6 exited ${run.k6.exit_code}`])].filter(Boolean)
  const report: Record<string, any> = {
    ...verdict, problems, load_target: 'local', fixture: config.domain, budgets: config.budgets,
    needs: config.needs, cleanup: run.cleanup, k6: run.k6, started_at: startedAt, ended_at: endedAt,
    profile: config.profile,
    // Warm-up and steady activity run in one k6 run; the drain and the final verification follow it.
    phases: {
      warmup_ms: config.warmupMs, steady_ms: config.durationMs,
      work: { started_at: workStartedAt, ended_at: workEndedAt },
      drain: { started_at: workEndedAt, ended_at: drainEndedAt },
      final: { started_at: drainEndedAt, ended_at: endedAt, fixture_verified: !after },
    },
  }
  if (resourcesBefore && resourcesAfter) {
    const failures = judgeRecovery(resourcesBefore, resourcesAfter, config.recovery)
    report.recovery = { criteria: config.recovery, before: resourcesBefore, after: resourcesAfter,
      recovered: !failures.length }
    // A Stack that keeps what the run allocated is a regression, whatever the rates.
    if (failures.length && report.status === 'passed') report.status = 'failed'
    report.reasons = [...report.reasons ?? [], ...failures]
  }
  const { code, reason } = classifyMixed(report)
  report.outcome = code
  report.failure_reason = reason || null
  report.versions = await readVersions(target)
  report.result = mixedEnvelope(report, {
    interrupted: interruption() !== null, versions: report.versions, topology: topologyOf('local'),
    start: startedAt, end: endedAt,
  })
  writeFileSync(`${harness}${reportPath}`, `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 })
  console.log(`Mixed workload outcome ${code}${reason ? ` (${reason})` : ''}: ${reportPath}`)
  return code
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().then(code => { process.exitCode = code }, (error: unknown) => {
    // Setup problems are never correctness failures.
    console.error(message(error))
    process.exitCode = 2
  })
}
