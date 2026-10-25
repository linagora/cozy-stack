import { deepEqual, doesNotMatch, equal, match, ok } from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import { chmodSync, existsSync, mkdtempSync, readdirSync, readFileSync, readlinkSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const cwd = fileURLToPath(new URL('../../', import.meta.url))
const script = fileURLToPath(new URL('./dev-upload.ts', import.meta.url))
const fqdn = 'seedtest01.dev.example.test'
const uuid = 'uuid-seedtest01'

// Stands in for docker: logs each call, answers `compose run` like k6 would, per FAKE_K6.
const fakeDocker = `#!/usr/bin/env node
const fs = require('fs'), http = require('http')
const args = process.argv.slice(2)
const log = line => fs.appendFileSync(process.env.FAKE_DOCKER_LOG, JSON.stringify(line) + '\\n')
log({ args, token: process.env.COZY_ACCESS_TOKEN || null })
if (args[0] === 'image') process.exit(1)
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
  const finish = () => {
    const name = args.find(a => a.startsWith('--summary-export=')).split('/').pop()
    fs.writeFileSync('data/results/' + name, JSON.stringify({ metrics: {
      'http_req_duration{operation:file-upload}': { 'p(95)': 12 },
      'upload_success{operation:file-upload}': { value: 1 },
      iterations: { count: env.UPLOAD_VUS * env.ITERATIONS_PER_VU },
      cleanup_success: { value: mode.includes('leftover') ? 0 : 1 },
      upload_consistency: { value: mode.includes('mismatch') ? 0 : 1 },
    } }))
    process.exit(mode.includes('mismatch') ? 108 : 0)
  }
  if (!mode.includes('leftover')) finish()
  else http.request(process.env.DEV_STACK_URL + '/files/io.cozy.files.root-dir?Type=directory&Name=twake-load-leftover', {
    method: 'POST', headers: { Host: process.env.DEV_INSTANCE, Authorization: 'Bearer ' + process.env.COZY_ACCESS_TOKEN },
  }, res => { res.resume(); res.on('end', finish) }).end()
}
`

// One server plays the Cloudery token route and the instance (by Host); the root lists one child per page.
async function fixture(t: TestContext) {
  const state = {
    root: new Map<string, string>([['docs', 'Documents']]), trash: new Map<string, string>(),
    requests: [] as string[], stuck: '', uuid,
  }
  const server = createServer(async (req, res) => {
    for await (const _ of req);
    const url = new URL(req.url!, 'http://x')
    const route = `${req.method} ${url.pathname}`
    state.requests.push(`${req.headers.host!.replace(/:\d+$/, '')} ${route}`)
    const send = (status: number, value?: unknown) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(value === undefined ? '' : JSON.stringify(value))
    }
    if (route === `POST /api/public/instances/${fqdn}/token`) {
      return req.headers.authorization === 'Bearer public-secret' ? send(200, { token: 'instance-token' }) : send(401)
    }
    if (req.headers.host !== fqdn || req.headers.authorization !== 'Bearer instance-token') return send(401)
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
    if (req.method === 'DELETE' && first === 'trash' && state.trash.has(second!) && state.trash.get(second!) !== state.stuck) {
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
  const dir = mkdtempSync(join(tmpdir(), 'dev-upload-'))
  const executionId = `dev-upload-test-${process.pid}-${Date.now()}`
  t.after(() => {
    rmSync(dir, { recursive: true, force: true })
    for (const suffix of ['summary', 'dev-upload']) rmSync(join(cwd, `data/results/${executionId}-${suffix}.json`), { force: true })
  })
  writeFileSync(join(dir, 'docker'), fakeDocker)
  chmodSync(join(dir, 'docker'), 0o700)
  writeFileSync(join(dir, 'token.json'), JSON.stringify({ id: 'partner-id', public: 'public-secret', manager_url: origin }))
  writeFileSync(join(dir, 'manifest.json'), JSON.stringify({ instances: [{ fqdn, uuid, role: 'member' }] }))
  const dockerLog = join(dir, 'docker.log')

  const inherited = Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(SEED_|DEV_)/.test(key)))
  const baseEnv = {
    ...inherited, PATH: `${dir}:${process.env.PATH}`, FAKE_DOCKER_LOG: dockerLog, FAKE_K6: 'pass',
    SEED_CLOUDERY_TOKEN_FILE: join(dir, 'token.json'), SEED_CLOUDERY_MANIFEST: join(dir, 'manifest.json'),
    DEV_INSTANCE: fqdn, DEV_STACK_URL: origin, EXECUTION_ID: executionId,
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
  const report = () => JSON.parse(readFileSync(join(cwd, `data/results/${executionId}-dev-upload.json`), 'utf8'))
  return { state, docker, report, run: (env?: Record<string, string>) => start(env).done, start, env: baseEnv, dockerDir: dir }
}

test('rejects work beyond the shared DEV budget or outside the manifest before any request', async t => {
  const f = await fixture(t)
  for (const env of [{ VUS: '3' }, { ITERATIONS_PER_VU: '6' }, { FILE_SIZE: '10M' },
    { DEV_INSTANCE: 'other01.dev.example.test' }]) {
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
  const localEnv = join(mkdtempSync(join(tmpdir(), 'dev-upload-env-')), 'local.mk')
  writeFileSync(localEnv, 'COZY_ACCESS_TOKEN = local-instance-token\n')
  t.after(() => rmSync(localEnv, { force: true }))
  const { code, output } = await f.run({ VUS: '2', ITERATIONS_PER_VU: '5', FILE_SIZE: '1M', MAKEFILES: localEnv })
  equal(code, 0, output)
  deepEqual([...f.state.root.values()], ['Documents'])
  deepEqual([...f.state.trash.values()], [])

  const report = f.report()
  equal(report.outcome, 0)
  deepEqual(report.cleanup, { k6_stopped: [], before: ['twake-load-1k-old', 'twake-load-1m-older'], after: [], remaining: [] })
  deepEqual(report.parameters, { vus: 2, iterations_per_vu: 5, file_size: '1M' })
  equal(report.versions.stack, '1.6.test')
  equal(report.versions.node, process.version)
  equal(report.versions.k6_image, 'unknown')
  equal(report.monitoring.dev, 'unknown')
  match(report.monitoring.k6, /var-campaignid=dev-upload-test-/)

  // No local Stack: Compose only starts the observability services and runs k6.
  const calls = f.docker()
  deepEqual(calls.map(c => c.args.slice(0, 2)),
    [['ps', '-q'], ['compose', 'up'], ['compose', 'run'], ['ps', '-q'], ['image', 'inspect']])
  for (const call of calls) doesNotMatch(call.args.join(' '), /cozy|couchdb|instance-token/)
  ok(calls[2].args.includes(`BASE_URL=https://${fqdn}`))
  equal(calls[2].token, 'instance-token')
  doesNotMatch(JSON.stringify(report), /instance-token|public-secret/)
  // The instance only sees its own files, its settings and the version.
  for (const line of f.state.requests.slice(1)) match(line, new RegExp(`^${fqdn} (GET|DELETE) /(files/|settings/instance$|version$)`))
})

test('a content mismatch is a correctness failure', async t => {
  const f = await fixture(t)
  const { code, output } = await f.run({ FAKE_K6: 'mismatch' })
  equal(code, 1, output)
  match(f.report().reason, /did not match/)
})

test('a content mismatch stays a correctness failure when cleanup also fails', async t => {
  const f = await fixture(t)
  f.state.stuck = 'twake-load-leftover'
  const { code, output } = await f.run({ FAKE_K6: 'mismatch-leftover' })
  equal(code, 1, output)
  match(f.report().reason, /cleanup failed; uploaded content did not match.*test data remains/)
})

test('cleanup listing that outlasts its budget stops the run before any upload', async t => {
  const f = await fixture(t)
  for (let i = 0; i < 300; i++) f.state.root.set(`user-${i}`, `user file ${i}`)
  // Ten thousand times faster: the 5-minute cleanup budget lasts 30 ms of listing.
  const clock = join(mkdtempSync(join(tmpdir(), 'dev-upload-clock-')), 'clock.mjs')
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
  deepEqual(f.report().cleanup.k6_stopped, ['c1'])
})

test('test data that cannot be deleted leaves the run inconclusive and identified', async t => {
  const f = await fixture(t)
  f.state.stuck = 'twake-load-leftover'
  const { code, output } = await f.run({ FAKE_K6: 'leftover' })
  equal(code, 2, output)
  const report = f.report()
  match(report.reason, /cleanup failed.*test data remains/)
  deepEqual(report.cleanup.remaining.map((i: any) => i.name), ['twake-load-leftover'])
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
  match(f.report().reason, /interrupted by SIGTERM/)
  deepEqual(f.report().cleanup.remaining, [])
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
  const result = spawnSync('make', ['-s', '-o', 'node_modules/.load-ready', `DEV_LOCK_FILE=${lock}`, 'dev-upload'],
    { cwd, encoding: 'utf8', env: { ...process.env, PATH: `${dir}:${process.env.PATH}` } })
  equal(result.status, 2, result.stderr)
  match(result.stderr, /Shared DEV is busy/)
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
    try { return readFileSync(`/proc/${pid}/cmdline`, 'utf8').includes('dev-upload.ts') } catch { return false }
  })
  deepEqual(runners, [])
})
