import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

import { classifyStep } from './campaign-summary.ts'
import { validateEnvelope } from './result-envelope.ts'
import { fakeActors, org, writer } from './upload/fake-actors.ts'
import { fakeTarget, fqdn } from './upload/fake-target.ts'

const script = fileURLToPath(new URL('./shared-drive-campaign.ts', import.meta.url))
// The default plan, five 70-second levels up to 16 in a k6 run each, runs 96 VUs and at most 2,170 writer
// uploads in about nine minutes.
const budgets = { MAX_VUS: '96', MAX_UPLOAD_BYTES: '3M', MAX_RUNTIME: '10m' }
const fixture = async (t: TestContext) => {
  const f = await fakeTarget(t, script)
  return { ...f, run: (env: Record<string, string> = {}) => f.run({ ...budgets, ...env }) }
}

test('rejects a ramp whose first level is beyond its budgets before any request', async t => {
  const f = await fixture(t)
  for (const env of [{ MAX_VUS: '5' }, { MAX_UPLOAD_BYTES: '60K' }, { MAX_RUNTIME: '2m' }, { MAX_RUNTIME: '' },
    { CAPACITY_GROWTH: '1' }, { EXECUTION_ID: '../x' }]) {
    const { code, output } = await f.run(env)
    equal(code, 2, output)
  }
  deepEqual(f.state.requests, [])
  deepEqual(f.docker(), [])
  // Each level's k6 run holds one 30-minute instance token, so a level longer than a DEV run never starts.
  const { code, output } = await f.run({ CAPACITY_HOLD: '25m', MAX_RUNTIME: '3h', MAX_UPLOAD_BYTES: '20M' })
  equal(code, 2, output)
  match(output, /beyond one k6 run on this target/)
  deepEqual(f.state.requests, [`127.0.0.1 POST /api/public/instances/${fqdn}/token`, `${fqdn} GET /settings/instance`])
})

test('ramp setup cannot reset MAX_RUNTIME before creating its drive', async t => {
  const f = await fixture(t)
  f.state.tokenDelayMs = 1200
  const clock = join(f.dockerDir, 'clock.mjs')
  writeFileSync(clock, 'const real = Date.now, start = real()\nDate.now = () => start + (real() - start) * 100\n')
  const { code, output } = await f.start({ ...budgets, CAPACITY_MAX: '1', CAPACITY_HOLD: '5s',
    MAX_RUNTIME: '200s' }, ['--import', clock]).done
  equal(code, 2, output)
  match(output, /MAX_RUNTIME/)
  ok(!f.state.requests.some(r => r.endsWith('POST /sharings/drives')))
  ok(!f.docker().some(c => c.args?.[1] === 'run'))
})

test('creates the test drive once, recovers earlier data and keeps the ramp verdict', async t => {
  const f = await fixture(t)
  f.state.root.set('old', 'twake-load-1k-old')
  const { code, output } = await f.run()
  equal(code, 0, output)
  equal(f.state.drives.length, 1)
  const report = f.report('shared-drive-ramp')
  equal(report.outcome, 0)
  equal(report.result_bound, 'lower_bound')
  equal(report.max_sustained_level, 16)
  deepEqual(report.levels.map((l: any) => [l.level, l.execution_id]), [1, 2, 4, 8, 16].map(l => [l, `${f.id}-l${l}`]))
  equal(report.load_target, 'dev')
  deepEqual(report.budgets, { max_vus: 96, max_upload_bytes: 3 << 20, max_runtime: '10m' })
  deepEqual(report.recovery, { k6_stopped: [], cleaned: ['twake-load-1k-old'] })
  deepEqual(report.cleanup, { cleaned: [], remaining: [] })
  equal(report.versions.stack, '1.6.test')
  equal(report.monitoring.dev, 'unknown')
  const envelope = validateEnvelope(report.result)
  deepEqual([envelope?.producer, envelope?.outcome, envelope?.correctness, envelope?.capacity], ['shared-drive-ramp', 0,
    'not_verified', { kind: 'lower_bound', lower: 16, upper_exclusive: null, unit: 'activity level', verified: true,
      stop: 'every level passed, up to 16 (activity level)' }])
  deepEqual(classifyStep({ name: 'ramp', kind: 'shared-drive-ramp', id: 'x', startedAt: '', endedAt: '', exitCode: 0,
    producerExitCode: code }, report), { code: 0, reason: '' })
  // One k6 run per level, each under the ramp's ID in k6 metrics.
  const runs = f.docker().filter(c => c.args?.[1] === 'run')
  deepEqual(runs.map(c => c.args.find((a: string) => a.startsWith('CAPACITY_START='))), [1, 2, 4, 8, 16].map(l => `CAPACITY_START=${l}`))
  const run = runs[0]
  ok(run.args.includes('SHARED_DRIVE_ID=drive-1') && run.args.includes('SHARED_DRIVE_ROOT_ID=drive-root-1'))
  ok(run.args.includes(`BASE_URL=https://${fqdn}`))
  equal(run.token, 'instance-token')

  // Earlier ramp data in the drive is recovered, and the drive is reused.
  f.state.drive.set('older', 'twake-load-sd-older-1')
  equal((await f.run()).code, 0)
  equal(f.state.drives.length, 1)
  deepEqual(f.report('shared-drive-ramp').recovery.cleaned, ['twake-load-sd-older-1'])
  deepEqual([...f.state.drive.values()], [])
})

test('undeletable ramp data leaves the run inconclusive, with the levels as evidence', async t => {
  const f = await fixture(t)
  f.state.stuck = '-leftover'
  const { code, output } = await f.run({ FAKE_K6: 'leftover' })
  equal(code, 2, output)
  const report = f.report('shared-drive-ramp')
  equal(report.status, 'infrastructure_failure')
  equal(report.result_bound, 'inconclusive')
  equal(report.max_sustained_level, 0)
  match(report.failure_reason, /level 1: test data remains/)
  deepEqual(report.cleanup.remaining.map((i: { name: string }) => i.name), [`twake-load-sd-${f.id}-l1-leftover`])
  // The ramp stopped at the level it could not verify.
  equal(f.docker().filter(c => c.args?.[1] === 'run').length, 1)
})

test('the ramp stops at its first failing level, or before a level beyond its budget', async t => {
  const f = await fixture(t)
  equal((await f.run({ FAKE_RAMP_FAIL: '8' })).code, 0)
  let report = f.report('shared-drive-ramp')
  deepEqual([report.result_bound, report.max_sustained_level, report.first_failed_level], ['bracket', 4, 8])
  deepEqual(report.levels.map((l: any) => [l.level, l.status]), [[1, 'passed'], [2, 'passed'], [4, 'passed'], [8, 'failed']])
  equal(report.result.capacity.stop, 'level 8 (activity level): upload p95 3000 ms exceeds 2000 ms')
  // Level 8 runs 48 VUs, beyond 30.
  equal((await f.run({ MAX_VUS: '30' })).code, 0)
  report = f.report('shared-drive-ramp')
  deepEqual([report.result_bound, report.max_sustained_level, report.budget_reached], ['lower_bound', 4, 'MAX_VUS'])
  equal(report.result.capacity.stop, 'MAX_VUS reached after level 4 (activity level)')
})

test('a first failing level exits 1; a missing report or an interruption exits 2', async t => {
  const f = await fixture(t)
  equal((await f.run({ FAKE_RAMP_FAIL: '1' })).code, 1)
  equal((await f.run({ FAKE_K6: 'no-report' })).code, 2)
  match(f.report('shared-drive-ramp').failure_reason, /k6 wrote no ramp report/)
  const run = f.start({ ...budgets, FAKE_K6: 'hang' })
  while (!f.docker().includes('running')) await new Promise(resolve => setTimeout(resolve, 50))
  run.child.kill('SIGTERM')
  const { code, output } = await run.done
  equal(code, 2, output)
  ok(f.docker().includes('k6 stopped'))
  match(f.report('shared-drive-ramp').failure_reason, /interrupted by SIGTERM/)
})

test('a completed ramp report cannot hide an unexpected k6 exit', async t => {
  for (const [exit, expected] of [[99, 0], [108, 2]] as const) await t.test(`k6 exit ${exit}`, async t => {
    const f = await fixture(t)
    const { code, output } = await f.run({ FAKE_RAMP_EXIT: String(exit), FAKE_RAMP_FAIL: '8' })
    equal(code, expected, output)
    const report = f.report('shared-drive-ramp')
    equal(report.k6.exit_code, exit)
    if (expected === 2) {
      equal(report.status, 'infrastructure_failure')
      match(report.failure_reason, /k6 exited 108/)
    }
  })
})

test('member actors play the ramp roles, and a proven access failure outweighs the measured level', async t => {
  const f = await fixture(t)
  const actors = await fakeActors(t)
  const env = { ...f.local, ...actors.env }
  let { code, output } = await f.run(env)
  equal(code, 0, output)
  let report = f.report('shared-drive-ramp')
  deepEqual(report.actors.placement.map((p: any) => p.path), ['owner', 'same-stack', 'same-stack'])
  deepEqual(report.actors.revocation, { writer_read: 403, writer_write: 403, reader_read: 200 })
  deepEqual(report.actors.cleanup.retained, [])
  const run = f.docker().find(c => c.args?.[1] === 'run')
  ok(run.args.includes(`BASE_URL=http://${org}`) && run.args.includes(`WRITER_BASE_URL=http://${writer}`))
  ok(run.args.includes('SHARED_DRIVE_ID=sid') && run.args.some((a: string) => a.startsWith('ACTOR_HOSTS=seed-')))
  deepEqual([run.token, run.writer], [`token-${org}`, `token-${writer}`])
  ok(!JSON.stringify(report).includes('token-'))

  // A fresh drive: the first run's groups and root are gone.
  actors.state.readOnlyWrites = true
  actors.state.root = 'live'
  actors.state.groups.length = 0
  ;({ code, output } = await f.run(env))
  equal(code, 1, output)
  report = f.report('shared-drive-ramp')
  match(report.failure_reason, /read-only member could write/)
  const envelope = validateEnvelope(report.result)
  deepEqual([envelope?.outcome, envelope?.correctness, envelope?.capacity?.verified], [1, 'failed', false])
  deepEqual(classifyStep({ name: 'ramp', kind: 'shared-drive-ramp', id: 'x', startedAt: '', endedAt: '', exitCode: 2,
    producerExitCode: code }, report), { code: 1, reason: envelope?.reason })
})

test('a ramp that runs out of time after preparing its actors still removes their fixtures', async t => {
  const f = await fixture(t)
  const actors = await fakeActors(t)
  // A clock so fast that preparing the actors always outlasts MAX_RUNTIME (2 ms are 200 s), so setup
  // stops before k6.
  const clock = join(f.dockerDir, 'clock.mjs')
  writeFileSync(clock, 'const real = Date.now, start = real()\nDate.now = () => start + (real() - start) * 100000\n')
  const { code, output } = await f.start({ ...budgets, ...f.local, ...actors.env, CAPACITY_MAX: '1',
    CAPACITY_HOLD: '5s', MAX_RUNTIME: '200s' }, ['--import', clock]).done
  equal(code, 2, output)
  match(output, /MAX_RUNTIME expired during ramp setup; its fixtures were removed/)
  deepEqual([actors.state.root, actors.state.docs.size], ['gone', 0])
  ok(!f.docker().some(c => c.args?.[1] === 'run'))
})

test('a local ramp cleans only its own data in the drive and never stops other runs', async t => {
  const f = await fixture(t)
  f.state.host = 'local.test:8080'
  f.state.drives.push({ id: 'drive-local', root: 'drive-root-local' })
  f.state.drive.set('other', 'twake-load-sd-other-run-1')
  f.state.drive.set('mine', `twake-load-sd-${f.id}-1`)
  const { code, output } = await f.run({ ...f.local, MAX_VUS: '', MAX_UPLOAD_BYTES: '', MAX_RUNTIME: '' })
  equal(code, 0, output)
  deepEqual([...f.state.drive.values()], ['twake-load-sd-other-run-1'])
  const report = f.report('shared-drive-ramp')
  equal(report.recovery, null)
  deepEqual(report.cleanup, { cleaned: [`twake-load-sd-${f.id}-1`], remaining: [] })
  // No recovery: the only k6 inventory checks each level's own container after its k6.
  deepEqual(f.docker().map(c => c.args.slice(0, 2).join(' ')),
    [...[1, 2, 4, 8, 16].flatMap(() => ['compose up', 'compose run', 'ps -q']), 'image inspect'])
})
