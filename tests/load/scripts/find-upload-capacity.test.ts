import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import { getCapacityResultBound } from './find-upload-capacity.ts'

describe('getCapacityResultBound', (): void => {
  it('returns a lower bound when the configured cap passed', (): void => {
    assert.equal(getCapacityResultBound(8, 8, 0), 'lower_bound')
  })

  it('returns a lower bound for a single-VU configured cap', (): void => {
    assert.equal(getCapacityResultBound(1, 1, 0), 'lower_bound')
  })

  it('returns an exact bound when a failing level was measured', (): void => {
    assert.equal(getCapacityResultBound(7, 8, 8), 'exact')
  })
})
