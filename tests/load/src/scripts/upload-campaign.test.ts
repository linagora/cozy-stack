import { deepEqual, doesNotMatch, equal, match, ok } from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { chmodSync, existsSync, mkdtempSync, readdirSync, readFileSync, readlinkSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

import { getBudgetResult } from './upload-campaign.ts'
import { cwd, fakeTarget, fqdn } from './upload/fake-target.ts'

const script = fileURLToPath(new URL('./upload-campaign.ts', import.meta.url))
const fixture = (t: TestContext) => fakeTarget(t, script)

const phases = (report: any) => report.attempts.map((a: any) => [a.phase, a.vus])

test('rejects work beyond its budgets or outside the manifest before any request', async t => {
  const f = await fixture(t)
  for (const env of [{ VUS: '3' }, { VUS: '2', ITERATIONS_PER_VU: '6', FILE_SIZE: '1M' }, { MAX_RUNTIME: '' },
    { MODE: 'capacity', FILE_SIZE: '1M', MAX_UPLOAD_BYTES: '1M' }, { DEV_INSTANCE: 'other01.dev.example.test' }]) {
    const { code, output } = await f.run(env)
    equal(code, 2, output)
  }
  f.state.uuid = 'recreated'
  const { code, output } = await f.run()
  equal(code, 2, output)
  match(output, /no longer the fixture instance/)
  deepEqual(f.state.requests, [`127.0.0.1 POST /api/public/instances/${fqdn}/token`, `${fqdn} GET /settings/instance`])
  deepEqual(f.docker(), [])
})

test('reconciles earlier data, runs only the k6 upload and records the run', async t => {
  const f = await fixture(t)
  f.state.root.set('old', 'twake-load-1k-old')
  f.state.trash.set('older', 'twake-load-1m-older')
  // Read like the harness .env, where cozy-prepare saves a local instance token.
  const localEnv = join(mkdtempSync(join(tmpdir(), 'load-runner-env-')), 'local.mk')
  writeFileSync(localEnv, 'COZY_ACCESS_TOKEN = local-instance-token\n')
  t.after(() => rmSync(localEnv, { force: true }))
  const { code, output } = await f.run({ VUS: '2', ITERATIONS_PER_VU: '5', FILE_SIZE: '1M', MAKEFILES: localEnv })
  equal(code, 0, output)
  deepEqual([...f.state.root.values()], ['Documents'])
  deepEqual([...f.state.trash.values()], [])

  const report = f.report()
  equal(report.outcome, 0)
  equal(report.status, 'completed')
  deepEqual(report.recovery, { k6_stopped: [], cleaned: ['twake-load-1k-old', 'twake-load-1m-older'] })
  deepEqual(report.budgets, { max_vus: 2, max_upload_bytes: 10 << 20, max_runtime: '10m', upload_timeout: null })
  deepEqual(phases(report), [['regression', 2]])
  equal(report.attempts[0].iterations_per_vu, 5)
  deepEqual(report.attempts[0].cleanup, { cleaned: [], remaining: [] })
  equal(report.file_size, '1M')
  equal(report.versions.stack, '1.6.test')
  equal(report.versions.node, process.version)
  equal(report.versions.k6_image, 'unknown')
  equal(report.monitoring.dev, 'unknown')
  match(report.monitoring.k6, /var-campaignid=load-runner-test-/)

  // No local Stack: Compose only starts the observability services and runs k6.
  const calls = f.docker()
  deepEqual(calls.map(c => c.args.slice(0, 2)),
    [['info', '--format'], ['ps', '-q'], ['compose', 'up'], ['compose', 'run'], ['ps', '-q'], ['image', 'inspect']])
  for (const call of calls) doesNotMatch(call.args.join(' '), /cozy|couchdb|instance-token/)
  ok(calls[3].args.includes(`BASE_URL=https://${fqdn}`))
  equal(calls[3].token, 'instance-token')
  doesNotMatch(JSON.stringify(report), /instance-token|public-secret/)
  // The instance only sees its own files, its settings and the version.
  for (const line of f.state.requests.filter(line => !line.endsWith('/token'))) {
    match(line, new RegExp(`^${fqdn} (GET|DELETE) /(files/|settings/instance$|version$)`))
  }
})

test('a content mismatch is a correctness failure', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ FAKE_K6: 'mismatch' })
  equal(code, 1, output)
  equal(f.report().status, 'correctness_failure')
  match(f.report().failure_reason, /did not match/)
})

test('a content mismatch stays a correctness failure when cleanup also fails', async t => {
  const f = await fixture(t)
  f.state.stuck = '-leftover'
  const { code, output } = await f.run({ FAKE_K6: 'mismatch-leftover' })
  equal(code, 1, output)
  match(f.report().failure_reason, /cleanup failed; uploaded content did not match.*test data remains/)
})

test('cleanup listing that outlasts its budget stops the run before any upload', async t => {
  const f = await fixture(t)
  for (let i = 0; i < 300; i++) f.state.root.set(`user-${i}`, `user file ${i}`)
  // Ten thousand times faster: the 5-minute cleanup budget lasts 30 ms of listing.
  const clock = join(mkdtempSync(join(tmpdir(), 'load-runner-clock-')), 'clock.mjs')
  writeFileSync(clock, 'const real = Date.now, start = real()\nDate.now = () => start + (real() - start) * 10_000\n')
  t.after(() => rmSync(clock, { force: true }))
  const { code, output } = await f.start({}, ['--import', clock]).done
  equal(code, 2, output)
  match(output, /cleanup exceeded its 5 minute budget/)
  ok(!f.docker().some(c => c.args?.[1] === 'run'))
})

test('an unverifiable Docker inventory stops recovery before cleanup or load', async t => {
  const f = await fixture(t)
  f.state.root.set('old', 'twake-load-1k-old')
  equal((await f.run({ FAKE_DOCKER_FAIL: 'ps' })).code, 2)
  // A listed container whose environment cannot be read may still be loading DEV.
  writeFileSync(`${f.dockerLog}.pid`, JSON.stringify({ pid: 0, env: [] }))
  const { code, output } = await f.run({ FAKE_DOCKER_FAIL: 'inspect' })
  equal(code, 2, output)
  match(output, /docker inspect c1 failed/)
  deepEqual([...f.state.root.values()], ['Documents', 'twake-load-1k-old'])
  ok(!f.docker().some(c => c.args?.[1] === 'run'))
  // A container that exited since the listing no longer runs k6.
  equal((await f.run({ FAKE_DOCKER_FAIL: 'inspect-gone' })).code, 0)
  // Without the inventory after k6, a run cannot show that its k6 stopped.
  equal((await f.run({ LOAD_TARGET: 'local', BASE_URL: 'http://mock', FAKE_DOCKER_FAIL: 'ps' })).code, 2)
  match(f.report().failure_reason, /k6 containers could not be verified: docker ps failed/)
})

test('a k6 container left by a killed coordinator is stopped before cleanup', async t => {
  const f = await fixture(t)
  const killed = f.start({ FAKE_K6: 'hang' })
  while (!f.docker().includes('running')) await new Promise(resolve => setTimeout(resolve, 50))
  killed.child.kill('SIGKILL')
  await killed.done
  const { code, output } = await f.run()
  equal(code, 0, output)
  const log = f.docker().map(c => typeof c === 'string' ? c : c.args.join(' '))
  const stoppedAt = log.indexOf('k6 stopped')
  ok(stoppedAt > 0 && log.indexOf('stop --time 90 c1') < stoppedAt, output)
  ok(log.map(line => line.startsWith('compose run')).lastIndexOf(true) > stoppedAt, output)
  deepEqual(f.report().recovery.k6_stopped, ['c1'])
})

test('test data that cannot be deleted leaves the run inconclusive and identified', async t => {
  const f = await fixture(t)
  f.state.stuck = '-leftover'
  const { code, output } = await f.run({ FAKE_K6: 'leftover' })
  equal(code, 2, output)
  const report = f.report()
  match(report.failure_reason, /cleanup failed.*test data remains/)
  deepEqual(report.attempts[0].cleanup.remaining.map((i: any) => i.name),
    [`twake-load-1k-${f.id}-regression-v1-a1-leftover`])
})

test('an interruption stops k6, still cleans up and exits 2', async t => {
  const f = await fixture(t)
  const run = f.start({ FAKE_K6: 'hang' })
  while (!f.docker().includes('running')) await new Promise(resolve => setTimeout(resolve, 50))
  run.child.kill('SIGTERM')
  const { code, output } = await run.done
  equal(code, 2, output)
  ok(f.docker().some(c => c.args?.join(' ') === 'stop --time 90 c1'))
  ok(f.docker().includes('k6 stopped'))
  match(f.report().failure_reason, /interrupted by SIGTERM/)
  deepEqual(f.report().attempts[0].cleanup.remaining, [])
})

test('a capacity search on DEV stays within its concurrency and upload-bytes budgets', async t => {
  const f = await fixture(t)
  const search = { MODE: 'capacity', BASELINE_ITERATIONS: '2', CONFIRMATION_ITERATIONS: '2' }
  const { code, output } = await f.run(search)
  equal(code, 0, output)
  let report = f.report('upload-capacity')
  deepEqual(phases(report), [['baseline', 1], ['discovery', 2], ['confirmation', 2]])
  equal(report.maximum_concurrent_uploads, 2)
  equal(report.result_bound, 'lower_bound')
  equal(report.budget_reached, 'concurrency')
  ok(report.attempts.every((a: any) => a.cleanup !== null), 'every point verifies its own cleanup')

  // Four 1 KiB uploads per confirmation point allow two workers out of eight.
  equal((await f.run({ ...search, MAX_VUS: '8', MAX_UPLOAD_BYTES: '4K' })).code, 0)
  report = f.report('upload-capacity')
  equal(report.configured_max_vus, 2)
  equal(report.maximum_concurrent_uploads, 2)
  equal(report.budget_reached, 'upload bytes')
})

test('a content mismatch ends a capacity search at once', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ MODE: 'capacity', MAX_VUS: '8', FAKE_K6: 'mismatch', FAKE_MISMATCH_VUS: '2' })
  equal(code, 1, output)
  const report = f.report('upload-capacity')
  equal(report.status, 'correctness_failure')
  deepEqual(phases(report), [['baseline', 1], ['discovery', 2]])
})

test('the runtime budget stops a running point without claiming a capacity', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ MODE: 'capacity', MAX_RUNTIME: '2s', FAKE_K6: 'hang' })
  equal(code, 2, output)
  const report = f.report('upload-capacity')
  match(report.failure_reason, /MAX_RUNTIME budget ran out before a level was confirmed/)
  equal(report.attempts[0].result, 'stopped')
  ok(f.docker().includes('k6 stopped'))
})

test('a runtime stop keeps its lower bound as evidence but not an unverified cleanup', async t => {
  const f = await fixture(t)
  f.state.stuck = '-leftover'
  const search = { MODE: 'capacity', MAX_VUS: '4', MAX_RUNTIME: '20s', BASELINE_ITERATIONS: '2', CONFIRMATION_ITERATIONS: '2' }
  const plan = (stopped: string) => JSON.stringify({ 'confirmation:4': 'fail', 'confirmation:3': stopped })
  // DEV keeps undeletable data; the mock's k6 is killed before its own teardown reports.
  for (const [env, reason] of [[{ FAKE_PLAN: plan('hang-leftover') }, /test data remains/],
    [{ FAKE_PLAN: plan('hang'), LOAD_TARGET: 'local', BASE_URL: 'http://mock' }, /cleanup could not be verified/]] as const) {
    const { code, output } = await f.run({ ...search, ...env })
    equal(code, 2, output)
    const report = f.report('upload-capacity')
    deepEqual(phases(report), [['baseline', 1], ['discovery', 2], ['discovery', 4], ['confirmation', 4],
      ['confirmation', 2], ['confirmation', 3]])
    equal(report.attempts[5].result, 'stopped')
    equal(report.status, 'infrastructure_failure')
    equal(report.maximum_concurrent_uploads, 2)
    equal(report.result_bound, 'lower_bound')
    equal(report.budget_reached, 'runtime')
    match(report.failure_reason, reason)
  }
})

test('a slow token cannot start k6 after the runtime budget', async t => {
  const f = await fixture(t)
  f.state.tokenDelayMs = 1200
  const { code, output } = await f.run({ MAX_RUNTIME: '1s' })
  equal(code, 2, output)
  match(f.report().failure_reason, /MAX_RUNTIME/)
  ok(!f.docker().some(c => c.args?.[1] === 'run'))
})

test('a runtime stop keeps only confirmed levels, as a lower bound', () => {
  const attempt = (phase: string, vus: number, passed: boolean) => ({ phase: phase as 'confirmation', vus, passed, p95_ms: 9 })
  deepEqual(getBudgetResult([attempt('baseline', 1, true), attempt('discovery', 8, true), attempt('confirmation', 8, false),
    attempt('confirmation', 4, true)], null), { baselineP95Ms: 9, bound: 'lower_bound', maximumVus: 4, p95LimitMs: null, status: 'completed' })
  equal(getBudgetResult([attempt('baseline', 1, true), attempt('discovery', 2, true)], null).status, 'infrastructure_failure')
})

test('a local run cleans only its own data and never stops other runs', async t => {
  const f = await fixture(t)
  f.state.host = 'local.test:8080'
  f.state.root.set('other', 'twake-load-1k-other-run-1')
  f.state.root.set('mine', `twake-load-1k-${f.id}-regression-v1-a1-1`)
  const { code, output } = await f.run(f.local)
  equal(code, 0, output)
  deepEqual([...f.state.root.values()], ['Documents', 'twake-load-1k-other-run-1'])
  const report = f.report()
  equal(report.load_target, 'local')
  equal(report.recovery, null)
  deepEqual(report.attempts[0].cleanup, { cleaned: [`twake-load-1k-${f.id}-regression-v1-a1-1`], remaining: [] })
  equal(report.monitoring.dev, undefined)
  deepEqual(f.docker().map(c => c.args.slice(0, 2)),
    [['info', '--format'], ['compose', 'up'], ['compose', 'run'], ['ps', '-q'], ['image', 'inspect']])
  equal(f.docker()[2].token, 'instance-token')
})

test('a mock run leaves cleanup to k6', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ LOAD_TARGET: 'local', BASE_URL: 'http://mock' })
  equal(code, 0, output)
  equal(f.report().attempts[0].cleanup, null)
  deepEqual(f.state.requests, [])
})

test('make dev-upload refuses a second run while the shared DEV lock is held', async t => {
  if (spawnSync('sh', ['-c', 'command -v flock']).status !== 0) return t.skip('flock is not installed')
  const dir = mkdtempSync(join(tmpdir(), 'dev-lock-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const lock = join(dir, 'shared-dev.lock')
  const holder = spawn('bash', ['campaigns/lib/shared-dev-lock.sh', lock, 'sh', '-c', 'echo held; sleep 30'], { cwd })
  t.after(() => holder.kill())
  await new Promise(resolve => holder.stdout.once('data', resolve))
  const direct = spawnSync('bash', ['campaigns/lib/shared-dev-lock.sh', lock, 'touch', join(dir, 'ran')], { cwd })
  equal(direct.status, 2)
  writeFileSync(join(dir, 'node'), `#!/bin/sh\ntouch ${join(dir, 'ran')}\n`)
  chmodSync(join(dir, 'node'), 0o700)
  for (const target of ['dev-upload', 'dev-capacity', 'dev-shared-drive-ramp', 'dev-org-drive-fanout']) {
    const result = spawnSync('make', ['-s', '-o', 'node_modules/.load-ready', `DEV_LOCK_FILE=${lock}`, target],
      { cwd, encoding: 'utf8', env: { ...process.env, PATH: `${dir}:${process.env.PATH}` } })
    equal(result.status, 2, result.stderr)
    match(result.stderr, /Shared DEV is busy/)
  }
  ok(!existsSync(join(dir, 'ran')))
})

test('the process holding the shared DEV lock is the runner itself', async t => {
  if (!existsSync('/proc/self/fd')) return t.skip('needs /proc')
  if (spawnSync('sh', ['-c', 'command -v flock']).status !== 0) return t.skip('flock is not installed')
  const f = await fixture(t)
  const lock = join(mkdtempSync(join(tmpdir(), 'dev-lock-')), 'shared-dev.lock')
  const dir = f.dockerDir
  const make = spawn('make', ['-s', '-o', 'node_modules/.load-ready', `DEV_LOCK_FILE=${lock}`, 'dev-upload'],
    { cwd, env: { ...f.env, FAKE_K6: 'hang' }, stdio: 'ignore' })
  t.after(() => {
    make.kill('SIGKILL')
    // Ends the fake k6 the killed runner left behind.
    spawnSync(join(dir, 'docker'), ['stop', 'c1'], { env: f.env })
  })
  while (!f.docker().includes('running')) await new Promise(resolve => setTimeout(resolve, 50))
  const pids = () => readdirSync('/proc').filter(pid => /^\d+$/.test(pid))
  const holders = pids().filter(pid => {
    try { return readlinkSync(`/proc/${pid}/fd/9`) === lock } catch { return false }
  })
  equal(holders.length, 1)
  process.kill(Number(holders[0]), 'SIGKILL')
  await new Promise(resolve => setTimeout(resolve, 500))
  const runners = pids().filter(pid => {
    try { return readFileSync(`/proc/${pid}/cmdline`, 'utf8').includes('upload-campaign.ts') } catch { return false }
  })
  deepEqual(runners, [])
})
