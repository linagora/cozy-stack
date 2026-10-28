import { parseDurationToMs } from './duration.ts'
import { parsePositiveIntegerEnv } from './env.ts'

// The levels every capacity search steps through, each in its runner's own unit (README, Capacity
// searches): CAPACITY_START, then CAPACITY_GROWTH times the last level, up to and including
// CAPACITY_MAX. Runners that hold a level for a time hold it for CAPACITY_HOLD.

export type CapacitySteps = { holdMs: number, levels: number[] }

export function readCapacitySteps(
  env: Record<string, string | undefined>,
  defaults: { max: number, hold?: string },
): CapacitySteps {
  const start = parsePositiveIntegerEnv(env, 'CAPACITY_START', 1)
  const max = parsePositiveIntegerEnv(env, 'CAPACITY_MAX', defaults.max)
  const growth = Number(env.CAPACITY_GROWTH || '2')
  if (!Number.isFinite(growth) || growth <= 1) {
    throw new Error(`CAPACITY_GROWTH must be a number above 1, got "${env.CAPACITY_GROWTH}"`)
  }
  if (max < start) throw new Error(`CAPACITY_MAX ${max} is below CAPACITY_START ${start}`)
  const levels = [start]
  for (let last = start; last < max; levels.push(last)) {
    // A growth that rounds back to the last level still advances by one.
    last = Math.min(max, Math.max(last + 1, Math.round(last * growth)))
  }
  return { holdMs: parseDurationToMs(env.CAPACITY_HOLD || defaults.hold || '1m'), levels }
}
