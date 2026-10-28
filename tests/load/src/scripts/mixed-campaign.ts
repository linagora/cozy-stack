// Runs the mixed arrival-rate workload (README, Mixed workload) against a verified tree fixture
// (SEEDING.md, Profiles) on a local Stack or shared DEV, through the exact-work runner (upload/exact-work.ts).
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
import { mixedCapacityEnvelope, mixedEnvelope, topologyOf } from './result-envelope.ts'
import { getBytes, getDurationMs, getPositiveInteger } from './upload/campaign-config.ts'
import {
  check, harness, interruption, message, readVersions, recover, runK6Work, stackClient, trapInterrupts, type Target,
} from './upload/exact-work.ts'
import { devTarget } from './upload/targets.ts'

type Env = Record<string, string | undefined>
const ROOT = 'io.cozy.files.root-dir'
// Each workflow lists, reads and uploads once; each observer opens one session.
const REQUESTS_PER_WORKFLOW = 3
const UPLOAD_BYTES = 1024
// k6 setup and teardown, the runner's verification and cleanup, on top of the arrivals.
const OVERHEAD_MS = 180_000
// A soak longer than this needs a passed shorter trial at its rate first.
const TRIAL_MAX_MS = 10 * 60_000
// A capacity search gives each rate at least this many VUs per workflow/s, so a workflow may take this
// many seconds before arrivals drop.
const VUS_PER_RATE = 8
// Starting and stopping k6 again, and cleaning up, for each further rate of a capacity search.
const LEVEL_OVERHEAD_MS = 60_000

// Every setting and budget is checked here, before the first request.
export function readConfig(env: Env) {
  const executionId = env.EXECUTION_ID || ''
  check(/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(executionId), 'EXECUTION_ID must be a file-name-safe ID')
  const loadTarget = env.LOAD_TARGET || 'local'
  check(loadTarget === 'local' || loadTarget === 'dev', 'LOAD_TARGET must be local or dev')
  const dev = loadTarget === 'dev'
  check(!dev || (env.MAX_VUS && env.MAX_REQUESTS && env.MAX_UPLOAD_BYTES && env.MAX_RUNTIME),
    'Shared DEV needs MAX_VUS, MAX_REQUESTS, MAX_UPLOAD_BYTES and MAX_RUNTIME agreed with the DEV owner')
  const domain = env.MIXED_DOMAIN
  check(domain, 'MIXED_DOMAIN must name a tree fixture (make seed-scale SEED_PROFILE=tree)')
  const manifestFile = env.SEED_MANIFEST || manifestPath(domain)
  const manifest = readManifest(manifestFile, domain)
  const entry = manifest.instances[0]!
  // One instance: verifying and cleaning it then costs a known number of requests.
  check(manifest.status === 'complete' && manifest.profile === 'tree' && manifest.profile_version === TREE_VERSION &&
    !manifest.organization && entry.files.length,
  `${domain} is not a complete single-instance tree fixture, version ${TREE_VERSION} ` +
    '(make seed-scale SEED_PROFILE=tree)')
  const profile = env.MIXED_PROFILE || 'fixed'
  check(['fixed', 'soak', 'capacity'].includes(profile), 'MIXED_PROFILE must be fixed, soak or capacity')
  const soak = profile === 'soak'
  const capacity = profile === 'capacity'
  check(!dev || !soak, 'A soak samples the Stack admin metrics, which shared DEV does not expose')
  // A capacity search steps through increasing rates, one k6 run each; fixed and soak runs keep one rate.
  const rates = capacity ? (env.MIXED_RATES || '1,2,4,8').split(',').map(r => getPositiveInteger('MIXED_RATES', r))
    : [getPositiveInteger('MIXED_RATE', env.MIXED_RATE || '1')]
  check(rates.every((r, i) => i === 0 || r > rates[i - 1]!), 'MIXED_RATES must be increasing workflows per second')
  const rate = rates[0]!
  const durationMs = getDurationMs('MIXED_DURATION', env.MIXED_DURATION || (soak ? '10m' : capacity ? '1m' : '2m'))
  const warmupMs = getDurationMs('MIXED_WARMUP', env.MIXED_WARMUP || (soak ? '1m' : '0s'))
  const drainMs = getDurationMs('MIXED_DRAIN', env.MIXED_DRAIN || (soak ? '1m' : '0s'))
  const maxVus = getPositiveInteger('MIXED_MAX_VUS', env.MIXED_MAX_VUS || '10')
  // Make passes unset settings as empty strings.
  const observers = Number(env.MIXED_OBSERVERS || '1')
  check(Number.isSafeInteger(observers) && observers >= 0, 'MIXED_OBSERVERS must be a non-negative integer')
  const graceMs = warmupMs ? WARMUP_GRACE_MS : 0
  const levels = rates.map(r => ({ rate: r, maxVus: capacity ? Math.max(maxVus, VUS_PER_RATE * r) : maxVus }))
  // The caps cover the warm-up, the observers, both fixture verifications and cleanup too. k6 starts one
  // more arrival at the very start of each arrival scenario than rate × seconds.
  const workflows = rates.reduce((sum, r) =>
    sum + r * Math.ceil((warmupMs + durationMs) / 1000) + (warmupMs ? 2 : 1), 0)
  // A verification reads the instance, mints a token, then reads every directory, file and drive root and
  // the root's entry count.
  const verifyRequests = 3 + (entry.dirs?.length ?? 0) + entry.files.length + entry.drives.length
  // Cleanup lists the root (100 entries a page) and the trash before and after deleting the run's
  // directory, in two requests. shortcut: the trash is taken to fit one page, as on a fixture only these
  // runs write to; count its pages too once fixtures are shared.
  const cleanupRequests = 2 * (Math.ceil(((entry.root_entries ?? 0) + 1) / 100) + 1) + 2
  // Each k6 run's setup creates its directory, which cleanup removes; the runner mints a token, reads the
  // Stack version and, in a soak, samples the Stack's metrics before and after. On shared DEV, the
  // runner also reads the instance and lists the root and the trash for an earlier run's leftovers.
  const needs = {
    vus: Math.max(...levels.map(l => l.maxVus)) + observers, bytes: workflows * UPLOAD_BYTES,
    runtimeMs: rates.length * (warmupMs + graceMs + durationMs) + (rates.length - 1) * LEVEL_OVERHEAD_MS + drainMs +
      OVERHEAD_MS,
    requests: workflows * REQUESTS_PER_WORKFLOW + rates.length * (observers + 1 + cleanupRequests) +
      2 * verifyRequests + 2 + (soak ? 2 : 0) + (dev ? 3 : 0),
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
    executionId, loadTarget, domain, manifestFile, manifest, entry, profile, soak, capacity, rate, levels, durationMs,
    warmupMs, graceMs, drainMs, maxVus, observers, needs, budgets, recovery, deadlineMs: runtimeMs, ...criteria(env),
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

// The fixture as seeded, or the reason it is not: through the Stack admin API locally, through the
// Cloudery on shared DEV.
function verify({ loadTarget, domain }: { loadTarget: string, domain: string }): string {
  const dev = loadTarget === 'dev'
  const seeder = fileURLToPath(new URL(dev ? './seed-cloudery.ts' : './seed-scale-data.ts', import.meta.url))
  const [prefix, ...suffix] = domain.split('.')
  const run = spawnSync(process.execPath, ['--import', 'tsx', seeder, ...(dev ? ['verify-tree'] : ['verify', domain])], {
    encoding: 'utf8',
    env: dev ? { ...process.env, SEED_CLOUDERY_PREFIX: prefix, SEED_CLOUDERY_DOMAIN: suffix.join('.') } : process.env,
  })
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

// The highest rate that passed, and the first that failed. The search stops at the first rate that did
// not pass, so the rates that passed come first.
export function judgeCapacity(levels: Record<string, any>[], planned: number, problems: string[]) {
  const passed = levels.filter(l => l.status === 'passed' && !l.problems.length)
  const last = levels.at(-1)
  const failed = last?.status === 'failed' && !last.problems.length ? last : undefined
  const allProblems = [...levels.flatMap(l => l.problems.map((p: string) => `${l.rate} workflows/s: ${p}`)),
    ...problems]
  const correctnessFailures = levels.flatMap(l => (l.correctness_failures ?? [])
    .map((f: string) => `${l.rate} workflows/s: ${f}`))
  const resultBound = failed ? 'bracket' : passed.length === planned ? 'lower_bound' : 'inconclusive'
  return {
    max_sustained_rate: passed.at(-1)?.rate ?? 0,
    first_failed_rate: failed?.rate ?? null,
    result_bound: resultBound,
    status: correctnessFailures.length ? 'correctness_failure'
      : allProblems.length || resultBound === 'inconclusive' ? 'inconclusive' : 'completed',
    correctness_failures: correctnessFailures,
    problems: allProblems,
    reasons: last && last.status !== 'passed'
      ? (last.reasons ?? []).map((r: string) => `${last.rate} workflows/s: ${r}`) : [],
  }
}

// As the shared-drive ramp: a failure at the first rate is a proven regression, at a later one a bracket.
export function classifyMixedCapacity(report: Record<string, any>): { code: 0 | 1 | 2, reason: string } {
  if (report.correctness_failures.length) return { code: 1, reason: report.correctness_failures.join('; ') }
  if (report.problems.length) return { code: 2, reason: report.problems.join('; ') }
  if (report.result_bound === 'bracket' && report.max_sustained_rate === 0) {
    return { code: 1, reason: `the first rate failed: ${report.reasons.join('; ')}` }
  }
  if (report.result_bound === 'inconclusive') {
    return { code: 2, reason: report.reasons.join('; ') || 'the search measured no rate' }
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

// The bundled Stack, with a token and metrics from its admin API.
async function localTarget(env: Env, domain: string) {
  const stackOrigin = env.SEED_STACK_URL
  const adminURL = env.SEED_ADMIN_URL
  check(stackOrigin && adminURL && env.SEED_ADMIN_PASSWORD,
    'SEED_ADMIN_URL, SEED_ADMIN_PASSWORD and SEED_STACK_URL are required')
  const credentials = `${env.SEED_ADMIN_USERNAME || 'admin'}:${env.SEED_ADMIN_PASSWORD}`
  const query = new URLSearchParams({ Domain: domain, Audience: 'cli', Scope: 'io.cozy.files' })
  const minted = await request(new URL(`/instances/token?${query}`, adminURL), 'POST',
    { Authorization: `Basic ${Buffer.from(credentials).toString('base64')}` }, 60_000)
  check(minted.status === 200 && minted.body && !/[\s\x00-\x1f\x7f]/.test(minted.body), 'Invalid instance token')
  const token = minted.body
  const scheme = new URL(stackOrigin).protocol
  const target: Target = {
    kind: 'local', baseUrl: `${scheme}//${domain}`, lockedUrls: [], maxPointMs: Infinity,
    makeArgs: [`ACTOR_HOSTS=${new URL(`${scheme}//${domain}`).hostname}=${stackAddress()}`],
    session: async () => ({ token, stack: stackClient(stackOrigin, domain, token) }),
  }
  const resources = async () => parseResources((await request(new URL('/metrics', adminURL), 'GET',
    { Authorization: `Basic ${Buffer.from(credentials).toString('base64')}` }, 60_000)).body)
  return { target, resources }
}

async function run(env: Env, config: ReturnType<typeof readConfig>): Promise<number> {
  const startedAt = new Date().toISOString()
  const deadline = Date.now() + config.deadlineMs
  const { manifest, entry } = config
  const { target, resources } = config.loadTarget === 'dev'
    // The Cloudery mints each rate's token for the fixture instance alone; k6 reaches DEV by name.
    ? { target: { ...await devTarget({ ...env, DEV_INSTANCE: entry.domain, SEED_CLOUDERY_MANIFEST: config.manifestFile }),
      makeArgs: [] }, resources: null }
    : await localTarget(env, entry.domain)
  // The DEV fixture outlives runs: what an interrupted earlier run left in it goes first, under the DEV lock.
  if (config.loadTarget === 'dev') await recover(target)
  const before = verify(config)
  check(!before, `The fixture did not verify before the run: ${before}`)
  const resourcesBefore = config.soak ? await resources!() : null
  const files = treeFiles(manifest.generation_seed!)
    .map((f, index) => ({ id: entry.files[index]!, md5: f.md5, size: f.size }))
  const fixture = JSON.stringify({ dirs: [ROOT, ...entry.dirs ?? []], files })

  // One k6 run at one rate, under its own ID. An interruption stops k6; the run's directory, including
  // writes still landing, is removed after it.
  // A search's rates share its run ID in k6 metrics, as an upload search's attempts do; each keeps its own.
  const runIds = config.capacity ? [`CAPACITY_RUN_ID=${config.executionId}`, 'MIXED_MODE=capacity'] : []
  const runRate = async (rate: number, maxVus: number, id: string) => {
    const fixturePath = `${harness}data/results/${id}-mixed-fixture.json`
    writeFileSync(fixturePath, fixture, { mode: 0o600 })
    const reportPath = `${harness}data/results/${id}-mixed-workload.json`
    rmSync(reportPath, { force: true })
    const work = await runK6Work(target, {
      executionId: id,
      makeTarget: 'mixed-run',
      args: [`MIXED_RATE=${rate}`, `MIXED_DURATION=${config.durationMs}ms`, `MIXED_MAX_VUS=${maxVus}`,
        `MIXED_WARMUP=${config.warmupMs}ms`, `MIXED_OBSERVERS=${config.observers}`,
        `MIN_SUCCESS_RATE=${config.minSuccessRate}`, `P95_LIMIT_MS=${config.p95LimitMs}`, ...runIds],
      deadline,
      cleanup: { prefix: `twake-load-mixed-${id}-`, dirs: [ROOT] },
    })
    rmSync(fixturePath, { force: true })
    let verdict: Record<string, any> | null = null
    try { verdict = JSON.parse(readFileSync(reportPath, 'utf8')) } catch {}
    const problems = [work.timeout, ...work.problems, ...(verdict ? [] : ['k6 wrote no mixed workload report']),
      ...(work.k6.exit_code === 0 ? [] : [`k6 exited ${work.k6.exit_code}`])].filter(Boolean)
    return { work, verdict, problems, reportPath }
  }
  const context = (versions: Record<string, string>, endedAt: string) => ({
    interrupted: interruption() !== null, versions, topology: topologyOf(config.loadTarget), start: startedAt,
    end: endedAt,
  })

  if (config.capacity) {
    const levels: Record<string, any>[] = []
    for (const { rate, maxVus } of config.levels) {
      const id = `${config.executionId}-r${rate}`
      const levelStartedAt = new Date().toISOString()
      const { work, verdict, problems, reportPath } = await runRate(rate, maxVus, id)
      // The search's report keeps every rate's verdict.
      rmSync(reportPath, { force: true })
      levels.push({ ...verdict, rate, max_vus: maxVus, execution_id: id, started_at: levelStartedAt,
        ended_at: new Date().toISOString(), problems, cleanup: work.cleanup, k6: work.k6 })
      if (problems.length || verdict?.status !== 'passed' || interruption() !== null) break
    }
    // Every run's own directory is gone by now, so the fixture must verify as seeded.
    const after = verify(config)
    const endedAt = new Date().toISOString()
    const report: Record<string, any> = {
      mode: 'capacity', unit: 'workflows/s', load_target: config.loadTarget, fixture: config.domain,
      dataset: { profile: 'tree', version: TREE_VERSION }, budgets: config.budgets, needs: config.needs,
      plan: { rates: config.levels.map(l => l.rate), duration_s: config.durationMs / 1000,
        observers: config.observers },
      criteria: { minimum_success_rate: config.minSuccessRate, p95_limit_ms: config.p95LimitMs },
      levels, ...judgeCapacity(levels, config.levels.length,
        after ? [`the fixture did not verify after the run: ${after}`] : []),
      started_at: startedAt, ended_at: endedAt,
    }
    const { code, reason } = classifyMixedCapacity(report)
    report.outcome = code
    report.failure_reason = reason || null
    report.versions = await readVersions(target)
    report.result = mixedCapacityEnvelope(report, context(report.versions, endedAt))
    const reportPath = `data/results/${config.executionId}-mixed-capacity.json`
    writeFileSync(`${harness}${reportPath}`, `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 })
    console.log(`Mixed capacity outcome ${code}${reason ? ` (${reason})` : ''}: ${reportPath}`)
    return code
  }

  const workStartedAt = new Date().toISOString()
  const { work, verdict, problems: workProblems, reportPath } = await runRate(config.rate, config.maxVus,
    config.executionId)
  const workEndedAt = new Date().toISOString()
  // The Stack settles before the final state is read; an interrupted run does not wait.
  if (interruption() === null) await new Promise(resolve => setTimeout(resolve, config.drainMs))
  const drainEndedAt = new Date().toISOString()
  // The run's own directory is gone by now, so the fixture must verify as seeded.
  const after = verify(config)
  let resourcesAfter: Record<string, number> | null = null
  let resourceProblem = ''
  try { resourcesAfter = resourcesBefore && await resources!() } catch (error) { resourceProblem = message(error) }
  const endedAt = new Date().toISOString()

  const problems = [...workProblems, ...(after ? [`the fixture did not verify after the run: ${after}`] : []),
    resourceProblem].filter(Boolean)
  const report: Record<string, any> = {
    ...verdict, problems, load_target: config.loadTarget, fixture: config.domain, budgets: config.budgets,
    needs: config.needs, cleanup: work.cleanup, k6: work.k6, started_at: startedAt, ended_at: endedAt,
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
  report.result = mixedEnvelope(report, context(report.versions, endedAt))
  writeFileSync(reportPath, `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 })
  console.log(`Mixed workload outcome ${code}${reason ? ` (${reason})` : ''}: ${reportPath.slice(harness.length)}`)
  return code
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().then(code => { process.exitCode = code }, (error: unknown) => {
    // Setup problems are never correctness failures.
    console.error(message(error))
    process.exitCode = 2
  })
}
