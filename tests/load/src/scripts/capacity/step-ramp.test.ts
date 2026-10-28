import { deepEqual } from 'node:assert/strict'
import { test } from 'node:test'

import { stepRamp, type LevelVerdict } from './step-ramp.ts'

const plan = [1, 2, 4, 8]
// Runs levels with the given verdicts, recording which ran.
const runner = (verdicts: Record<number, LevelVerdict>, ran: number[] = []) => async (level: number) => {
  ran.push(level)
  return { verdict: verdicts[level] ?? 'passed', reasons: verdicts[level] ? [`level ${level} ${verdicts[level]}`] : [] }
}
const outcome = ({ lower, upper, bound, budget }: { lower: number, upper: number | null, bound: string,
  budget: string | null }) => ({ lower, upper, bound, budget })

test('a search advances after each passing level and its last level is a lower bound', async () => {
  const ran: number[] = []
  const search = await stepRamp(plan, runner({}, ran))
  deepEqual([ran, outcome(search)], [plan, { lower: 8, upper: null, bound: 'lower_bound', budget: null }])
  deepEqual(search.levels.map(l => [l.level, l.verdict]), plan.map(l => [l, 'passed']))
})

test('a search stops at the first failing level, which brackets it', async () => {
  const ran: number[] = []
  deepEqual(outcome(await stepRamp(plan, runner({ 4: 'failed', 8: 'failed' }, ran))),
    { lower: 2, upper: 4, bound: 'bracket', budget: null })
  deepEqual(ran, [1, 2, 4])
  // A first level that fails measured no capacity: [0, 1).
  deepEqual(outcome(await stepRamp(plan, runner({ 1: 'failed' }))), { lower: 0, upper: 1, bound: 'bracket', budget: null })
})

test('a level that cannot be judged stops the search without a bound', async () => {
  const ran: number[] = []
  deepEqual(outcome(await stepRamp(plan, runner({ 2: 'inconclusive' }, ran))),
    { lower: 1, upper: null, bound: 'inconclusive', budget: null })
  deepEqual(ran, [1, 2])
})

test('a budget that ends while levels pass leaves a lower bound; one with no room leaves nothing', async () => {
  const ran: number[] = []
  const fits = (level: number) => level > 2 ? 'MAX_VUS' : ''
  deepEqual(outcome(await stepRamp(plan, runner({}, ran), { fits })),
    { lower: 2, upper: null, bound: 'lower_bound', budget: 'MAX_VUS' })
  deepEqual(ran, [1, 2])
  deepEqual(outcome(await stepRamp(plan, runner({}), { fits: () => 'MAX_RUNTIME' })),
    { lower: 0, upper: null, bound: 'inconclusive', budget: 'MAX_RUNTIME' })
})

test('an interruption stops the search after the running level, without a bound', async () => {
  const ran: number[] = []
  deepEqual(outcome(await stepRamp(plan, runner({}, ran), { interrupted: () => ran.length === 2 })),
    { lower: 2, upper: null, bound: 'inconclusive', budget: null })
  deepEqual(ran, [1, 2])
})
