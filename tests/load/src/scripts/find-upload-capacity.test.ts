import { equal } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  computeP95LimitMs,
  getCapacityResultBound,
  isInterruptedCapacityPoint,
} from './find-upload-capacity.ts'

describe('getCapacityResultBound', (): void => {
  it('returns a lower bound when the configured cap passed', (): void => {
    equal(getCapacityResultBound(8, 8, 0), 'lower_bound')
  })

  it('returns a lower bound for a single-VU configured cap', (): void => {
    equal(getCapacityResultBound(1, 1, 0), 'lower_bound')
  })

  it('returns an exact bound when a failing level was measured', (): void => {
    equal(getCapacityResultBound(7, 8, 8), 'exact')
  })
})

describe('computeP95LimitMs', (): void => {
  it('uses the baseline multiplier when no absolute limit is configured', (): void => {
    equal(computeP95LimitMs(25, 20, null), 500)
  })

  it('uses the absolute limit when configured', (): void => {
    equal(computeP95LimitMs(25, 20, 2_000), 2_000)
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
