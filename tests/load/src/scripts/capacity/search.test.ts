import { deepEqual, equal } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  getCapacityResultBound,
  searchCapacity,
  type CapacitySearchOptions,
  type PointOutcome,
  type PointRequest,
} from './search.ts'

type RecordedRequest = Pick<
  PointRequest,
  'iterationsPerVu' | 'p95LimitMs' | 'phase' | 'vus'
>

function makeOptions(
  runPoint: (request: PointRequest) => PointOutcome,
  overrides: Partial<CapacitySearchOptions> = {},
): CapacitySearchOptions {
  return {
    baselineIterations: 50,
    configuredP95LimitMs: null,
    confirmationIterations: 20,
    levels: [1, 2, 4, 8, 16],
    runPoint: async (request: PointRequest): Promise<PointOutcome> => runPoint(request),
    ...overrides,
  }
}

function passUpTo(
  maximumPassingVus: number,
  requests: RecordedRequest[],
): (request: PointRequest) => PointOutcome {
  return (request: PointRequest): PointOutcome => {
    requests.push(request)
    return {
      p95Ms: 100,
      result: request.vus <= maximumPassingVus ? 'passed' : 'failed',
    }
  }
}

describe('searchCapacity', (): void => {
  it('reports a lower bound when every level passes up to the cap', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(
      makeOptions(passUpTo(Infinity, requests), { levels: [1, 2, 4, 8] }),
    )

    deepEqual(result, {
      baselineP95Ms: 100,
      bound: 'lower_bound',
      maximumVus: 8,
      p95LimitMs: null,
      status: 'completed',
    })
    deepEqual(
      requests.map(({ phase, vus, iterationsPerVu }) => [
        phase,
        vus,
        iterationsPerVu,
      ]),
      [
        ['baseline', 1, 50],
        ['discovery', 2, 1],
        ['discovery', 4, 1],
        ['discovery', 8, 1],
        ['confirmation', 8, 20],
      ],
    )
  })

  it('without refinement, keeps the levels\' own bracket or lower bound', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(
      makeOptions(passUpTo(5, requests), { confirmationIterations: 0, levels: [1, 2, 4, 8, 16] }),
    )
    deepEqual(result, {
      baselineP95Ms: 100, bound: 'bracket', firstFailingVus: 8, maximumVus: 4, p95LimitMs: null, status: 'completed',
    })
    // No bisection between 4 and 8, and no confirmation.
    deepEqual(requests.map(({ phase, vus }) => [phase, vus]),
      [['baseline', 1], ['discovery', 2], ['discovery', 4], ['discovery', 8], ['discovery-retry', 8]])
    const all = await searchCapacity(makeOptions(passUpTo(Infinity, []), { confirmationIterations: 0, levels: [1, 2] }))
    deepEqual([all.status, all.status === 'completed' && all.bound], ['completed', 'lower_bound'])
  })

  it('judges no point on latency unless a limit is configured', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(
      makeOptions(passUpTo(Infinity, requests), { levels: [1, 2] }),
    )

    equal(result.status === 'completed' && result.p95LimitMs, null)
    deepEqual(
      requests.map(({ p95LimitMs }) => p95LimitMs),
      [null, null, null],
    )
  })

  it('uses the configured absolute p95 limit instead of the baseline multiplier', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(
      makeOptions(passUpTo(Infinity, requests), {
        configuredP95LimitMs: 750,
        levels: [1, 2],
      }),
    )

    equal(result.status === 'completed' && result.p95LimitMs, 750)
    deepEqual(
      requests.map(({ p95LimitMs }) => p95LimitMs),
      [null, 750, 750],
    )
  })

  it('bisects between the last passing and first failing levels', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(makeOptions(passUpTo(5, requests)))

    deepEqual(result, {
      baselineP95Ms: 100,
      bound: 'exact',
      maximumVus: 5,
      p95LimitMs: null,
      status: 'completed',
    })
    deepEqual(
      requests.map(({ phase, vus }) => [phase, vus]),
      [
        ['baseline', 1],
        ['discovery', 2],
        ['discovery', 4],
        ['discovery', 8],
        ['discovery-retry', 8],
        ['discovery', 6],
        ['discovery-retry', 6],
        ['discovery', 5],
        ['confirmation', 5],
      ],
    )
  })

  it('runs a deciding point when a failing level passes on retry', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const outcomes: Record<string, PointOutcome['result']> = {
      'discovery-2': 'failed',
      'discovery-retry-2': 'passed',
      'discovery-decider-2': 'passed',
    }
    const result = await searchCapacity(
      makeOptions(
        (request: PointRequest): PointOutcome => {
          requests.push(request)
          return {
            p95Ms: 100,
            result: outcomes[`${request.phase}-${request.vus}`] ?? 'passed',
          }
        },
        { levels: [1, 2] },
      ),
    )

    equal(result.status === 'completed' && result.maximumVus, 2)
    deepEqual(
      requests.map(({ phase }) => phase),
      [
        'baseline',
        'discovery',
        'discovery-retry',
        'discovery-decider',
        'confirmation',
      ],
    )
  })

  it('searches downward with confirmation points when the candidate fails confirmation', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(
      makeOptions(
        (request: PointRequest): PointOutcome => {
          requests.push(request)
          const sustainedLimit = 3
          const passed =
            request.phase !== 'confirmation' || request.vus <= sustainedLimit
          return { p95Ms: 100, result: passed ? 'passed' : 'failed' }
        },
        { levels: [1, 2, 4, 8] },
      ),
    )

    deepEqual(result, {
      baselineP95Ms: 100,
      bound: 'exact',
      maximumVus: 3,
      p95LimitMs: null,
      status: 'completed',
    })
    deepEqual(
      requests
        .filter(({ phase }) => phase === 'confirmation')
        .map(({ vus, iterationsPerVu }) => [vus, iterationsPerVu]),
      [
        [8, 20],
        [4, 20],
        [2, 20],
        [3, 20],
      ],
    )
  })

  it('reports zero when no level passes confirmation', async (): Promise<void> => {
    const result = await searchCapacity(
      makeOptions(
        (request: PointRequest): PointOutcome => ({
          p95Ms: 100,
          result: request.phase === 'confirmation' ? 'failed' : 'passed',
        }),
        { levels: [1, 2, 4] },
      ),
    )

    equal(result.status === 'completed' && result.maximumVus, 0)
  })

  it('reports zero without further points when the baseline fails', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(makeOptions(passUpTo(0, requests)))

    deepEqual(result, {
      baselineP95Ms: 100,
      bound: 'exact',
      maximumVus: 0,
      p95LimitMs: null,
      status: 'completed',
    })
    equal(requests.length, 1)
  })

  it('aborts when the baseline hits an infrastructure failure', async (): Promise<void> => {
    const result = await searchCapacity(
      makeOptions(
        (): PointOutcome => ({ p95Ms: null, result: 'infrastructure_failure' }),
      ),
    )

    deepEqual(result, {
      reason: 'baseline failed because of test infrastructure',
      status: 'infrastructure_failure',
    })
  })

  it('aborts when the baseline has no p95 measurement', async (): Promise<void> => {
    const result = await searchCapacity(
      makeOptions((): PointOutcome => ({ p95Ms: null, result: 'passed' })),
    )

    deepEqual(result, {
      reason: 'baseline produced no p95 measurement',
      status: 'infrastructure_failure',
    })
  })

  it('aborts with the level when discovery hits an infrastructure failure', async (): Promise<void> => {
    const result = await searchCapacity(
      makeOptions(
        (request: PointRequest): PointOutcome => ({
          p95Ms: 100,
          result: request.vus === 4 ? 'infrastructure_failure' : 'passed',
        }),
      ),
    )

    deepEqual(result, {
      reason: 'discovery failed because of test infrastructure at 4 VUs',
      status: 'infrastructure_failure',
    })
  })

  it('confirms a single-VU cap without discovery points', async (): Promise<void> => {
    const requests: RecordedRequest[] = []
    const result = await searchCapacity(
      makeOptions(passUpTo(Infinity, requests), { levels: [1] }),
    )

    equal(result.status === 'completed' && result.bound, 'lower_bound')
    deepEqual(
      requests.map(({ phase }) => phase),
      ['baseline', 'confirmation'],
    )
  })
})

describe('getCapacityResultBound', (): void => {
  it('returns a lower bound when the configured cap passed', async (): Promise<void> => {
    equal(getCapacityResultBound(8, 8, 0), 'lower_bound')
  })

  it('returns a lower bound for a single-VU configured cap', async (): Promise<void> => {
    equal(getCapacityResultBound(1, 1, 0), 'lower_bound')
  })

  it('returns an exact bound when a failing level was measured', async (): Promise<void> => {
    equal(getCapacityResultBound(7, 8, 8), 'exact')
  })
})
