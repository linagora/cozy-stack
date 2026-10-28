// Runs the shared-drive activity ramp through the exact-work runner (upload/exact-work.ts) on a
// local or shared DEV target (upload/targets.ts), one k6 run per level through the step loop
// (capacity/step-ramp.ts): the ramp stops at the first level that does not pass. k6 judges each level;
// the runner owns the test drive, budgets, deadline, recovery and cleanup, and adds what it verified.
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { getUploadDataSizeBytes, isUploadDataSize } from '../fixtures/upload-sizes.ts'
import { getRampTotalDurationMs, parseRampCriteria, parseRampPlan } from '../lib/load-profile.ts'
import { buildGrafanaLink, classifyRamp, K6_THRESHOLDS_FAILED } from './campaign-summary.ts'
import { stepRamp } from './capacity/step-ramp.ts'
import { AccessFailure, prepareActors } from './drive-actors.ts'
import { FixtureError } from './fixture-http.ts'
import { DEFAULT_DRIVE_NAME } from './prepare-shared-drive.ts'
import { rampEnvelope, topologyOf } from './result-envelope.ts'
import { getBytes, getDurationMs, getPositiveInteger } from './upload/campaign-config.ts'
import {
  check, harness, interruption, message, readVersions, recover, runK6Work, trapInterrupts, type Stack, type Target,
} from './upload/exact-work.ts'
import { devTarget, localTarget } from './upload/targets.ts'

type Env = Record<string, string | undefined>
// Preparing the drive and actors, and the final report.
const OVERHEAD_MS = 60_000
// Each level's k6 setup, the changes readers' drain and teardown, on top of its transition and hold.
const LEVEL_OVERHEAD_MS = 20_000

// Every setting and budget is checked here, before the first request.
function readConfig(env: Env) {
  const loadTarget = env.LOAD_TARGET || 'local'
  check(loadTarget === 'local' || loadTarget === 'dev', 'LOAD_TARGET must be local or dev')
  const executionId = env.EXECUTION_ID || ''
  check(/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(executionId), 'EXECUTION_ID must be a file-name-safe ID')
  const fileSize = env.FILE_SIZE || '1K'
  check(isUploadDataSize(fileSize), `Unknown FILE_SIZE ${fileSize}`)
  const plan = parseRampPlan(env)
  parseRampCriteria(env)
  const { writers, wsReaders, changesReaders } = plan.vusPerLevel
  // One level's own k6 run.
  const levelMs = getRampTotalDurationMs({ ...plan, levels: [plan.levels[0]!] }) + LEVEL_OVERHEAD_MS
  // Each writer sleeps a second between uploads, so a level uploads at most one file per writer-second.
  const spanS = (plan.transitionMs + plan.holdMs) / 1000
  // What the first n levels need.
  const needsFor = (n: number) => ({
    vus: plan.levels[n - 1]! * (writers + wsReaders + changesReaders),
    bytes: plan.levels.slice(0, n).reduce((sum, level) => sum + level * writers * spanS, 0) *
      getUploadDataSizeBytes(fileSize),
    runtimeMs: n * levelMs + OVERHEAD_MS,
  })
  const budgets = {
    max_vus: env.MAX_VUS ? getPositiveInteger('MAX_VUS', env.MAX_VUS) : null,
    max_upload_bytes: env.MAX_UPLOAD_BYTES ? getBytes('MAX_UPLOAD_BYTES', env.MAX_UPLOAD_BYTES) : null,
    max_runtime: env.MAX_RUNTIME || null,
  }
  const runtimeMs = budgets.max_runtime ? getDurationMs('MAX_RUNTIME', budgets.max_runtime) : Infinity
  // The budget the first n levels exceed, or ''.
  const exceeds = (n: number) => {
    const at = needsFor(n)
    return budgets.max_vus !== null && at.vus > budgets.max_vus ? 'MAX_VUS'
      : budgets.max_upload_bytes !== null && at.bytes > budgets.max_upload_bytes ? 'MAX_UPLOAD_BYTES'
      : at.runtimeMs > runtimeMs ? 'MAX_RUNTIME' : ''
  }
  // The first level must fit; the ramp stops before a later one that does not, as a lower bound.
  const first = needsFor(1)
  check(budgets.max_vus === null || first.vus <= budgets.max_vus, `The first level runs ${first.vus} VUs, beyond MAX_VUS`)
  check(budgets.max_upload_bytes === null || first.bytes <= budgets.max_upload_bytes,
    `The first level can upload ${first.bytes} bytes, beyond MAX_UPLOAD_BYTES`)
  check(first.runtimeMs <= runtimeMs, `The first level needs ${Math.ceil(first.runtimeMs / 60_000)} minutes, beyond MAX_RUNTIME`)
  return {
    loadTarget, executionId, fileSize, budgets, plan, levelMs, deadlineMs: runtimeMs,
    exceeds: (level: number) => exceeds(plan.levels.indexOf(level) + 1),
  }
}

// Reuses the owner's test drive, or creates it, and returns its sharing and root directory IDs.
async function ensureDrive(stack: Stack, deadline: number): Promise<{ id: string, root: string }> {
  const rootOf = (drive: any): string | undefined => drive?.attributes?.rules?.[0]?.values?.[0]
  const drives: any[] = (await stack('GET', '/sharings/drives', [200], deadline)).data ?? []
  const drive = drives.find(d => d.attributes?.description === DEFAULT_DRIVE_NAME && d.attributes?.active === true &&
    d.attributes?.owner === true && d.attributes?.drive_root_type === 'directory' && rootOf(d)) ??
    (await stack('POST', '/sharings/drives', [200, 201], deadline, {
      data: { type: 'io.cozy.sharings', attributes: { name: DEFAULT_DRIVE_NAME, description: DEFAULT_DRIVE_NAME } },
    })).data
  check(drive?.id && rootOf(drive), 'The test shared drive has no ID or root directory')
  return { id: drive.id, root: rootOf(drive)! }
}

async function main(): Promise<number> {
  trapInterrupts()
  const env: Env = process.env
  const config = readConfig(env)
  const startedAt = new Date().toISOString()
  const deadline = Date.now() + config.deadlineMs
  // Member actors play the drive's roles from an organization fixture (drive-actors.ts).
  check(!env.ACTORS_DOMAIN || config.loadTarget === 'local', 'Shared-drive actors run on a local Stack only')
  const actors = env.ACTORS_DOMAIN ? await prepareActors(env, config.executionId, deadline) : null
  const reportPath = `data/results/${config.executionId}-shared-drive-ramp.json`
  const levelReport = (id: string) => `data/results/${id}-shared-drive-ramp.json`
  const { target, drive, recovery } = await (async () => {
    const target: Target = actors?.target ?? (config.loadTarget === 'dev' ? await devTarget(env) : localTarget(env))
    // Each level's k6 run holds one token.
    check(config.levelMs <= target.maxPointMs,
      `A ramp level needs ${Math.ceil(config.levelMs / 60_000)} minutes, beyond one k6 run on this target`)
    const { stack } = await target.session()
    check(stack, `The shared-drive ramp needs a Stack it can reach with a token: ${target.baseUrl}`)
    check(Date.now() < deadline, 'MAX_RUNTIME expired during ramp setup')
    const drive = actors?.drive ?? await ensureDrive(stack, deadline)
    const recovery = target.lockedUrls.length ? await recover(target, [drive.root]) : null
    mkdirSync(`${harness}data/results`, { recursive: true })
    rmSync(`${harness}${reportPath}`, { force: true })
    return { target, drive, recovery }
  })().catch(async (error: unknown) => {
    // Setup failed once the actors were prepared: their fixtures go all the same.
    if (!actors) throw error
    const { retained } = await actors.abandon()
    throw new FixtureError(`${message(error)}; ${retained.length ? `fixtures remain: ${retained.join(' ')}`
      : 'its fixtures were removed'}`)
  })

  // A level's k6 run shares the ramp's ID in k6 metrics, as a mixed search's rates do.
  const search = await stepRamp(config.plan.levels, async level => {
    const id = `${config.executionId}-l${level}`
    const startedAt = new Date().toISOString()
    rmSync(`${harness}${levelReport(id)}`, { force: true })
    const run = await runK6Work(target, {
      executionId: id,
      makeTarget: 'shared-drive-ramp-run',
      args: ['UPLOAD_TARGET=shared-drive', `SHARED_DRIVE_ID=${drive.id}`, `SHARED_DRIVE_ROOT_ID=${drive.root}`,
        `FILE_SIZE=${config.fileSize}`, `CAPACITY_START=${level}`, `CAPACITY_MAX=${level}`,
        `CAPACITY_RUN_ID=${config.executionId}`],
      deadline,
      // Every level's data, and any this run left earlier: the levels run one after another.
      cleanup: { prefix: `twake-load-sd-${config.executionId}-`, dirs: [drive.root] },
      ...(actors ? { env: actors.env } : {}),
    })
    let verdict: Record<string, any> | null = null
    try { verdict = JSON.parse(readFileSync(`${harness}${levelReport(id)}`, 'utf8')) } catch {}
    // The ramp's report keeps every level.
    rmSync(`${harness}${levelReport(id)}`, { force: true })
    const result = verdict?.levels?.[0] ?? { level, status: 'not_measured', reasons: [] }
    const problems = [run.timeout, ...run.problems, ...(verdict ? [] : ['k6 wrote no ramp report']),
      ...(verdict && verdict.status !== 'completed' ? [verdict.failure_reason ?? 'k6 could not judge the level'] : []),
      ...([0, K6_THRESHOLDS_FAILED].includes(run.k6.exit_code) ? [] : [`k6 exited ${run.k6.exit_code}`])]
      .filter(Boolean)
    return {
      ...result, execution_id: id, started_at: startedAt, ended_at: new Date().toISOString(), problems,
      correctness_failures: verdict?.correctness_failures ?? [], criteria: verdict?.criteria,
      cleanup_success_rate: verdict?.cleanup_success_rate ?? null, cleanup: run.cleanup, k6: run.k6,
      verdict: problems.length ? 'inconclusive' as const : result.status === 'passed' ? 'passed' as const
        : result.status === 'failed' ? 'failed' as const : 'inconclusive' as const,
      reasons: result.reasons ?? [],
    }
  }, { fits: config.exceeds, interrupted: () => interruption() !== null })
  let accessFailure = ''
  let actorProblem = ''
  try { await actors?.finish() } catch (error) {
    if (error instanceof AccessFailure) accessFailure = error.message
    else actorProblem = message(error)
  }
  const endedAt = new Date().toISOString()

  const { levels } = search
  const problems = [...levels.flatMap(l => l.problems.map((p: string) => `level ${l.level}: ${p}`)), actorProblem]
    .filter(Boolean)
  const cleanupRates = levels.map(l => l.cleanup_success_rate)
  const cleanups = levels.map(l => l.cleanup)
  // Anything unverified leaves the passed levels as evidence, never as a result.
  const report: Record<string, unknown> = {
    status: problems.length ? 'infrastructure_failure' : 'completed',
    result_bound: problems.length ? 'inconclusive' : search.bound,
    max_sustained_level: search.lower,
    first_failed_level: search.upper,
    budget_reached: search.budget,
    failure_reason: problems.join('; ') || null,
    correctness_failures: levels.flatMap(l => l.correctness_failures.map((f: string) => `level ${l.level}: ${f}`)),
    criteria: levels.find(l => l.criteria)?.criteria ?? null,
    plan: { hold_ms: config.plan.holdMs, levels: config.plan.levels, transition_ms: config.plan.transitionMs,
      vus_per_level: config.plan.vusPerLevel },
    levels,
    // k6's own teardown checks, the lowest of them.
    cleanup_success_rate: cleanupRates.every(r => typeof r === 'number') ? Math.min(...cleanupRates) : null,
    load_target: target.kind,
    file_size: config.fileSize,
    budgets: config.budgets,
    recovery,
    ...(actors ? { actors: actors.report } : {}),
    ...(accessFailure ? { access_failure: accessFailure } : {}),
    cleanup: cleanups.every(Boolean) ? { cleaned: cleanups.flatMap(c => c!.cleaned),
      remaining: cleanups.flatMap(c => c!.remaining) } : null,
    k6: levels.at(-1)?.k6 ?? null,
    started_at: startedAt,
    ended_at: endedAt,
  }
  // A proven access failure outweighs any measured level.
  if (accessFailure) report.failure_reason = [accessFailure, report.failure_reason].filter(Boolean).join('; ')
  const { code, reason } = accessFailure ? { code: 1, reason: accessFailure } : classifyRamp(report)
  if (code !== 0 && !report.failure_reason) report.failure_reason = reason
  report.outcome = code
  const versions = await readVersions(target)
  report.versions = versions
  report.monitoring = {
    k6: buildGrafanaLink(env.GRAFANA_URL || 'http://localhost:3000', {
      name: 'shared-drive-ramp', kind: 'shared-drive-ramp', id: config.executionId, startedAt, endedAt,
      exitCode: code, producerExitCode: code,
    }, { loadTarget: target.kind, mode: 'capacity' }),
    ...(target.kind === 'dev' ? { dev: env.DEV_MONITORING_URL || 'unknown' } : {}),
  }
  report.result = rampEnvelope(report, {
    interrupted: interruption() !== null, versions, topology: topologyOf(target.kind), start: startedAt, end: endedAt,
  })
  writeFileSync(`${harness}${reportPath}`, `${JSON.stringify(report, null, 2)}\n`, { mode: 0o600 })
  console.log(`Shared-drive ramp outcome ${code}${reason ? ` (${reason})` : ''}: ${reportPath}`)
  return code
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  main().then(code => { process.exitCode = code }, (error: unknown) => {
    // Setup problems are never correctness failures.
    console.error(message(error))
    process.exitCode = 2
  })
}
