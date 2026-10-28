import { deepEqual, throws } from 'node:assert/strict'
import { test } from 'node:test'

import { readCapacitySteps } from './capacity-steps.ts'

test('a capacity search grows from its start to its maximum, which it always reaches', () => {
  deepEqual(readCapacitySteps({}, { max: 16 }), { holdMs: 60_000, levels: [1, 2, 4, 8, 16] })
  deepEqual(readCapacitySteps({ CAPACITY_MAX: '20', CAPACITY_HOLD: '30s' }, { max: 16 }).levels, [1, 2, 4, 8, 16, 20])
  deepEqual(readCapacitySteps({ CAPACITY_START: '128', CAPACITY_GROWTH: '1.25', CAPACITY_MAX: '256' }, { max: 16 }).levels,
    [128, 160, 200, 250, 256])
  // A growth that rounds back to the last level still advances.
  deepEqual(readCapacitySteps({ CAPACITY_GROWTH: '1.1', CAPACITY_MAX: '4' }, { max: 16 }).levels, [1, 2, 3, 4])
  deepEqual(readCapacitySteps({ CAPACITY_START: '2', CAPACITY_MAX: '2' }, { max: 16, hold: '10s' }),
    { holdMs: 10_000, levels: [2] })
  throws(() => readCapacitySteps({ CAPACITY_GROWTH: '1' }, { max: 16 }), /CAPACITY_GROWTH/)
  throws(() => readCapacitySteps({ CAPACITY_START: '8', CAPACITY_MAX: '4' }, { max: 16 }), /below CAPACITY_START/)
  throws(() => readCapacitySteps({ CAPACITY_MAX: '0' }, { max: 16 }), /CAPACITY_MAX/)
  throws(() => readCapacitySteps({ CAPACITY_HOLD: 'soon' }, { max: 16 }), /duration/)
})
