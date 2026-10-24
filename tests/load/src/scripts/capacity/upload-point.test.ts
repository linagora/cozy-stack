import { deepEqual, equal } from 'node:assert/strict'
import { describe, it } from 'node:test'

import { searchCapacity, type PointRequest } from './search.ts'
import {
  classifyUploadPoint,
  hasContentMismatch,
  isInterruptedCapacityPoint,
  readUploadPointMetrics,
  type UploadPointClassificationInput,
  type UploadPointMetrics,
} from './upload-point.ts'

const passingMetrics: UploadPointMetrics = {
  cleanupRate: 1,
  completedIterations: 4,
  consistencyRate: 1,
  droppedIterations: 0,
  p95Ms: 150,
  successRate: 1,
}

function makeInput(
  overrides: Partial<UploadPointClassificationInput> = {},
): UploadPointClassificationInput {
  return {
    expectedIterations: 4,
    metrics: passingMetrics,
    minimumSuccessRate: 0.99,
    p95LimitMs: 200,
    runnerExitCode: 0,
    summaryFound: true,
    ...overrides,
  }
}

describe('readUploadPointMetrics', (): void => {
  it('prefers the file-upload submetrics', (): void => {
    deepEqual(
      readUploadPointMetrics({
        metrics: {
          cleanup_success: { value: 1 },
          dropped_iterations: { count: 2 },
          http_req_duration: { 'p(95)': 999 },
          'http_req_duration{operation:file-upload}': { 'p(95)': 120 },
          iterations: { count: 10 },
          upload_consistency: { value: 1 },
          upload_success: { value: 0.5 },
          'upload_success{operation:file-upload}': { value: 0.9 },
        },
      }),
      {
        cleanupRate: 1,
        completedIterations: 10,
        consistencyRate: 1,
        droppedIterations: 2,
        p95Ms: 120,
        successRate: 0.9,
      },
    )
  })

  it('reports no consistency rate when the scenario did not record one', (): void => {
    equal(
      readUploadPointMetrics({
        metrics: {
          cleanup_success: { value: 1 },
          http_req_duration: { 'p(95)': 120 },
          iterations: { count: 10 },
          upload_success: { value: 1 },
        },
      })?.consistencyRate,
      null,
    )
  })

  it('treats a missing dropped_iterations metric as zero', (): void => {
    equal(
      readUploadPointMetrics({
        metrics: {
          cleanup_success: { value: 1 },
          http_req_duration: { 'p(95)': 120 },
          iterations: { count: 10 },
          upload_success: { value: 1 },
        },
      })?.droppedIterations,
      0,
    )
  })

  it('returns null when a required metric is missing', (): void => {
    equal(
      readUploadPointMetrics({
        metrics: {
          http_req_duration: { 'p(95)': 120 },
          iterations: { count: 10 },
          upload_success: { value: 1 },
        },
      }),
      null,
    )
  })

  it('returns null for a summary that is not an object', (): void => {
    equal(readUploadPointMetrics(null), null)
    equal(readUploadPointMetrics([]), null)
  })
})

describe('classifyUploadPoint', (): void => {
  it('passes when every criterion holds and k6 succeeded', (): void => {
    deepEqual(classifyUploadPoint(makeInput()), {
      reason: '',
      result: 'passed',
    })
  })

  it('reports an infrastructure failure when k6 wrote no summary', (): void => {
    deepEqual(
      classifyUploadPoint(makeInput({ metrics: null, summaryFound: false })),
      { reason: 'k6 produced no summary', result: 'infrastructure_failure' },
    )
  })

  it('reports an infrastructure failure when the summary lacks metrics', (): void => {
    deepEqual(classifyUploadPoint(makeInput({ metrics: null })), {
      reason: 'summary is missing required metrics',
      result: 'infrastructure_failure',
    })
  })

  it('fails a point whose uploads did not read back intact, even after k6 aborted', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({
          metrics: {
            ...passingMetrics,
            cleanupRate: 1,
            completedIterations: 2,
            consistencyRate: 0.5,
          },
          runnerExitCode: 108,
        }),
      ),
      {
        reason: 'uploaded content did not match after download',
        result: 'failed',
      },
    )
  })

  for (const { completedIterations, runnerExitCode } of [
    { completedIterations: 4, runnerExitCode: 0 },
    { completedIterations: 2, runnerExitCode: 108 },
  ]) {
    it(`prioritizes cleanup over content mismatch after ${completedIterations} iterations`, (): void => {
      deepEqual(
        classifyUploadPoint(
          makeInput({
            metrics: {
              ...passingMetrics,
              cleanupRate: 0,
              completedIterations,
              consistencyRate: 0.5,
            },
            runnerExitCode,
          }),
        ),
        {
          reason: 'test-directory cleanup failed; uploaded content did not match after download',
          result: 'infrastructure_failure',
        },
      )
    })
  }

  it('stops capacity probes after cleanup fails with a content mismatch', (): void => {
    const requests: PointRequest[] = []
    const result = searchCapacity({
      baselineIterations: 4,
      configuredP95LimitMs: 200,
      confirmationIterations: 4,
      maxVus: 8,
      runPoint: (request) => {
        requests.push(request)
        const expectedIterations = request.iterationsPerVu * request.vus
        const cleanupFailed = request.vus === 4
        const metrics: UploadPointMetrics = {
          ...passingMetrics,
          cleanupRate: cleanupFailed ? 0 : 1,
          completedIterations: cleanupFailed ? 2 : expectedIterations,
          consistencyRate: cleanupFailed ? 0.5 : 1,
        }
        const classification = classifyUploadPoint(
          makeInput({
            expectedIterations,
            metrics,
            p95LimitMs: request.p95LimitMs,
            runnerExitCode: cleanupFailed ? 108 : 0,
          }),
        )

        return { p95Ms: metrics.p95Ms, result: classification.result }
      },
    })

    deepEqual(
      requests.map(({ phase, vus }) => [phase, vus]),
      [
        ['baseline', 1],
        ['discovery', 2],
        ['discovery', 4],
      ],
    )
    deepEqual(result, {
      reason: 'discovery failed because of test infrastructure at 4 VUs',
      status: 'infrastructure_failure',
    })
  })

  it('reports cleanup failures before measured failures', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({
          metrics: { ...passingMetrics, cleanupRate: 0, successRate: 0 },
        }),
      ),
      {
        reason: 'test-directory cleanup failed',
        result: 'infrastructure_failure',
      },
    )
  })

  it('reports an interrupted run as an infrastructure failure', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({
          metrics: { ...passingMetrics, completedIterations: 3 },
          runnerExitCode: 99,
        }),
      ),
      {
        reason: 'k6 stopped before completing every upload consistency check',
        result: 'infrastructure_failure',
      },
    )
  })

  it('reports a run without iterations as an infrastructure failure', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({ metrics: { ...passingMetrics, completedIterations: 0 } }),
      ),
      {
        reason: 'k6 completed no upload iterations',
        result: 'infrastructure_failure',
      },
    )
  })

  it('fails when iterations were dropped', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({ metrics: { ...passingMetrics, droppedIterations: 1 } }),
      ),
      { reason: 'iterations were dropped or incomplete', result: 'failed' },
    )
  })

  it('fails when the success rate is below the minimum', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({ metrics: { ...passingMetrics, successRate: 0.98 } }),
      ),
      { reason: 'upload success rate is below the minimum', result: 'failed' },
    )
  })

  it('fails when the p95 exceeds the limit', (): void => {
    deepEqual(
      classifyUploadPoint(
        makeInput({ metrics: { ...passingMetrics, p95Ms: 201 } }),
      ),
      { reason: 'upload p95 exceeds the configured limit', result: 'failed' },
    )
  })

  it('ignores the p95 when no limit applies', (): void => {
    equal(
      classifyUploadPoint(
        makeInput({
          metrics: { ...passingMetrics, p95Ms: 10_000 },
          p95LimitMs: null,
        }),
      ).result,
      'passed',
    )
  })

  it('reports a measured failure even when k6 exited unsuccessfully', (): void => {
    equal(
      classifyUploadPoint(
        makeInput({
          metrics: { ...passingMetrics, successRate: 0.5 },
          runnerExitCode: 99,
        }),
      ).result,
      'failed',
    )
  })

  it('reports an infrastructure failure when k6 failed but criteria passed', (): void => {
    deepEqual(classifyUploadPoint(makeInput({ runnerExitCode: 99 })), {
      reason: 'k6 exited unsuccessfully even though measured criteria passed',
      result: 'infrastructure_failure',
    })
  })
})

describe('isInterruptedCapacityPoint', (): void => {
  it('detects an aborted run with incomplete consistency checks', (): void => {
    equal(isInterruptedCapacityPoint(2, 3, 4), true)
  })

  it('does not confuse a completed threshold failure with an abort', (): void => {
    equal(isInterruptedCapacityPoint(2, 4, 4), false)
  })

  it('does not treat an incomplete successful process as an abort', (): void => {
    equal(isInterruptedCapacityPoint(0, 3, 4), false)
  })
})

describe('hasContentMismatch', (): void => {
  it('finds any attempt whose downloaded content differed', (): void => {
    equal(hasContentMismatch([{ consistency_rate: 1 }, { consistency_rate: 0.99 }]), true)
  })

  it('ignores attempts without a consistency measurement', (): void => {
    equal(hasContentMismatch([{ consistency_rate: 1 }, { consistency_rate: null }, {}]), false)
  })
})
