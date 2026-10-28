// The step loop capacity searches share (README, Capacity searches): run a level, judge it, then advance
// after a pass or stop. A search stops at the first level that fails or cannot be judged, before a level
// its budget no longer covers, or once interrupted. Running and judging a level stay with the runner.

export type LevelVerdict = 'passed' | 'failed' | 'inconclusive'
export interface Judged { verdict: LevelVerdict, reasons: string[] }
export interface StepSearch<T> {
  levels: Array<T & Judged & { level: number }>
  // The highest passing level; 0 when the first level failed.
  lower: number
  // The first failing level, in a bracket.
  upper: number | null
  bound: 'bracket' | 'lower_bound' | 'inconclusive'
  // The budget setting that ended the search while its levels still passed.
  budget: string | null
}

export async function stepRamp<T extends object>(
  levels: readonly number[],
  run: (level: number) => Promise<T & Judged>,
  // fits names the budget a level would exceed, or returns ''.
  { fits = () => '', interrupted = () => false }: { fits?: (level: number) => string, interrupted?: () => boolean } = {},
): Promise<StepSearch<T>> {
  const judged: StepSearch<T>['levels'] = []
  const end = (bound: StepSearch<T>['bound'], budget: string | null = null): StepSearch<T> => ({
    levels: judged, lower: judged.filter(l => l.verdict === 'passed').at(-1)?.level ?? 0,
    upper: bound === 'bracket' ? judged.at(-1)!.level : null, bound, budget,
  })
  for (const level of levels) {
    const budget = fits(level)
    // Nothing measured yet: the budget leaves no room for the search at all.
    if (budget) return end(judged.length ? 'lower_bound' : 'inconclusive', budget)
    const result = await run(level)
    judged.push({ ...result, level })
    if (interrupted()) return end('inconclusive')
    if (result.verdict !== 'passed') return end(result.verdict === 'failed' ? 'bracket' : 'inconclusive')
  }
  return end('lower_bound')
}
