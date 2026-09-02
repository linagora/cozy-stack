import { equal } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  computeP95LimitMs,
  getCapacityResultBound,
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
