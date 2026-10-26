import {
  getRampHoldStartMs,
  parseRampUploadLevel,
  type RampPlan,
} from './load-profile.ts'

// What one reader saw of the ramp's uploads: unique files per level, so a
// repeated notification or a coalesced revision never counts twice.
export class LevelObservations {
  private readonly seen = new Map<string, Set<string>>()
  duplicates = 0

  // True for a ramp upload this reader had not seen yet.
  record(name: unknown): boolean {
    const level = parseRampUploadLevel(name)
    if (level === 'unattributed' || typeof name !== 'string') {
      return false
    }
    const files = this.seen.get(level) ?? new Set<string>()
    this.seen.set(level, files)
    if (files.has(name)) {
      this.duplicates++
      return false
    }
    files.add(name)
    return true
  }

  unique(level: number): number {
    return this.seen.get(String(level))?.size ?? 0
  }
}

// Writers derive each upload's bytes from the marker in its name, so a reader
// can recompute the content it should download.
export function parseUploadMarker(name: unknown): string | null {
  if (typeof name !== 'string') {
    return null
  }
  return /-iter[0-9]+-([0-9]+-[0-9]+-[0-9]+-[0-9]+)\.bin$/.exec(name)?.[1] ?? null
}

// Only a reader ready before a level's hold began can be expected to see all
// of its uploads; a late reader is not judged for that level.
export function observedWholeHold(
  plan: RampPlan,
  level: number,
  readyAtMs: number | null,
): boolean {
  return readyAtMs !== null && plan.levels.includes(level) &&
    readyAtMs <= getRampHoldStartMs(plan, level)
}
