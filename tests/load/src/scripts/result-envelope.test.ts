import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { test } from 'node:test'

import { classifyRamp, classifyStep, type CampaignStep } from './campaign-summary.ts'
import { renderMetrics } from './capacity-metrics.ts'
import {
  declaredOutcome, fanoutEnvelope, rampEnvelope, uploadEnvelope, validateEnvelope, type ResultEnvelope, type RunContext,
} from './result-envelope.ts'

const context: RunContext = {
  interrupted: false, versions: { harness: 'abc123', stack: '1.6.test', node: 'v22' }, topology: 'single-host',
  start: '2026-10-26T10:00:00.000Z', end: '2026-10-26T10:05:00.000Z',
}
const clean = { cleaned: ['twake-load-1'], remaining: [] }
const upload = {
  mode: 'capacity', load_target: 'local', status: 'completed', outcome: 0, result_bound: 'exact',
  maximum_concurrent_uploads: 8, file_size: '1M', criteria: { minimum_success_rate: 1 }, p95_limit_ms: 900,
  attempts: [{ vus: 8, consistency_rate: 1, cleanup_rate: 1, cleanup: clean }],
}
const step = (kind: string): CampaignStep => ({
  name: kind, kind, id: 'run-1', startedAt: context.start, endedAt: context.end, exitCode: 0, producerExitCode: 0,
})
const counts = { valid: 1, invalid: 0, unsupported: 0 }

test('a producer envelope keeps each outcome dimension apart and validates', () => {
  const envelope = uploadEnvelope(upload, context)
  deepEqual(validateEnvelope(envelope), envelope)
  deepEqual([envelope.execution, envelope.correctness, envelope.performance, envelope.cleanup],
    ['completed', 'verified', 'passed', 'verified'])
  deepEqual(envelope.capacity, { kind: 'exact', lower: 8, upper_exclusive: null, unit: 'concurrent uploads', verified: true,
    stop: 'level 9 (concurrent uploads): failed' })
  deepEqual(envelope.versions, context.versions)
  deepEqual(envelope.time, { start: context.start, end: context.end })
  equal(declaredOutcome({ result: envelope })?.code, 0)
  // Legacy reports carry no envelope and keep their own classification.
  equal(declaredOutcome({ status: 'completed' }), null)
})

test('an envelope whose verdict disagrees with its evidence is never a pass', () => {
  const base = uploadEnvelope(upload, context)
  const variants: Array<Partial<ResultEnvelope> | ((e: ResultEnvelope) => void)> = [
    { execution: 'interrupted' }, { execution: 'incomplete' }, { correctness: 'failed' }, { cleanup: 'failed' },
    { capacity: null }, { capacity: { ...base.capacity!, verified: false } },
    { outcome: 2 }, { outcome: 1 }, { outcome: 1, capacity: null },
    { capacity: { ...base.capacity!, kind: 'bracket', upper_exclusive: 8 } },
    { capacity: { ...base.capacity!, kind: 'lower_bound', upper_exclusive: 9 } },
    { schema: 'cozy-load-result/2' as ResultEnvelope['schema'] }, { producer: 'other' as ResultEnvelope['producer'] },
    { time: { start: context.end, end: context.start } }, { time: { start: 'yesterday', end: context.end } },
    e => { delete e.versions.stack }, { load_target: 'prod' as 'dev' }, { reason: undefined as unknown as string },
  ]
  for (const variant of variants) {
    const envelope = structuredClone(base)
    if (typeof variant === 'function') variant(envelope)
    else Object.assign(envelope, variant)
    equal(validateEnvelope(envelope), null, JSON.stringify(variant))
    deepEqual(declaredOutcome({ result: envelope }), { code: 2, reason: 'invalid result envelope' })
  }
  deepEqual(declaredOutcome({ result: 'passed' }), { code: 2, reason: 'invalid result envelope' })
})

test('unverified and interrupted measurements stay evidence, and failures name their dimension', () => {
  const budget = uploadEnvelope({ ...upload, status: 'infrastructure_failure', outcome: 2, result_bound: 'lower_bound',
    failure_reason: 'the MAX_RUNTIME budget ran out' }, { ...context, interrupted: true })
  deepEqual([budget.execution, budget.capacity?.kind, budget.capacity?.verified, budget.reason],
    ['interrupted', 'lower_bound', false, 'the MAX_RUNTIME budget ran out'])
  ok(validateEnvelope(budget))
  const mismatch = uploadEnvelope({ ...upload, status: 'correctness_failure', outcome: 1,
    attempts: [{ vus: 2, consistency_rate: 0.5, cleanup: clean }] }, context)
  deepEqual([mismatch.correctness, mismatch.capacity?.verified], ['failed', false])
  ok(validateEnvelope(mismatch))
  const regression = uploadEnvelope({ ...upload, mode: 'regression', status: 'regression_failure', outcome: 1 }, context)
  deepEqual([regression.producer, regression.performance, regression.capacity], ['upload-regression', 'failed', null])
  ok(validateEnvelope(regression))

  const ramp = { status: 'completed', outcome: 0, load_target: 'dev', result_bound: 'bracket', max_sustained_level: 8,
    first_failed_level: 16, cleanup_success_rate: 1, cleanup: clean, criteria: {}, plan: { levels: [8, 16] } }
  const sampled = rampEnvelope(ramp, context)
  deepEqual([sampled.correctness, sampled.cleanup, sampled.capacity], ['not_verified', 'verified',
    { kind: 'bracket', lower: 8, upper_exclusive: 16, unit: 'activity level', verified: true,
      stop: 'level 16 (activity level): failed' }])
  ok(validateEnvelope(sampled))
  const stopped = rampEnvelope({ ...ramp, status: 'infrastructure_failure', outcome: 2,
    failure_reason: 'k6 exited 2' }, context)
  deepEqual([stopped.execution, stopped.capacity?.verified, stopped.reason], ['incomplete', false, 'k6 exited 2'])
  const first = rampEnvelope({ ...ramp, outcome: 1, max_sustained_level: 0, first_failed_level: 1 }, context)
  deepEqual([first.performance, first.capacity?.verified], ['failed', false])
  ok(validateEnvelope(first))

  const fanout = { status: 'passed', outcome: 0, load_target: 'local', content_mismatch: false,
    capacity: { kind: 'lower_bound', lower: 16, upper_exclusive: null }, points: [{ n: 16, status: 'passed' }],
    settings: { levels: [16], timeout_ms: 60000, stable_ms: 3000 } }
  // Locally the organization is retained by design: the pass claims no cleanup.
  const local = fanoutEnvelope(fanout, context)
  deepEqual([local.cleanup, local.correctness, local.capacity?.unit], ['not_verified', 'verified', 'recipients'])
  ok(validateEnvelope(local))
  const leftovers = fanoutEnvelope({ ...fanout, cleanup: { cleaned: [], retained: ['drive-1'] } }, context)
  equal(validateEnvelope(leftovers), null)
  const corrupt = fanoutEnvelope({ ...fanout, status: 'failed', outcome: 1, content_mismatch: true, error: 'bytes',
    capacity: { kind: 'inconclusive', lower: null, upper_exclusive: null }, points: [{ n: 1, status: 'failed' }] },
  context)
  deepEqual([corrupt.correctness, corrupt.capacity, corrupt.reason], ['failed', null, 'bytes'])
  ok(validateEnvelope(corrupt))
})

test('ramp readers that verified content, or read it wrong, set correctness', () => {
  const ramp = { status: 'completed', outcome: 0, load_target: 'local', result_bound: 'lower_bound',
    max_sustained_level: 2, first_failed_level: null, cleanup_success_rate: 1, cleanup: clean, criteria: {},
    levels: [{ level: 1, content_verified: 2 }, { level: 2, content_verified: 2 }], correctness_failures: [] }
  equal(rampEnvelope(ramp, context).correctness, 'verified')
  const failures = ['level 2: 1 uploads read back different content']
  // Proven wrong content outweighs every measured level, even an unverified cleanup.
  const wrong = { ...ramp, status: 'infrastructure_failure', correctness_failures: failures }
  deepEqual(classifyRamp(wrong), { code: 1, reason: failures[0] })
  const envelope = rampEnvelope({ ...wrong, outcome: 1, failure_reason: failures[0] }, context)
  deepEqual([envelope.correctness, envelope.capacity?.verified], ['failed', false])
  ok(validateEnvelope(envelope))
})

test('the summary and the exporter present the declared verdict and never upgrade it', () => {
  // Legacy fields alone would classify this report as a completed measurement of 8.
  const declared = { ...upload, result: uploadEnvelope({ ...upload, outcome: 2, failure_reason: 'cleanup unproven' },
    context) }
  deepEqual(classifyStep(step('upload-capacity'), declared), { code: 2, reason: 'cleanup unproven' })
  deepEqual(classifyStep(step('upload-capacity'), upload), { code: 0, reason: 'completed' })
  const output = renderMetrics([{ campaign: 'c', run: 'run-1', kind: 'upload', timestamp: 1, data: declared }], counts)
  match(output, /status="inconclusive",bound="inconclusive"/)
  match(output, /outcome="2",evidence="= 8"/)
  ok(!output.includes('cozy_load_capacity_lower{'))

  const invalid = { ...upload, result: { ...uploadEnvelope(upload, context), cleanup: 'failed' } }
  deepEqual(classifyStep(step('upload-capacity'), invalid), { code: 2, reason: 'invalid result envelope' })
  const shown = renderMetrics([{ campaign: 'c', run: 'run-1', kind: 'upload', timestamp: 1, data: invalid }], counts)
  match(shown, /outcome="2",evidence="none"/)
  ok(!shown.includes('cozy_load_capacity_lower{'))

  // A valid pass still needs the producer to have exited 0.
  const passing = { ...upload, result: uploadEnvelope(upload, context) }
  deepEqual(classifyStep(step('upload-capacity'), passing), { code: 0, reason: '' })
  equal(classifyStep({ ...step('upload-capacity'), producerExitCode: 2 }, passing).code, 2)
  match(renderMetrics([{ campaign: 'c', run: 'run-1', kind: 'upload', timestamp: 1, data: passing }], counts),
    /status="completed",bound="exact",result="= 8"/)
})

test('every capacity search records why it stopped, in its own unit', () => {
  const failed = { vus: 9, result: 'failed', reason: 'upload p95 950 ms exceeds 900 ms' }
  const stop = (envelope: ResultEnvelope) => envelope.capacity?.stop
  equal(stop(uploadEnvelope({ ...upload, attempts: [...upload.attempts, failed] }, context)),
    'level 9 (concurrent uploads): upload p95 950 ms exceeds 900 ms')
  equal(stop(uploadEnvelope({ ...upload, result_bound: 'lower_bound', budget_reached: 'upload bytes' }, context)),
    'MAX_UPLOAD_BYTES reached after level 8 (concurrent uploads)')
  equal(stop(uploadEnvelope({ ...upload, result_bound: 'lower_bound', budget_reached: 'levels' }, context)),
    'every level passed, up to 8 (concurrent uploads)')
  const ramp = { status: 'completed', outcome: 0, load_target: 'local', result_bound: 'bracket', max_sustained_level: 8,
    first_failed_level: 16, cleanup_success_rate: 1, cleanup: clean,
    levels: [{ level: 16, status: 'failed', reasons: ['changes delivery 0.8775 is below 0.95', 'upload p95 high'] }] }
  equal(stop(rampEnvelope(ramp, context)),
    'level 16 (activity level): changes delivery 0.8775 is below 0.95; upload p95 high')
  const fanout = { status: 'passed', outcome: 0, load_target: 'local', points: [{ n: 4, status: 'passed' }],
    capacity: { kind: 'lower_bound', lower: 4, upper_exclusive: null } }
  equal(stop(fanoutEnvelope({ ...fanout, budget_reached: 'runtime' }, context)),
    'MAX_RUNTIME reached after level 4 (recipients)')
  equal(stop(fanoutEnvelope({ ...fanout, status: 'failed', capacity: { kind: 'bracket', lower: 4, upper_exclusive: 8 },
    points: [...fanout.points, { n: 8, status: 'failed', error: 'Observation SLA deadline exceeded' }] }, context)),
  'level 8 (recipients): Observation SLA deadline exceeded')
})
