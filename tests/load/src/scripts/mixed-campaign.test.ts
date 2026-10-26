import { deepEqual, equal, ok, throws } from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'

import { classifyMixed, readConfig } from './mixed-campaign.ts'
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
