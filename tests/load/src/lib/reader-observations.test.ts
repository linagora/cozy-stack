import { deepEqual, equal } from 'node:assert/strict'
import { describe, it } from 'node:test'

import { type RampPlan } from './load-profile.ts'
import { LevelObservations, observedWholeHold, parseUploadMarker } from './reader-observations.ts'

const plan: RampPlan = {
  holdMs: 60_000,
  levels: [1, 2],
  transitionMs: 10_000,
  vusPerLevel: { changesReaders: 2, writers: 1, wsReaders: 3 },
}
const file = (level: number, n: number) => `sd-L${level}-vu1-iter${n}-${n}-1-1700000000000-42.bin`

describe('LevelObservations', (): void => {
  it('counts each file once per level, whatever repeats', (): void => {
    const seen = new LevelObservations()
    deepEqual([file(1, 1), file(1, 1), file(1, 2), file(2, 1), file(1, 2)].map(name => seen.record(name)),
      [true, false, true, true, false])
    deepEqual([seen.unique(1), seen.unique(2), seen.duplicates], [2, 1, 2])
  })

  it('leaves a missing file missing and ignores what no level produced', (): void => {
    const seen = new LevelObservations()
    for (const name of [file(1, 1), 'sd-transition-vu1-iter1-1-1-1-1.bin', 'other.txt', undefined, 42]) {
      seen.record(name)
    }
    deepEqual([seen.unique(1), seen.unique(2), seen.duplicates], [1, 0, 0])
  })
})

describe('parseUploadMarker', (): void => {
  it('reads the marker a writer derived its bytes from', (): void => {
    equal(parseUploadMarker(file(2, 7)), '7-1-1700000000000-42')
  })

  it('refuses malformed names', (): void => {
    for (const name of ['sd-L1-vu1-iter1-x.bin', 'sd-L1-vu1-iter1-1-1-1-1.txt', 'sd-L1.bin', null, {}]) {
      equal(parseUploadMarker(name), null)
    }
  })
})

describe('observedWholeHold', (): void => {
  it('judges only readers ready before the hold began', (): void => {
    // Level 2 holds from 80 s: ready during its transition is in time, ready during the hold is late.
    deepEqual([75_000, 80_000, 80_001].map(at => observedWholeHold(plan, 2, at)), [true, true, false])
    deepEqual([observedWholeHold(plan, 1, null), observedWholeHold(plan, 3, 0)], [false, false])
  })
})
