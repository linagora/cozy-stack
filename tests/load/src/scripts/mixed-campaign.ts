// Runs the mixed arrival-rate workload (README, Mixed workload) against a verified tree fixture
// (SEEDING.md, Profiles) on a local Stack, through the exact-work runner (upload/exact-work.ts).
// Budgets are checked before any request, the fixture is verified before and after the run, and the
// run's own uploads are removed. k6 judges the rates; this runner adds what it verified.
import { spawnSync } from 'node:child_process'
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { treeFiles } from './dataset-profile.ts'
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
// Each workflow lists, reads and uploads once; every observer opens at least one session.
const REQUESTS_PER_WORKFLOW = 3
const UPLOAD_BYTES = 1024
// k6 setup and teardown, the runner's verification and cleanup, on top of the arrivals.
const OVERHEAD_MS = 180_000

// Every setting and budget is checked here, before the first request.
export function readConfig(env: Env) {
  const executionId = env.EXECUTION_ID || ''
  check(/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(executionId), 'EXECUTION_ID must be a file-name-safe ID')
  // shortcut: the Cloudery seeder has no tree profile yet, so shared DEV has no fixture to run against.
  check((env.LOAD_TARGET || 'local') === 'local', 'The mixed workload runs on a local tree fixture only')
  const domain = env.MIXED_DOMAIN
  check(domain, 'MIXED_DOMAIN must name a tree fixture (make seed-scale SEED_PROFILE=tree)')
  const rate = getPositiveInteger('MIXED_RATE', env.MIXED_RATE || '1')
  const durationMs = getDurationMs('MIXED_DURATION', env.MIXED_DURATION || '2m')
  const maxVus = getPositiveInteger('MIXED_MAX_VUS', env.MIXED_MAX_VUS || '10')
  // Make passes unset settings as empty strings.
  const observers = Number(env.MIXED_OBSERVERS || '1')
  check(Number.isSafeInteger(observers) && observers >= 0, 'MIXED_OBSERVERS must be a non-negative integer')
  const workflows = rate * Math.ceil(durationMs / 1000)
  const needs = { vus: maxVus + observers, requests: workflows * REQUESTS_PER_WORKFLOW + observers,
    bytes: workflows * UPLOAD_BYTES, runtimeMs: durationMs + OVERHEAD_MS }
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
  return {
    executionId, domain, rate, durationMs, maxVus, observers, needs, budgets, deadlineMs: runtimeMs,
    minSuccessRate: env.MIN_SUCCESS_RATE || '0.99', p95LimitMs: env.P95_LIMIT_MS || '2000',
  }
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
  const startedAt = new Date().toISOString()
  const deadline = Date.now() + config.deadlineMs
  const manifest = readManifest(env.SEED_MANIFEST || manifestPath(config.domain), config.domain)
  const entry = manifest.instances.find(i => i.files.length)
  check(manifest.status === 'complete' && manifest.profile === 'tree' && entry,
    `${config.domain} is not a complete tree fixture`)
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
  const files = treeFiles(manifest.generation_seed!)
    .map((f, index) => ({ id: entry.files[index]!, md5: f.md5, size: f.size }))
  const fixturePath = `data/results/${config.executionId}-mixed-fixture.json`
  mkdirSync(`${harness}data/results`, { recursive: true })
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
  const run = await runK6Work(target, {
    executionId: config.executionId,
    makeTarget: 'mixed-run',
    args: [`MIXED_RATE=${config.rate}`, `MIXED_DURATION=${config.durationMs}ms`, `MIXED_MAX_VUS=${config.maxVus}`,
      `MIXED_OBSERVERS=${config.observers}`, `MIN_SUCCESS_RATE=${config.minSuccessRate}`,
      `P95_LIMIT_MS=${config.p95LimitMs}`],
    deadline,
    cleanup: { prefix: `twake-load-mixed-${config.executionId}-`, dirs: [ROOT] },
  })
  rmSync(`${harness}${fixturePath}`, { force: true })
  // The run's own directory is gone by now, so the fixture must verify as seeded.
  const after = verify(config.domain)
  const endedAt = new Date().toISOString()

  let verdict: Record<string, any> | null = null
  try { verdict = JSON.parse(readFileSync(`${harness}${reportPath}`, 'utf8')) } catch {}
  const problems = [run.timeout, ...run.problems, ...(verdict ? [] : ['k6 wrote no mixed workload report']),
    ...(after ? [`the fixture did not verify after the run: ${after}`] : []),
    ...(run.k6.exit_code === 0 ? [] : [`k6 exited ${run.k6.exit_code}`])].filter(Boolean)
  const report: Record<string, any> = {
    ...verdict, problems, load_target: 'local', fixture: config.domain, budgets: config.budgets,
    needs: config.needs, cleanup: run.cleanup, k6: run.k6, started_at: startedAt, ended_at: endedAt,
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
