import { spawn } from 'node:child_process'
import assert from 'node:assert/strict'
import { once } from 'node:events'
import {
  linkSync, mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, utimesSync, writeFileSync,
} from 'node:fs'
import { mkdtemp, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

import { createResultsServer, readResults } from './serve-capacity-results.ts'

function uploadReport() {
  return {
    campaign_id: 'run', status: 'completed', result_bound: 'lower_bound',
    maximum_concurrent_uploads: 8, file_size: '1M', upload_target: 'personal',
    attempts: [{ vus: 1, phase: 'baseline', result: 'passed' }],
  }
}

const scratch = tmpdir()

function directory(t: { after: (fn: () => void) => void }): string {
  const path = mkdtempSync(join(scratch, 'capacity-reader-'))
  t.after(() => rmSync(path, { recursive: true, force: true }))
  return path
}

test('reads recognized top-level reports and preserves file mtime, not scrape time', t => {
  const dir = directory(t)
  const path = join(dir, 'run-upload-capacity.json')
  writeFileSync(path, JSON.stringify(uploadReport()))
  utimesSync(path, 100, 100)
  writeFileSync(join(dir, 'other-summary.json'), '{"metrics":{}}')
  const snapshot = readResults(dir)
  assert.equal(snapshot.reports.length, 1)
  assert.equal(snapshot.reports[0]?.timestamp, 100)
  assert.deepEqual(snapshot.counts, { valid: 1, invalid: 0, unsupported: 1 })
})

test('rejects file and root symlinks without following them', t => {
  const dir = directory(t)
  const outside = directory(t)
  writeFileSync(join(outside, 'private.json'), JSON.stringify(uploadReport()))
  symlinkSync(join(outside, 'private.json'), join(dir, 'run-upload-capacity.json'))
  assert.equal(readResults(dir).reports.length, 0)
  const rootLink = join(dir, 'root-link')
  symlinkSync(outside, rootLink)
  assert.throws(() => readResults(rootLink), /unsafe_directory/)
})

test('rejects duplicate run identities rather than selecting any maximum', t => {
  const dir = directory(t)
  for (const name of ['one', 'two']) {
    writeFileSync(join(dir, `${name}-upload-capacity.json`), JSON.stringify(uploadReport()))
  }
  const result = readResults(dir)
  assert.equal(result.reports.length, 0)
  assert.equal(result.counts.invalid, 2)
})

test('rejects oversized report files and bounds total bytes', t => {
  const dir = directory(t)
  const payload = `${JSON.stringify(uploadReport())}${' '.repeat(1048576)}`
  writeFileSync(join(dir, 'run-upload-capacity.json'), payload)
  assert.equal(readResults(dir).counts.invalid, 1)
  writeFileSync(join(dir, 'run-upload-capacity.json'), JSON.stringify(uploadReport()))
  assert.throws(() => readResults(dir, { maxTotalBytes: 10 }), /work_limit/)
})

test('bounds directory entries and points instead of returning truncated capacity', t => {
  const dir = directory(t)
  for (let i = 0; i < 3; i++) writeFileSync(join(dir, `entry${i}`), '')
  assert.throws(() => readResults(dir, { maxEntries: 2 }), /work_limit/)
  const report = uploadReport()
  report.attempts = Array(257).fill(report.attempts[0])
  writeFileSync(join(dir, 'run-upload-capacity.json'), JSON.stringify(report))
  assert.equal(readResults(dir).counts.invalid, 1)
})

test('bounds aggregate point output across otherwise small valid reports', t => {
  const dir = directory(t)
  for (let i = 0; i < 9; i++) {
    const source = uploadReport()
    source.campaign_id = `run${i}`
    source.attempts = Array(256).fill(source.attempts[0])
    writeFileSync(join(dir, `run${i}-upload-capacity.json`), JSON.stringify(source))
  }
  assert.throws(() => readResults(dir), /work_limit/)
})

test('malformed JSON and malformed identities are counted but never exposed', t => {
  const dir = directory(t)
  writeFileSync(join(dir, 'bad-upload-capacity.json'), '{"secret":"https://private.example"')
  writeFileSync(join(dir, 'id-upload-capacity.json'), JSON.stringify({
    ...uploadReport(), campaign_id: '../private',
  }))
  const result = readResults(dir)
  assert.deepEqual(result.counts, { valid: 0, invalid: 2, unsupported: 0 })
  assert.equal(JSON.stringify(result).includes('private'), false)
})

test('campaign membership comes from exact step IDs and recorded time, not date prefixes', t => {
  const dir = directory(t)
  const campaign = join(dir, 'campaigns', 'batch')
  mkdirSync(campaign, { recursive: true })
  writeFileSync(join(dir, 'one-upload-capacity.json'), JSON.stringify(uploadReport()))
  writeFileSync(join(dir, 'other-upload-capacity.json'), JSON.stringify({
    ...uploadReport(), campaign_id: 'run2',
  }))
  writeFileSync(join(campaign, 'environment.txt'), 'run_id=campaign-1\nBASE_URL=https://private.example\n')
  const step = 'upload\tupload-capacity\trun\t2026-01-01T00:00:00Z\t2026-01-01T00:01:00Z\t0\n'
  writeFileSync(join(campaign, 'steps.tsv'), step)
  const snapshot = readResults(dir)
  const run = snapshot.reports.find(result => result.run === 'run')
  assert.equal(run?.campaign, 'campaign-1')
  assert.equal(run?.startedAt, Date.parse('2026-01-01T00:00:00Z') / 1000)
  assert.equal(snapshot.reports.find(result => result.run === 'run2')?.campaign, 'run2')
  assert.equal(JSON.stringify(snapshot).includes('private'), false)
})

test('ambiguous campaign step IDs reject the affected report', t => {
  const dir = directory(t)
  writeFileSync(join(dir, 'run-upload-capacity.json'), JSON.stringify(uploadReport()))
  for (const name of ['one', 'two']) {
    const campaign = join(dir, 'campaigns', name)
    mkdirSync(campaign, { recursive: true })
    writeFileSync(join(campaign, 'environment.txt'), `run_id=${name}\n`)
    writeFileSync(join(campaign, 'steps.tsv'), 'upload\tupload-capacity\trun\t\t\t0\n')
  }
  assert.equal(readResults(dir).reports.length, 0)
})

test('symlinked campaign directories cannot change standalone provenance', t => {
  const dir = directory(t)
  const outside = directory(t)
  writeFileSync(join(dir, 'run-upload-capacity.json'), JSON.stringify(uploadReport()))
  writeFileSync(join(outside, 'steps.tsv'), 'upload\tupload-capacity\trun\t\t\t0\n')
  writeFileSync(join(outside, 'environment.txt'), 'run_id=forged\n')
  mkdirSync(join(dir, 'campaigns'))
  symlinkSync(outside, join(dir, 'campaigns', 'unsafe'))
  assert.throws(() => readResults(dir), /unsafe_campaign/)
})

test('loopback server serves classified metrics and health but no raw files or write API', async t => {
  const dir = mkdtempSync(join(tmpdir(), 'capacity-http-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const path = join(dir, 'run-upload-capacity.json')
  const raw = JSON.stringify(uploadReport())
  writeFileSync(path, raw)
  const server = createResultsServer(dir)
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => new Promise<void>(resolve => server.close(() => resolve())))
  const address = server.address()
  assert.ok(address && typeof address !== 'string')
  const base = `http://127.0.0.1:${address.port}`
  const health = await fetch(`${base}/health`)
  assert.equal(health.status, 200)
  assert.equal(await health.text(), 'ok\n')
  const metrics = await fetch(`${base}/metrics`)
  assert.match(metrics.headers.get('content-type')!, /text\/plain; version=0.0.4/)
  assert.match(await metrics.text(), /cozy_load_capacity_lower\{.*\} 8\n/)
  for (const route of ['/run-upload-capacity.json', '/results', '/metrics?raw=1', '/%2e%2e/private']) {
    assert.equal((await fetch(`${base}${route}`)).status, 404)
  }
  for (const method of ['POST', 'PUT', 'DELETE', 'PATCH', 'HEAD', 'OPTIONS']) {
    assert.equal((await fetch(`${base}/metrics`, { method })).status, 405)
  }
  assert.equal(readFileSync(path, 'utf8'), raw)
  writeFileSync(path, JSON.stringify({ ...uploadReport(), status: 'infrastructure_failure' }))
  assert.doesNotMatch(await (await fetch(`${base}/metrics`)).text(), /^cozy_load_capacity_lower\{/m)
})

test('unreadable result root returns a bounded failure, never internal paths', async t => {
  const server = createResultsServer('/not/a/real/private/results/path')
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => new Promise<void>(resolve => server.close(() => resolve())))
  const address = server.address()
  assert.ok(address && typeof address !== 'string')
  const reply = await fetch(`http://127.0.0.1:${address.port}/metrics`)
  assert.equal(reply.status, 503)
  assert.equal(await reply.text(), 'results_unavailable\n')
})

test('the results container mounts every module the exporter loads at runtime', () => {
  const compose = readFileSync(new URL('../../compose.yaml', import.meta.url), 'utf8')
  const modules = new Set<string>()
  const queue = ['serve-capacity-results.ts']
  for (let file = queue.pop(); file !== undefined; file = queue.pop()) {
    if (modules.has(file)) continue
    modules.add(file)
    // Type-only imports are erased by --experimental-strip-types.
    for (const [, path] of readFileSync(new URL(file, import.meta.url), 'utf8')
      .matchAll(/^import (?!type )[^'"]*from '(\.[^']+)'/gm)) queue.push(join(dirname(file), path!))
  }
  assert.ok(modules.has('result-envelope.ts'))
  for (const file of modules) assert.ok(compose.includes(`./src/scripts/${file}:/scripts/${file}:ro`), file)
})

test('entrypoint starts with native strip-types, without npm or tsx at runtime', { timeout: 5000 }, async t => {
  const dir = mkdtempSync(join(tmpdir(), 'capacity-native-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  writeFileSync(join(dir, 'run-upload-capacity.json'), JSON.stringify(uploadReport()))
  const entrypoint = fileURLToPath(new URL('./serve-capacity-results.ts', import.meta.url))
  const child = spawn(process.execPath, ['--experimental-strip-types', entrypoint], {
    env: { ...process.env, CAPACITY_RESULTS_DIR: dir, CAPACITY_RESULTS_PORT: '0' },
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  t.after(async () => {
    if (child.exitCode !== null) return
    const exit = once(child, 'exit')
    child.kill('SIGTERM')
    await exit
  })
  const lines = createInterface({ input: child.stdout })
  t.after(() => lines.close())
  const [line] = await once(lines, 'line')
  const match = /^capacity_results_listening ([0-9]+)$/.exec(String(line))
  assert.ok(match)
  const base = `http://127.0.0.1:${match[1]}`
  assert.equal((await fetch(`${base}/health`)).status, 200)
  const response = await fetch(`${base}/metrics`)
  assert.equal(response.status, 200)
  assert.match(await response.text(), /cozy_load_capacity_lower\{.*\} 8\n/)
})

test('exporter presents the producer capacity without recomputing missing point diagnostics', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'producer-authority-'))
  t.after(() => rm(directory, { recursive: true, force: true }))
  const report = {
    campaign_id: 'producer-authority', status: 'completed', result_bound: 'exact',
    maximum_concurrent_uploads: 57, file_size: '1M', upload_target: 'personal',
  }
  await writeFile(join(directory, 'producer-authority-upload-capacity.json'), JSON.stringify(report))
  const server = createResultsServer(directory)
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve())))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('Expected a loopback listener')
  const response = await fetch(`http://127.0.0.1:${address.port}/metrics`)
  const metrics = await response.text()
  assert.match(metrics, /cozy_load_capacity_lower\{[^\n]*run="producer-authority"[^\n]*\} 57\n/)
})

test('point work limit uses the report kind, not an unrelated empty array', async t => {
  const directory = await mkdtemp(join(tmpdir(), 'producer-budget-'))
  t.after(() => rm(directory, { recursive: true, force: true }))
  const report = {
    status: 'completed', result_bound: 'bracket', max_sustained_level: 1, first_failed_level: 2,
    attempts: [], levels: Array.from({ length: 257 }, () => ({ level: 1, status: 'passed' })),
  }
  await writeFile(join(directory, 'bounded-shared-drive-ramp.json'), JSON.stringify(report))
  const server = createResultsServer(directory)
  server.listen(0, '127.0.0.1')
  await once(server, 'listening')
  t.after(() => new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve())))
  const address = server.address()
  if (!address || typeof address === 'string') throw new Error('Expected a loopback listener')
  const response = await fetch(`http://127.0.0.1:${address.port}/metrics`)
  const metrics = await response.text()
  assert.match(metrics, /cozy_load_exporter_reports\{state="invalid"\} 1\n/)
  assert.doesNotMatch(metrics, /^cozy_load_point_info\{/m)
})

test('rejects hardlinked reports rather than reading an alias of a private file', t => {
  const dir = directory(t)
  const outside = directory(t)
  const secret = join(outside, 'private.json')
  writeFileSync(secret, JSON.stringify(uploadReport()))
  linkSync(secret, join(dir, 'run-upload-capacity.json'))
  assert.equal(readResults(dir).counts.invalid, 1)
})

test('symlinked campaign metadata cannot forge membership', t => {
  const dir = directory(t)
  const outside = directory(t)
  const campaign = join(dir, 'campaigns', 'batch')
  mkdirSync(campaign, { recursive: true })
  writeFileSync(join(dir, 'run-upload-capacity.json'), JSON.stringify(uploadReport()))
  writeFileSync(join(outside, 'steps.tsv'), 'upload\tupload-capacity\trun\t\t\t0\n')
  symlinkSync(join(outside, 'steps.tsv'), join(campaign, 'steps.tsv'))
  assert.throws(() => readResults(dir), /unsafe_campaign/)
})

test('recognized report files still require a JSON object', t => {
  const dir = directory(t)
  for (const [index, value] of [null, [], 1, 'private'].entries()) {
    writeFileSync(join(dir, `invalid${index}-org-drive-fanout.json`), JSON.stringify(value))
  }
  assert.deepEqual(readResults(dir).counts, { valid: 0, invalid: 4, unsupported: 0 })
})

test('regression reports are read, timed by their own record outside a campaign', t => {
  const dir = directory(t)
  writeFileSync(join(dir, 'reg-upload-regression.json'), JSON.stringify({
    campaign_id: 'reg', mode: 'regression', status: 'completed', outcome: 0,
    started_at: '2026-10-25T13:52:08.000Z', ended_at: '2026-10-25T13:52:13.000Z', attempts: [],
  }))
  const snapshot = readResults(dir)
  assert.deepEqual(snapshot.counts, { valid: 1, invalid: 0, unsupported: 0 })
  assert.equal(snapshot.reports[0]?.run, 'reg')
  assert.equal(snapshot.reports[0]?.kind, 'upload')
  assert.equal(snapshot.reports[0]?.startedAt, Date.parse('2026-10-25T13:52:08Z') / 1000)
  assert.equal(snapshot.reports[0]?.endedAt, Date.parse('2026-10-25T13:52:13Z') / 1000)
})
