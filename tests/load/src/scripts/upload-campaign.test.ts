import { deepEqual, doesNotMatch, equal, match, ok } from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { chmodSync, existsSync, mkdtempSync, readdirSync, readFileSync, readlinkSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

import { getBudgetResult } from './upload-campaign.ts'

const cwd = fileURLToPath(new URL('../../', import.meta.url))
const script = fileURLToPath(new URL('./upload-campaign.ts', import.meta.url))
const fqdn = 'seedtest01.dev.example.test'
const uuid = 'uuid-seedtest01'

// Stands in for docker: logs each call, answers `compose run` like k6 would, per FAKE_K6.
const fakeDocker = `#!/usr/bin/env node
const fs = require('fs'), http = require('http')
const args = process.argv.slice(2)
const log = line => fs.appendFileSync(process.env.FAKE_DOCKER_LOG, JSON.stringify(line) + '\\n')
log({ args, token: process.env.COZY_ACCESS_TOKEN || null })
if (args[0] === 'image') process.exit(1)
if (args[0] === 'info') console.log(String(8 * 2 ** 30))
// A hanging k6 is container c1, with its run's environment, until docker stop ends it.
const pidFile = process.env.FAKE_DOCKER_LOG + '.pid'
const container = () => JSON.parse(fs.readFileSync(pidFile, 'utf8'))
if (args[0] === 'ps') console.log(fs.existsSync(pidFile) ? 'c1' : '')
if (args[0] === 'inspect') console.log(JSON.stringify(container().env))
if (args[0] === 'stop') process.kill(container().pid, 'SIGUSR2')
if (args[1] !== 'run') process.exit(0)
const env = Object.fromEntries(args.flatMap((a, i) => args[i - 1] === '-e' ? [a.split(/=(.*)/s).slice(0, 2)] : []))
const mode = process.env.FAKE_K6
if (mode === 'hang') {
  // Signalling docker compose leaves the container running; only docker stop (SIGUSR2 here) ends it.
  fs.writeFileSync(pidFile, JSON.stringify({ pid: process.pid,
    env: ['EXECUTION_ID=' + env.EXECUTION_ID, 'BASE_URL=' + env.BASE_URL] }))
  process.on('SIGTERM', () => {})
  process.on('SIGUSR2', () => { fs.rmSync(pidFile); log('k6 stopped'); process.exit(105) })
  log('running')
  setInterval(() => {}, 1000)
} else {
  const mismatch = mode.includes('mismatch') && Number(env.UPLOAD_VUS) >= Number(process.env.FAKE_MISMATCH_VUS || 1)
  const finish = () => {
    const name = args.find(a => a.startsWith('--summary-export=')).split('/').pop()
    fs.writeFileSync('data/results/' + name, JSON.stringify({ metrics: {
      'http_req_duration{operation:file-upload}': { 'p(95)': 12 },
      'upload_success{operation:file-upload}': { value: 1 },
      iterations: { count: env.UPLOAD_VUS * env.ITERATIONS_PER_VU },
      cleanup_success: { value: mode.includes('leftover') ? 0 : 1 },
      upload_consistency: { value: mismatch ? 0 : 1 },
    } }))
    process.exit(mismatch ? 108 : 0)
  }
  // Named like the scenario's own test directory, which only teardown deletes.
  const name = 'twake-load-' + env.FILE_SIZE.toLowerCase() + '-' + env.EXECUTION_ID + '-leftover'
  if (!mode.includes('leftover')) finish()
  else http.request(process.env.FAKE_STACK_URL + '/files/io.cozy.files.root-dir?Type=directory&Name=' + name, {
    method: 'POST', headers: { Host: new URL(env.BASE_URL).host, Authorization: 'Bearer ' + process.env.COZY_ACCESS_TOKEN },
  }, res => { res.resume(); res.on('end', finish) }).end()
}
`

// One server plays the Cloudery token route and the instance (by Host); the root lists one child per page.
async function fixture(t: TestContext) {
  const state = {
    root: new Map<string, string>([['docs', 'Documents']]), trash: new Map<string, string>(),
    requests: [] as string[], stuck: '', uuid, host: fqdn,
  }
  const stuck = (name: string) => state.stuck !== '' && name.endsWith(state.stuck)
  const server = createServer(async (req, res) => {
    for await (const _ of req);
    const url = new URL(req.url!, 'http://x')
    const route = `${req.method} ${url.pathname}`
    state.requests.push(`${req.headers.host!.replace(/^127\.0\.0\.1:\d+$/, '127.0.0.1')} ${route}`)
    const send = (status: number, value?: unknown) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(value === undefined ? '' : JSON.stringify(value))
    }
    if (route === `POST /api/public/instances/${fqdn}/token`) {
      return req.headers.authorization === 'Bearer public-secret' ? send(200, { token: 'instance-token' }) : send(401)
    }
    if (req.headers.host !== state.host || req.headers.authorization !== 'Bearer instance-token') return send(401)
    const list = (items: Map<string, string>) => [...items].map(([id, name]) => ({ id, attributes: { name } }))
    const [, , first, second] = url.pathname.split('/')
    if (route === 'GET /version') return send(200, { version: '1.6.test' })
    if (route === 'GET /settings/instance') return send(200, { data: { attributes: { uuid: state.uuid } } })
    if (route === 'GET /files/io.cozy.files.root-dir') {
      const cursor = Number(url.searchParams.get('page[cursor]') ?? 0)
      return send(200, { data: { id: 'io.cozy.files.root-dir' }, included: list(state.root).slice(cursor, cursor + 1),
        links: cursor + 1 < state.root.size ? { next: `/files/io.cozy.files.root-dir?page[cursor]=${cursor + 1}` } : {} })
    }
    if (route === 'GET /files/trash') return send(200, { data: list(state.trash) })
    if (route === 'POST /files/io.cozy.files.root-dir') {
      state.root.set(`dir-${state.requests.length}`, url.searchParams.get('Name')!)
      return send(201, {})
    }
    if (req.method === 'DELETE' && first === 'trash' && state.trash.has(second!) && !stuck(state.trash.get(second!)!)) {
      state.trash.delete(second!)
      return send(204)
    }
    if (req.method === 'DELETE' && state.root.has(first!)) {
      state.trash.set(first!, state.root.get(first!)!)
      state.root.delete(first!)
      return send(200, {})
    }
    send(500)
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => new Promise<void>(resolve => {
    server.closeAllConnections()
    server.close(() => resolve())
  }))
  const origin = `http://127.0.0.1:${(server.address() as { port: number }).port}`
  const dir = mkdtempSync(join(tmpdir(), 'upload-campaign-'))
  const id = `upload-campaign-test-${process.pid}-${Date.now()}`
  t.after(() => {
    rmSync(dir, { recursive: true, force: true })
    const results = join(cwd, 'data/results')
    for (const name of existsSync(results) ? readdirSync(results) : []) {
      if (name.startsWith(id)) rmSync(join(results, name), { force: true })
    }
  })
  writeFileSync(join(dir, 'docker'), fakeDocker)
  chmodSync(join(dir, 'docker'), 0o700)
  writeFileSync(join(dir, 'token.json'), JSON.stringify({ id: 'partner-id', public: 'public-secret', manager_url: origin }))
  writeFileSync(join(dir, 'manifest.json'), JSON.stringify({ instances: [{ fqdn, uuid, role: 'member' }] }))
  const dockerLog = join(dir, 'docker.log')

  const settings = /^(SEED_|DEV_|MAX_|MODE$|LOAD_TARGET$|BASE_URL$|STACK_URL$|COZY_ACCESS_TOKEN$|CAPACITY_RUN_ID$|EXECUTION_ID$|UPLOAD_TARGET$|VUS$|ITERATIONS_PER_VU$|FILE_SIZE$|BASELINE_ITERATIONS$|CONFIRMATION_ITERATIONS$|UPLOAD_TIMEOUT$)/
  const inherited = Object.fromEntries(Object.entries(process.env).filter(([key]) => !settings.test(key)))
  const baseEnv: Record<string, string> = {
    ...inherited, PATH: `${dir}:${process.env.PATH}`, FAKE_DOCKER_LOG: dockerLog, FAKE_K6: 'pass', FAKE_STACK_URL: origin,
    MODE: 'regression', LOAD_TARGET: 'dev', MAX_VUS: '2', MAX_UPLOAD_BYTES: '10M', MAX_RUNTIME: '10m',
    SEED_CLOUDERY_TOKEN_FILE: join(dir, 'token.json'), SEED_CLOUDERY_MANIFEST: join(dir, 'manifest.json'),
    DEV_INSTANCE: fqdn, DEV_STACK_URL: origin, EXECUTION_ID: id, CAPACITY_RUN_ID: id,
  }
  function start(env: Record<string, string> = {}, preload: string[] = []) {
    const child = spawn(process.execPath, ['--import', 'tsx', ...preload, script], {
      cwd, env: { ...baseEnv, ...env }, timeout: 60_000,
    })
    let output = ''
    child.stdout.on('data', chunk => { output += chunk })
    child.stderr.on('data', chunk => { output += chunk })
    const done = new Promise<{ code: number | null, output: string }>(resolve =>
      child.on('close', code => resolve({ code, output })))
    return { child, done }
  }
  const docker = (): any[] => existsSync(dockerLog)
    ? readFileSync(dockerLog, 'utf8').trim().split('\n').map(line => JSON.parse(line)) : []
  const report = (mode = 'regression') =>
    JSON.parse(readFileSync(join(cwd, `data/results/${id}-upload-${mode}.json`), 'utf8'))
  const local = { LOAD_TARGET: 'local', BASE_URL: 'http://local.test:8080', STACK_URL: origin, COZY_ACCESS_TOKEN: 'instance-token' }
  return { state, docker, report, run: (env?: Record<string, string>) => start(env).done, start, env: baseEnv, dockerDir: dir, id, local }
}

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
  const localEnv = join(mkdtempSync(join(tmpdir(), 'upload-campaign-env-')), 'local.mk')
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
  match(report.monitoring.k6, /var-campaignid=upload-campaign-test-/)

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
  const clock = join(mkdtempSync(join(tmpdir(), 'upload-campaign-clock-')), 'clock.mjs')
  writeFileSync(clock, 'const real = Date.now, start = real()\nDate.now = () => start + (real() - start) * 10_000\n')
  t.after(() => rmSync(clock, { force: true }))
  const { code, output } = await f.start({}, ['--import', clock]).done
  equal(code, 2, output)
  match(output, /cleanup exceeded its 5 minute budget/)
  ok(!f.docker().some(c => c.args?.[1] === 'run'))
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
  let report = f.report('capacity')
  deepEqual(phases(report), [['baseline', 1], ['discovery', 2], ['confirmation', 2]])
  equal(report.maximum_concurrent_uploads, 2)
  equal(report.result_bound, 'lower_bound')
  equal(report.budget_reached, 'concurrency')
  ok(report.attempts.every((a: any) => a.cleanup !== null), 'every point verifies its own cleanup')

  // Four 1 KiB uploads per confirmation point allow two workers out of eight.
  equal((await f.run({ ...search, MAX_VUS: '8', MAX_UPLOAD_BYTES: '4K' })).code, 0)
  report = f.report('capacity')
  equal(report.configured_max_vus, 2)
  equal(report.maximum_concurrent_uploads, 2)
  equal(report.budget_reached, 'upload bytes')
})

test('a content mismatch ends a capacity search at once', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ MODE: 'capacity', MAX_VUS: '8', FAKE_K6: 'mismatch', FAKE_MISMATCH_VUS: '2' })
  equal(code, 1, output)
  const report = f.report('capacity')
  equal(report.status, 'correctness_failure')
  deepEqual(phases(report), [['baseline', 1], ['discovery', 2]])
})

test('the runtime budget stops a running point without claiming a capacity', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ MODE: 'capacity', MAX_RUNTIME: '2s', FAKE_K6: 'hang' })
  equal(code, 2, output)
  const report = f.report('capacity')
  match(report.failure_reason, /MAX_RUNTIME budget ran out before a level was confirmed/)
  equal(report.attempts[0].result, 'stopped')
  ok(f.docker().includes('k6 stopped'))
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
  for (const target of ['dev-upload', 'dev-capacity']) {
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
