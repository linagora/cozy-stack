import { deepEqual, equal, ok, throws } from 'node:assert/strict'
import { chmodSync, existsSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'

import {
  classifyMixed, holdLock, judgeRecovery, parseResources, readConfig, runningMixedWorkloads,
} from './mixed-campaign.ts'
import { mixedEnvelope, validateEnvelope, type RunContext } from './result-envelope.ts'

// A seeded single-instance tree fixture: 5 directories, 7 files, 1 drive and 6 root entries.
function manifest(organization: string | null = null): string {
  const path = join(mkdtempSync(join(tmpdir(), 'mixed-manifest-')), 'manifest.json')
  writeFileSync(path, JSON.stringify({
    version: 1, profile: 'tree', profile_version: 1, generation_seed: '1', status: 'complete',
    domain: 'seed-a.localhost:8080', organization, parameters: { users: 0, files: 7, drives: 1, context: null },
    content: { bytes: 1, md5: 'x' }, instances: [{ domain: 'seed-a.localhost:8080', uuid: 'u', role: 'owner',
      created: true, dirs: ['d1', 'd2', 'd3', 'd4', 'd5'], files: ['f1', 'f2', 'f3', 'f4', 'f5', 'f6', 'f7'],
      drives: [{ sharing: 's', root: 'r' }], root_entries: 6 }],
  }))
  return path
}
const env = { EXECUTION_ID: 'mixed-1', MIXED_DOMAIN: 'seed-a.localhost:8080', MIXED_RATE: '2', MIXED_DURATION: '1m',
  MIXED_MAX_VUS: '8', MIXED_OBSERVERS: '1', SEED_MANIFEST: manifest() }
const context: RunContext = { interrupted: false, versions: { harness: 'abc', stack: '1.6.test' },
  topology: 'single-host', start: '2026-10-26T10:00:00Z', end: '2026-10-26T10:02:00Z' }
const passed = { status: 'passed', reasons: [], correctness_failures: [], problems: [], load_target: 'local',
  correctness: { metadata_checks: 120, upload_checks: 120, complete: true },
  cleanup: { cleaned: ['d'], remaining: [] } }

test('budgets are checked against what the arrivals can need, before any request', () => {
  const config = readConfig(env)
  // 121 workflows (k6 starts one more at t=0) of 3 requests and one observer session, plus k6 setup, a
  // token, the Stack version, two verifications of 16 requests each and 6 cleanup requests.
  deepEqual(config.needs, { vus: 9, requests: 405, bytes: 123_904, runtimeMs: 240_000 })
  readConfig({ ...env, MAX_VUS: '9', MAX_REQUESTS: '405', MAX_UPLOAD_BYTES: '121K', MAX_RUNTIME: '4m' })
  throws(() => readConfig({ ...env, SEED_MANIFEST: manifest('seed-a') }),
    /single-instance tree fixture/)
  for (const [name, value, reason] of [['MAX_VUS', '8', /VUs/], ['MAX_REQUESTS', '404', /requests/],
    ['MAX_UPLOAD_BYTES', '100K', /bytes/], ['MAX_RUNTIME', '3m', /minutes/]] as const) {
    throws(() => readConfig({ ...env, [name]: value }), reason)
  }
  throws(() => readConfig({ ...env, LOAD_TARGET: 'dev' }), /local tree fixture/)
  throws(() => readConfig({ ...env, MIXED_DOMAIN: '' }), /MIXED_DOMAIN/)
  throws(() => readConfig({ ...env, MIXED_OBSERVERS: '-1' }), /MIXED_OBSERVERS/)
  // Unreadable criteria would pass every comparison in k6.
  throws(() => readConfig({ ...env, MIN_SUCCESS_RATE: '0,99' }), /MIN_SUCCESS_RATE/)
  throws(() => readConfig({ ...env, P95_LIMIT_MS: '2s' }), /P95_LIMIT_MS/)
  deepEqual([config.minSuccessRate, config.p95LimitMs], [0.99, 2000])
  const observers = (value: string) => readConfig({ ...env, MIXED_OBSERVERS: value }).observers
  deepEqual([observers(''), observers('0')],
    [1, 0])
})

test('wrong content exits 1, a regression 1, anything unverified or saturated 2', () => {
  deepEqual(classifyMixed(passed), { code: 0, reason: '' })
  equal(classifyMixed({ ...passed, status: 'failed', reasons: ['upload p95 3000 ms exceeds 2000 ms'] }).code, 1)
  deepEqual(classifyMixed({ ...passed, status: 'failed', correctness_failures: ['1 uploads were recorded wrong'],
    problems: ['k6 exited 2'] }), { code: 1, reason: '1 uploads were recorded wrong' })
  equal(classifyMixed({ ...passed, problems: ['test data remains after cleanup'] }).code, 2)
  equal(classifyMixed({ ...passed, status: 'generator_saturated', reasons: ['3 workflows dropped'] }).code, 2)
})

test('the envelope keeps the rate verdict, its correctness and cleanup apart, and never claims a capacity', () => {
  for (const [report, expected] of [
    [{ ...passed, outcome: 0 }, ['passed', 'verified', 'verified']],
    [{ ...passed, outcome: 1, status: 'failed', reasons: ['list p95 3000 ms exceeds 2000 ms'] },
      ['failed', 'verified', 'verified']],
    [{ ...passed, outcome: 1, status: 'failed', reasons: ['2 metadata reads differed'],
      correctness_failures: ['2 metadata reads differed'] }, ['not_measured', 'failed', 'verified']],
    [{ ...passed, outcome: 2, status: 'generator_saturated', reasons: ['3 workflows dropped'] },
      ['not_measured', 'verified', 'verified']],
    // Checks that missed successful responses verify nothing.
    [{ ...passed, outcome: 2, status: 'not_measured', reasons: ['1 of 120 successful uploads were checked'],
      correctness: { metadata_checks: 120, upload_checks: 1, complete: false } },
    ['not_measured', 'not_verified', 'verified']],
  ] as const) {
    const envelope = mixedEnvelope(report, context)
    deepEqual([envelope.performance, envelope.correctness, envelope.cleanup], expected)
    equal(envelope.capacity, null)
    ok(validateEnvelope(envelope), JSON.stringify(report))
  }
})

test('a soak warms up and drains within its caps, and runs long only after a passed trial', t => {
  const soak = { ...env, MIXED_PROFILE: 'soak' }
  const config = readConfig(soak)
  // The steady phase as set, after the default one-minute warm-up and before the one-minute drain.
  deepEqual([config.warmupMs, config.durationMs, config.drainMs], [60_000, 60_000, 60_000])
  // 1,320 workflows over the warm-up and ten steady minutes, then the same extras as a fixed run plus two
  // metrics samples; the warm-up's 10 s grace keeps it apart from the steady phase.
  deepEqual(readConfig({ ...soak, MIXED_DURATION: '' }).needs, {
    // One extra arrival at the start of each of the two arrival scenarios.
    vus: 9, bytes: 1322 * 1024, requests: 1322 * 3 + 1 + 1 + 32 + 6 + 2 + 2,
    runtimeMs: 60_000 + 10_000 + 600_000 + 60_000 + 180_000,
  })
  throws(() => readConfig({ ...soak, MIXED_DURATION: '30m' }), /needs SOAK_TRIAL/)
  const dir = mkdtempSync(join(tmpdir(), 'soak-trial-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const trial = join(dir, 'trial.json')
  const write = (outcome: number, rate: number, fixture = env.MIXED_DOMAIN) => writeFileSync(trial, JSON.stringify(
    { fixture, plan: { rate }, result: { producer: 'mixed-workload', outcome } }))
  write(0, 2)
  equal(readConfig({ ...soak, MIXED_DURATION: '30m', SOAK_TRIAL: trial }).durationMs, 1_800_000)
  const trials = [[2, 2, env.MIXED_DOMAIN], [0, 1, env.MIXED_DOMAIN], [0, 2, 'other']] as const
  for (const [outcome, rate, fixture] of trials) {
    write(outcome, rate, fixture)
    throws(() => readConfig({ ...soak, MIXED_DURATION: '30m', SOAK_TRIAL: trial }), /not a passed trial/)
  }
})

test('the Stack must give back its goroutines and heap after the drain', () => {
  const sample = parseResources('# HELP x\ngo_goroutines 200\ngo_memstats_heap_inuse_bytes 4e+07\n' +
    'process_resident_memory_bytes 1.5e+08\n')
  deepEqual(sample, { goroutines: 200, heap_inuse_bytes: 40_000_000, resident_bytes: 150_000_000 })
  throws(() => parseResources('go_goroutines 200\n'), /lack goroutines or memory/)
  const limits = { max_goroutine_growth: 1.25, max_heap_growth: 1.5 }
  deepEqual(judgeRecovery(sample, { ...sample, goroutines: 260, heap_inuse_bytes: 70_000_000 }, limits), [])
  deepEqual(judgeRecovery(sample, { ...sample, goroutines: 300, heap_inuse_bytes: 90_000_000 }, limits), [
    'goroutines 300 after the drain exceed 1.25 × 200',
    'heap 90000000 bytes after the drain exceeds 1.5 × 40000000'])
})

test('one run holds the local lock; a lock whose process is gone is taken over', t => {
  const dir = mkdtempSync(join(tmpdir(), 'mixed-lock-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const lock = join(dir, '.lock')
  const release = holdLock(lock)
  throws(() => holdLock(lock), new RegExp(`pid ${process.pid}`))
  release()
  equal(existsSync(lock), false)
  writeFileSync(lock, '2147483646')
  holdLock(lock)()
  equal(existsSync(lock), false)
})

test('a k6 still running a mixed workload is found, and an unreadable inventory refuses to guess', t => {
  const dir = mkdtempSync(join(tmpdir(), 'mixed-docker-'))
  const path = process.env.PATH
  t.after(() => {
    process.env.PATH = path
    rmSync(dir, { recursive: true, force: true })
  })
  process.env.PATH = `${dir}:${path}`
  // Stands in for docker: two k6 containers, one of them a mixed workload. FAIL fails docker ps;
  // UNREADABLE fails inspecting c2, which GONE has removed by the time it is listed again.
  writeFileSync(join(dir, 'docker'), `#!/bin/sh
[ -n "$FAIL" ] && exit 1
[ "$1" = ps ] && [ "$4" = "id=c2" ] && { [ -n "$GONE" ] || echo c2; exit 0; }
[ "$1" = ps ] && { echo c1; echo c2; exit 0; }
[ "$4" = c2 ] && [ -n "$UNREADABLE" ] && exit 1
[ "$4" = c1 ] && echo '["EXECUTION_ID=old","MIXED_RATE=2"]' || echo '["FILE_SIZE=1M"]'
`)
  chmodSync(join(dir, 'docker'), 0o700)
  deepEqual(runningMixedWorkloads(), ['c1'])
  t.after(() => { for (const name of ['FAIL', 'UNREADABLE', 'GONE']) delete process.env[name] })
  // A container that cannot be inspected may be a survivor, unless it is gone.
  process.env.UNREADABLE = '1'
  throws(() => runningMixedWorkloads(), /docker inspect c2 failed/)
  process.env.GONE = '1'
  deepEqual(runningMixedWorkloads(), ['c1'])
  process.env.FAIL = '1'
  throws(() => runningMixedWorkloads(), /cannot be ruled out/)
})
