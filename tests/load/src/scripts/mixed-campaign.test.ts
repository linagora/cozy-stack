import { deepEqual, equal, ok, throws } from 'node:assert/strict'
import { test } from 'node:test'

import { classifyMixed, readConfig } from './mixed-campaign.ts'
import { mixedEnvelope, validateEnvelope, type RunContext } from './result-envelope.ts'

const env = { EXECUTION_ID: 'mixed-1', MIXED_DOMAIN: 'seed-a.localhost:8080', MIXED_RATE: '2', MIXED_DURATION: '1m',
  MIXED_MAX_VUS: '8', MIXED_OBSERVERS: '1' }
const context: RunContext = { interrupted: false, versions: { harness: 'abc', stack: '1.6.test' },
  topology: 'single-host', start: '2026-10-26T10:00:00Z', end: '2026-10-26T10:02:00Z' }
const passed = { status: 'passed', reasons: [], correctness_failures: [], problems: [], load_target: 'local',
  correctness: { metadata_checks: 120, upload_checks: 120 }, cleanup: { cleaned: ['d'], remaining: [] } }

test('budgets are checked against what the arrivals can need, before any request', () => {
  const config = readConfig(env)
  deepEqual(config.needs, { vus: 9, requests: 361, bytes: 122_880, runtimeMs: 240_000 })
  readConfig({ ...env, MAX_VUS: '9', MAX_REQUESTS: '361', MAX_UPLOAD_BYTES: '120K', MAX_RUNTIME: '4m' })
  for (const [name, value, reason] of [['MAX_VUS', '8', /VUs/], ['MAX_REQUESTS', '360', /requests/],
    ['MAX_UPLOAD_BYTES', '100K', /bytes/], ['MAX_RUNTIME', '3m', /minutes/]] as const) {
    throws(() => readConfig({ ...env, [name]: value }), reason)
  }
  throws(() => readConfig({ ...env, LOAD_TARGET: 'dev' }), /local tree fixture/)
  throws(() => readConfig({ ...env, MIXED_DOMAIN: '' }), /MIXED_DOMAIN/)
  throws(() => readConfig({ ...env, MIXED_OBSERVERS: '-1' }), /MIXED_OBSERVERS/)
  deepEqual([readConfig({ ...env, MIXED_OBSERVERS: '' }).observers, readConfig({ ...env, MIXED_OBSERVERS: '0' }).observers],
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
  ] as const) {
    const envelope = mixedEnvelope(report, context)
    deepEqual([envelope.performance, envelope.correctness, envelope.cleanup], expected)
    equal(envelope.capacity, null)
    ok(validateEnvelope(envelope), JSON.stringify(report))
  }
})
