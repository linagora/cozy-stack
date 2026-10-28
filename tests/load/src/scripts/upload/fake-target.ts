// A fake Cloudery, instance Stack and docker for the campaign runners' tests (upload-campaign.test.ts,
// shared-drive-campaign.test.ts).
import { spawn } from 'node:child_process'
import { chmodSync, existsSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

export const cwd = fileURLToPath(new URL('../../../', import.meta.url))
export const fqdn = 'seedtest01.dev.example.test'
const uuid = 'uuid-seedtest01'

// Stands in for docker: logs each call, answers `compose run` like k6 would, per FAKE_K6.
const fakeDocker = `#!/usr/bin/env node
const fs = require('fs'), http = require('http')
const args = process.argv.slice(2)
const log = line => fs.appendFileSync(process.env.FAKE_DOCKER_LOG, JSON.stringify(line) + '\\n')
log({ args, token: process.env.COZY_ACCESS_TOKEN || null, writer: process.env.WRITER_ACCESS_TOKEN || null })
if (args[0] === 'image') process.exit(1)
if (args[0] === 'info') console.log(String(8 * 2 ** 30))
// A hanging k6 is container c1, with its run's environment, until docker stop ends it.
const pidFile = process.env.FAKE_DOCKER_LOG + '.pid'
// FAKE_DOCKER_FAIL fails one command; with inspect-gone, the container also exits first.
const fail = process.env.FAKE_DOCKER_FAIL || ''
if (fail && args[0] === fail.split('-')[0]) {
  if (fail === 'inspect-gone') fs.rmSync(pidFile, { force: true })
  process.exit(1)
}
const container = () => JSON.parse(fs.readFileSync(pidFile, 'utf8'))
// The Stack container and its address, for member actors (drive-actors.ts).
if (args[0] === 'compose' && args[1] === 'ps') { console.log('stack-1'); process.exit(0) }
if (args[0] === 'inspect' && args[1] === '-f') { console.log('172.18.0.5 '); process.exit(0) }
if (args[0] === 'ps') console.log(fs.existsSync(pidFile) ? 'c1' : '')
if (args[0] === 'inspect') console.log(JSON.stringify(container().env))
if (args[0] === 'stop') process.kill(container().pid, 'SIGUSR2')
if (args[1] !== 'run') process.exit(0)
const env = Object.fromEntries(args.flatMap((a, i) => args[i - 1] === '-e' ? [a.split(/=(.*)/s).slice(0, 2)] : []))
// FAKE_PLAN picks a mode per "phase:workers"; fail misses the success criterion.
const mode = JSON.parse(process.env.FAKE_PLAN || '{}')[env.CAPACITY_PHASE + ':' + env.UPLOAD_VUS] || process.env.FAKE_K6
const hang = () => {
  // Signalling docker compose leaves the container running; only docker stop (SIGUSR2 here) ends it.
  fs.writeFileSync(pidFile, JSON.stringify({ pid: process.pid,
    env: ['EXECUTION_ID=' + env.EXECUTION_ID, 'BASE_URL=' + env.BASE_URL] }))
  process.on('SIGTERM', () => {})
  process.on('SIGUSR2', () => { fs.rmSync(pidFile); log('k6 stopped'); process.exit(105) })
  log('running')
  setInterval(() => {}, 1000)
}
const mismatch = mode.includes('mismatch') && Number(env.UPLOAD_VUS) >= Number(process.env.FAKE_MISMATCH_VUS || 1)
// The ramp writes its one level's verdict, as handleSummary does; levels from FAKE_RAMP_FAIL on fail.
const ramp = args.includes('/scripts/shared-drive-ramp.ts')
const finishRamp = () => {
  const level = Number(env.CAPACITY_START)
  const failed = level >= Number(process.env.FAKE_RAMP_FAIL || Infinity)
  if (mode !== 'no-report') fs.writeFileSync('data/results/' + env.EXECUTION_ID + '-shared-drive-ramp.json', JSON.stringify({
    status: 'completed', result_bound: failed ? 'bracket' : 'lower_bound', max_sustained_level: failed ? 0 : level,
    first_failed_level: failed ? level : null, failure_reason: null, cleanup_success_rate: 1, correctness_failures: [],
    levels: [{ level, status: failed ? 'failed' : 'passed', reasons: failed ? ['upload p95 3000 ms exceeds 2000 ms'] : [] }] }))
  process.exit(Number(process.env.FAKE_RAMP_EXIT || 0))
}
const finishUpload = () => {
  const name = args.find(a => a.startsWith('--summary-export=')).split('/').pop()
  fs.writeFileSync('data/results/' + name, JSON.stringify({ metrics: {
    'http_req_duration{operation:file-upload}': { 'p(95)': 12 },
    'upload_success{operation:file-upload}': { value: mode === 'fail' ? 0.5 : 1 },
    iterations: { count: env.UPLOAD_VUS * env.ITERATIONS_PER_VU },
    cleanup_success: { value: mode.includes('leftover') ? 0 : 1 },
    upload_consistency: { value: mismatch ? 0 : 1 },
  } }))
  process.exit(mismatch ? 108 : mode === 'fail' ? 99 : 0)
}
const proceed = mode.includes('hang') ? hang : ramp ? finishRamp : finishUpload
// Named and placed like the scenario's own test directory, which only teardown deletes.
const name = 'twake-load-' + (ramp ? 'sd' : env.FILE_SIZE.toLowerCase()) + '-' + env.EXECUTION_ID + '-leftover'
const parent = ramp ? '/sharings/drives/' + env.SHARED_DRIVE_ID + '/' + env.SHARED_DRIVE_ROOT_ID : '/files/io.cozy.files.root-dir'
if (!mode.includes('leftover')) proceed()
else http.request(process.env.FAKE_STACK_URL + parent + '?Type=directory&Name=' + name, {
  method: 'POST', headers: { Host: new URL(env.BASE_URL).host, Authorization: 'Bearer ' + process.env.COZY_ACCESS_TOKEN },
}, res => { res.resume(); res.on('end', proceed) }).end()
`

// One server plays the Cloudery token route and the instance (by Host); the root lists one child per
// page. The instance owns the test drives, whose roots share one listing.
export async function fakeTarget(t: TestContext, script: string) {
  const state = {
    root: new Map<string, string>([['docs', 'Documents']]), trash: new Map<string, string>(),
    drives: [] as Array<{ id: string, root: string }>, drive: new Map<string, string>(),
    requests: [] as string[], stuck: '', uuid, host: fqdn, tokenDelayMs: 0,
  }
  const asDrive = ({ id, root }: { id: string, root: string }) => ({ id, attributes: {
    description: 'Load-Test Shared Drive', active: true, owner: true, drive_root_type: 'directory', rules: [{ values: [root] }] } })
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
      await new Promise(resolve => setTimeout(resolve, state.tokenDelayMs))
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
    if (route === 'GET /sharings/drives') return send(200, { data: state.drives.map(asDrive) })
    if (route === 'POST /sharings/drives') {
      const n = state.drives.push({ id: `drive-${state.drives.length + 1}`, root: `drive-root-${state.drives.length + 1}` })
      return send(201, { data: asDrive(state.drives[n - 1]!) })
    }
    if (req.method === 'GET' && first && !second && state.drives.some(d => d.root === first)) {
      return send(200, { data: { id: first }, included: list(state.drive) })
    }
    if (req.method === 'POST' && route.startsWith('POST /sharings/drives/')) {
      state.drive.set(`sd-${state.requests.length}`, url.searchParams.get('Name')!)
      return send(201, {})
    }
    if (route === 'POST /files/io.cozy.files.root-dir') {
      state.root.set(`dir-${state.requests.length}`, url.searchParams.get('Name')!)
      return send(201, {})
    }
    if (req.method === 'DELETE' && first === 'trash' && state.trash.has(second!) && !stuck(state.trash.get(second!)!)) {
      state.trash.delete(second!)
      return send(204)
    }
    const dir = [state.root, state.drive].find(items => items.has(first!))
    if (req.method === 'DELETE' && dir) {
      state.trash.set(first!, dir.get(first!)!)
      dir.delete(first!)
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
  const dir = mkdtempSync(join(tmpdir(), 'load-runner-'))
  const id = `load-runner-test-${process.pid}-${Date.now()}`
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

  const settings = /^(SEED_|DEV_|MAX_|RAMP_|MODE$|LOAD_TARGET$|BASE_URL$|STACK_URL$|COZY_ACCESS_TOKEN$|CAPACITY_RUN_ID$|EXECUTION_ID$|UPLOAD_TARGET$|VUS$|ITERATIONS_PER_VU$|FILE_SIZE$|BASELINE_ITERATIONS$|CONFIRMATION_ITERATIONS$|UPLOAD_TIMEOUT$)/
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
  const report = (kind = 'upload-regression') => JSON.parse(readFileSync(join(cwd, `data/results/${id}-${kind}.json`), 'utf8'))
  const local = { LOAD_TARGET: 'local', BASE_URL: 'http://local.test:8080', STACK_URL: origin, COZY_ACCESS_TOKEN: 'instance-token' }
  return { state, docker, report, run: (env?: Record<string, string>) => start(env).done, start, env: baseEnv, dockerDir: dir, dockerLog, id, local }
}
