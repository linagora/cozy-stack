// Runs the shared-drive activity ramp through the exact-work runner (upload/exact-work.ts) on a
// local or shared DEV target (upload/targets.ts). k6 owns the ramp and its verdict; the runner owns
// the test drive, budgets, deadline, recovery and cleanup, and adds what it verified to k6's report.
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { getUploadDataSizeBytes, isUploadDataSize } from '../fixtures/upload-sizes.ts'
import { getRampTotalDurationMs, parseRampCriteria, parseRampPlan } from '../lib/load-profile.ts'
import { buildGrafanaLink, classifyRamp } from './campaign-summary.ts'
import { DEFAULT_DRIVE_NAME } from './prepare-shared-drive.ts'
import { getBytes, getDurationMs, getPositiveInteger } from './upload/campaign-config.ts'
import {
  check, harness, message, readVersions, recover, runK6Work, trapInterrupts, type Stack, type Target,
} from './upload/exact-work.ts'
import { devTarget, localTarget } from './upload/targets.ts'

type Env = Record<string, string | undefined>
// Setup, the changes readers' drain and teardown, on top of the ramp itself.
const OVERHEAD_MS = 120_000

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
  const peakVus = Math.max(...plan.levels) * (writers + wsReaders + changesReaders)
  // Each writer sleeps a second between uploads, so a level uploads at most one file per writer-second.
  const spanS = (plan.transitionMs + plan.holdMs) / 1000
  const maxBytes = plan.levels.reduce((sum, level) => sum + level * writers * spanS, 0) * getUploadDataSizeBytes(fileSize)
  const durationMs = getRampTotalDurationMs(plan) + OVERHEAD_MS
  const budgets = {
    max_vus: env.MAX_VUS ? getPositiveInteger('MAX_VUS', env.MAX_VUS) : null,
    max_upload_bytes: env.MAX_UPLOAD_BYTES ? getBytes('MAX_UPLOAD_BYTES', env.MAX_UPLOAD_BYTES) : null,
    max_runtime: env.MAX_RUNTIME || null,
  }
  const runtimeMs = budgets.max_runtime ? getDurationMs('MAX_RUNTIME', budgets.max_runtime) : Infinity
  check(budgets.max_vus === null || peakVus <= budgets.max_vus, `The highest level runs ${peakVus} VUs, beyond MAX_VUS`)
  check(budgets.max_upload_bytes === null || maxBytes <= budgets.max_upload_bytes,
    `The ramp can upload ${maxBytes} bytes, beyond MAX_UPLOAD_BYTES`)
  check(durationMs <= runtimeMs, `The ramp needs ${Math.ceil(durationMs / 60_000)} minutes, beyond MAX_RUNTIME`)
  return { loadTarget, executionId, fileSize, budgets, durationMs, deadlineMs: runtimeMs }
}

// Reuses the owner's test drive, or creates it, and returns its sharing and root directory IDs.
async function ensureDrive(stack: Stack): Promise<{ id: string, root: string }> {
  const rootOf = (drive: any): string | undefined => drive?.attributes?.rules?.[0]?.values?.[0]
  const drives: any[] = (await stack('GET', '/sharings/drives', [200])).data ?? []
  const drive = drives.find(d => d.attributes?.description === DEFAULT_DRIVE_NAME && d.attributes?.active === true &&
    d.attributes?.owner === true && d.attributes?.drive_root_type === 'directory' && rootOf(d)) ??
    (await stack('POST', '/sharings/drives', [200, 201], Infinity, {
      data: { type: 'io.cozy.sharings', attributes: { name: DEFAULT_DRIVE_NAME, description: DEFAULT_DRIVE_NAME } },
    })).data
  check(drive?.id && rootOf(drive), 'The test shared drive has no ID or root directory')
  return { id: drive.id, root: rootOf(drive)! }
}

async function main(): Promise<number> {
  trapInterrupts()
  const env: Env = process.env
  const config = readConfig(env)
  const target: Target = config.loadTarget === 'dev' ? await devTarget(env) : localTarget(env)
  // One k6 run holds one token for the whole ramp.
  check(config.durationMs <= target.maxPointMs,
    `The ramp needs ${Math.ceil(config.durationMs / 60_000)} minutes, beyond one k6 run on this target`)
  const { stack } = await target.session()
  check(stack, `The shared-drive ramp needs a Stack it can reach with a token: ${target.baseUrl}`)
  const drive = await ensureDrive(stack)
  const recovery = target.lockedUrls.length ? await recover(target, [drive.root]) : null

  const reportPath = `data/results/${config.executionId}-shared-drive-ramp.json`
  mkdirSync(`${harness}data/results`, { recursive: true })
  rmSync(`${harness}${reportPath}`, { force: true })
  const startedAt = new Date().toISOString()
  const run = await runK6Work(target, {
    executionId: config.executionId,
    makeTarget: 'shared-drive-ramp-run',
    args: ['UPLOAD_TARGET=shared-drive', `SHARED_DRIVE_ID=${drive.id}`, `SHARED_DRIVE_ROOT_ID=${drive.root}`,
      `FILE_SIZE=${config.fileSize}`],
    deadline: Date.now() + config.deadlineMs,
    cleanup: { prefix: `twake-load-sd-${config.executionId}-`, dirs: [drive.root] },
  })
  const endedAt = new Date().toISOString()

  let verdict: Record<string, any> | null = null
  try { verdict = JSON.parse(readFileSync(`${harness}${reportPath}`, 'utf8')) } catch {}
  const problems = [run.timeout, ...run.problems, ...(verdict ? [] : ['k6 wrote no ramp report'])].filter(Boolean)
  // Anything unverified leaves k6's levels as evidence, never as a result.
  const report: Record<string, unknown> = {
    ...verdict,
    ...(problems.length ? {
      status: 'infrastructure_failure', result_bound: 'inconclusive',
      failure_reason: [verdict?.failure_reason, ...problems].filter(Boolean).join('; '),
    } : {}),
    load_target: target.kind,
    file_size: config.fileSize,
    budgets: config.budgets,
    recovery,
    cleanup: run.cleanup,
    k6: run.k6,
    started_at: startedAt,
    ended_at: endedAt,
  }
  const { code, reason } = classifyRamp(report)
  report.outcome = code
  report.versions = await readVersions(target)
  report.monitoring = {
    k6: buildGrafanaLink(env.GRAFANA_URL || 'http://localhost:3000', {
      name: 'shared-drive-ramp', kind: 'shared-drive-ramp', id: config.executionId, startedAt, endedAt,
      exitCode: code, producerExitCode: code,
    }),
    ...(target.kind === 'dev' ? { dev: env.DEV_MONITORING_URL || 'unknown' } : {}),
  }
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
