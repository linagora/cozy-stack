import { deepEqual, equal } from 'node:assert/strict'
import { describe, it } from 'node:test'

import type { SummaryMetrics } from './load-profile.ts'
import { buildMixedThresholds, evaluateMixed, MIXED_OPERATIONS, operationMetric } from './mixed-workload.ts'

const plan = { durationS: 60, maxVus: 10, observers: 1, rate: 2 }
const criteria = { minimumSuccessRate: 0.99, p95LimitMs: 1000 }

type Sample = { iterations?: number, dropped?: number, p95?: number, errors?: number, metadataFails?: number,
  uploadFails?: number, checked?: number, unvalidated?: number }

function metrics({ iterations = 120, dropped = 0, p95 = 100, errors = 0, metadataFails = 0, uploadFails = 0,
  checked = iterations, unvalidated = 0 }: Sample): SummaryMetrics {
  const failed = Math.round(errors * iterations)
  const result: SummaryMetrics = {
    iterations: { values: { count: iterations } },
    dropped_iterations: { values: { count: dropped } },
    mixed_metadata_match: { values: { passes: checked - failed - metadataFails, fails: metadataFails } },
    mixed_upload_match: { values: { passes: checked - failed - uploadFails, fails: uploadFails } },
    mixed_observer_events: { values: { count: iterations } },
    mixed_observer_sessions: { values: { count: 1 } },
    mixed_unvalidated_responses: { values: { count: unvalidated } },
  }
  for (const operation of MIXED_OPERATIONS) {
    result[operationMetric('http_reqs', operation)] = { values: { count: iterations } }
    result[operationMetric('http_req_duration', operation)] = { values: { count: iterations, 'p(95)': p95 } }
    result[operationMetric('http_req_failed', operation)] = { values: { rate: errors, passes: failed,
      fails: iterations - failed } }
  }
  return result
}

describe('evaluateMixed', (): void => {
  it('reports workflows per second and request rates per operation', (): void => {
    const report = evaluateMixed(metrics({ iterations: 118, dropped: 0 }), plan, criteria)
    deepEqual([report.unit, report.offered_workflows, report.achieved_workflows, report.achieved_rate],
      ['workflows/s', 120, 118, 1.967])
    deepEqual(report.operations.upload, { error_rate: 0, p95_ms: 100, rate_per_s: 1.967, requests: 118 })
    deepEqual([report.status, report.reasons, report.correctness.metadata_checks], ['passed', [], 118])
  })

  it('tells a generator that fell behind from a slow or failing target', (): void => {
    const behind = evaluateMixed(metrics({ iterations: 100, dropped: 20 }), plan, criteria)
    deepEqual([behind.status, behind.reasons],
      ['generator_saturated', ['20 workflows dropped while every operation met its limits: the generator fell behind']])
    const slow = evaluateMixed(metrics({ iterations: 100, dropped: 20, p95: 1500 }), plan, criteria)
    equal(slow.status, 'failed')
    deepEqual(slow.reasons.slice(-2), ['upload p95 1500 ms exceeds 1000 ms', '20 workflows dropped'])
    const failing = evaluateMixed(metrics({ errors: 0.05 }), plan, criteria)
    deepEqual([failing.status, failing.reasons[0]], ['failed', 'list success rate 0.95 is below 0.99'])
  })

  it('records wrong metadata or uploads as correctness failures', (): void => {
    const report = evaluateMixed(metrics({ metadataFails: 2, uploadFails: 1 }), plan, criteria)
    deepEqual(report.correctness_failures, ['2 metadata reads differed from the fixture',
      '1 uploads were recorded with another checksum or size'])
    deepEqual([report.status, report.correctness.upload_mismatches], ['failed', 1])
  })

  it('responses left unchecked keep the run from passing and correctness from being complete', (): void => {
    const report = evaluateMixed(metrics({ checked: 1 }), plan, criteria)
    deepEqual([report.status, report.correctness.complete, report.correctness.metadata_expected],
      ['not_measured', false, 120])
    deepEqual(report.reasons,
      ['1 of 120 successful metadata reads were checked', '1 of 120 successful uploads were checked'])
    equal(evaluateMixed(metrics({}), plan, criteria).correctness.complete, true)
  })

  it('a response that could not be validated keeps the run from passing', (): void => {
    const report = evaluateMixed(metrics({ unvalidated: 1 }), plan, criteria)
    deepEqual([report.status, report.correctness.complete, report.reasons],
      ['not_measured', false, ['1 successful responses could not be validated']])
  })

  it('measures nothing when no workflow completed', (): void => {
    const report = evaluateMixed({}, plan, criteria)
    deepEqual([report.status, report.achieved_workflows, report.operations.list.p95_ms], ['not_measured', null, null])
  })

  it('keeps a submetric per operation', (): void => {
    deepEqual(Object.keys(buildMixedThresholds()).length, 9)
  })
})
