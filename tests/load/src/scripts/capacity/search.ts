export type CapacityPhase =
  | 'baseline'
  | 'discovery'
  | 'discovery-retry'
  | 'discovery-decider'
  | 'confirmation'

export type PointResult = 'passed' | 'failed' | 'infrastructure_failure'
export type ResultBound = 'exact' | 'lower_bound'

export type PointRequest = {
  iterationsPerVu: number
  p95LimitMs: number | null
  phase: CapacityPhase
  vus: number
}

export type PointOutcome = {
  p95Ms: number | null
  result: PointResult
}

export type CapacitySearchOptions = {
  baselineIterations: number
  configuredP95LimitMs: number | null
  confirmationIterations: number
  // The capacity levels (lib/capacity-steps.ts): level 1 is the baseline, the last is the cap.
  levels: number[]
  log?: (message: string) => void
  runPoint: (request: PointRequest) => Promise<PointOutcome>
}

export type CapacitySearchResult =
  | {
      baselineP95Ms: number
      bound: ResultBound
      maximumVus: number
      p95LimitMs: number | null
      status: 'completed'
    }
  | {
      reason: string
      status: 'infrastructure_failure'
    }

type SearchContext = {
  log: (message: string) => void
  options: CapacitySearchOptions
  p95LimitMs: number | null
}

// The search only orders k6 points. Running a point, judging it, and keeping
// the report stay with the caller, which makes the search testable without k6.
// Points are judged on latency only when an absolute limit is configured: a
// limit derived from the 1-VU baseline mostly measured latency doubling.

export function getCapacityResultBound(
  candidateVus: number,
  configuredMaxVus: number,
  firstFailingVus: number,
): ResultBound {
  return candidateVus === configuredMaxVus && firstFailingVus === 0
    ? 'lower_bound'
    : 'exact'
}

function abort(reason: string): CapacitySearchResult {
  return { reason, status: 'infrastructure_failure' }
}

async function evaluateBurstLevel(context: SearchContext, vus: number): Promise<PointOutcome> {
  const { options, p95LimitMs } = context
  const runBurst = (phase: CapacityPhase): Promise<PointOutcome> =>
    options.runPoint({ iterationsPerVu: 1, p95LimitMs, phase, vus })
  const firstOutcome = await runBurst('discovery')

  if (firstOutcome.result !== 'failed') {
    return firstOutcome
  }

  context.log(`Retrying failing discovery point at ${vus} VUs\n`)

  const secondOutcome = await runBurst('discovery-retry')

  if (secondOutcome.result === 'infrastructure_failure') {
    return secondOutcome
  }
  if (secondOutcome.result === firstOutcome.result) {
    return firstOutcome
  }

  context.log(`Running deciding discovery point at ${vus} VUs\n`)
  return runBurst('discovery-decider')
}

function evaluateConfirmationLevel(
  context: SearchContext,
  vus: number,
): Promise<PointOutcome> {
  return context.options.runPoint({
    iterationsPerVu: context.options.confirmationIterations,
    p95LimitMs: context.p95LimitMs,
    phase: 'confirmation',
    vus,
  })
}

type BisectionResult =
  | { highestPassingVus: number; status: 'completed' }
  | { failingVus: number; status: 'infrastructure_failure' }

async function bisect(
  low: number,
  high: number,
  initialPassingVus: number,
  evaluate: (vus: number) => Promise<PointOutcome>,
): Promise<BisectionResult> {
  let highestPassingVus = initialPassingVus
  let searchLow = low
  let searchHigh = high

  while (searchLow <= searchHigh) {
    const searchMid = Math.floor((searchLow + searchHigh) / 2)
    const outcome = await evaluate(searchMid)

    if (outcome.result === 'infrastructure_failure') {
      return { failingVus: searchMid, status: 'infrastructure_failure' }
    }

    if (outcome.result === 'passed') {
      highestPassingVus = searchMid
      searchLow = searchMid + 1
    } else {
      searchHigh = searchMid - 1
    }
  }

  return { highestPassingVus, status: 'completed' }
}

type DiscoveryResult =
  | { firstFailingVus: number; lastPassingVus: number; status: 'completed' }
  | { reason: string; status: 'infrastructure_failure' }

async function discover(context: SearchContext): Promise<DiscoveryResult> {
  let lastPassingVus = 1
  let firstFailingVus = 0

  for (const currentVus of context.options.levels.filter(level => level > 1)) {
    const outcome = await evaluateBurstLevel(context, currentVus)

    if (outcome.result === 'infrastructure_failure') {
      return {
        reason: `discovery failed because of test infrastructure at ${currentVus} VUs`,
        status: 'infrastructure_failure',
      }
    }

    if (outcome.result === 'failed') {
      firstFailingVus = currentVus
      break
    }

    lastPassingVus = currentVus
  }

  if (firstFailingVus === 0) {
    return { firstFailingVus, lastPassingVus, status: 'completed' }
  }

  const bisection = await bisect(
    lastPassingVus + 1,
    firstFailingVus - 1,
    lastPassingVus,
    (vus: number): Promise<PointOutcome> => evaluateBurstLevel(context, vus),
  )

  if (bisection.status === 'infrastructure_failure') {
    return {
      reason: `binary discovery failed because of test infrastructure at ${bisection.failingVus} VUs`,
      status: 'infrastructure_failure',
    }
  }

  return {
    firstFailingVus,
    lastPassingVus: bisection.highestPassingVus,
    status: 'completed',
  }
}

export async function searchCapacity(
  options: CapacitySearchOptions,
): Promise<CapacitySearchResult> {
  const baselineOutcome = await options.runPoint({
    iterationsPerVu: options.baselineIterations,
    p95LimitMs: null,
    phase: 'baseline',
    vus: 1,
  })

  if (baselineOutcome.result === 'infrastructure_failure') {
    return abort('baseline failed because of test infrastructure')
  }
  if (baselineOutcome.p95Ms === null) {
    return abort('baseline produced no p95 measurement')
  }

  const baselineP95Ms = baselineOutcome.p95Ms
  const p95LimitMs = options.configuredP95LimitMs
  const complete = (
    maximumVus: number,
    bound: ResultBound,
  ): CapacitySearchResult => ({
    baselineP95Ms,
    bound,
    maximumVus,
    p95LimitMs,
    status: 'completed',
  })

  if (baselineOutcome.result === 'failed') {
    return complete(0, 'exact')
  }

  const context: SearchContext = {
    log: options.log ?? ((): void => {}),
    options,
    p95LimitMs,
  }
  const discovery = await discover(context)

  if (discovery.status === 'infrastructure_failure') {
    return abort(discovery.reason)
  }

  const candidateVus = discovery.lastPassingVus
  const confirmationOutcome = await evaluateConfirmationLevel(context, candidateVus)

  if (confirmationOutcome.result === 'infrastructure_failure') {
    return abort(
      `confirmation failed because of test infrastructure at ${candidateVus} VUs`,
    )
  }

  if (confirmationOutcome.result === 'passed') {
    return complete(
      candidateVus,
      getCapacityResultBound(
        candidateVus,
        options.levels.at(-1)!,
        discovery.firstFailingVus,
      ),
    )
  }

  const confirmation = await bisect(
    1,
    candidateVus - 1,
    0,
    (vus: number): Promise<PointOutcome> => evaluateConfirmationLevel(context, vus),
  )

  if (confirmation.status === 'infrastructure_failure') {
    return abort(
      `confirmation search failed because of test infrastructure at ${confirmation.failingVus} VUs`,
    )
  }

  return complete(confirmation.highestPassingVus, 'exact')
}
