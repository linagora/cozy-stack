import { deepEqual, equal, match, throws } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  buildRampThresholds,
  buildRampingVuStages,
  evaluateRampLevels,
  formatRampReport,
  getCompletedRampLevels,
  getRampPhaseAt,
  getRampUploadPrefix,
  getServedRampPhase,
  getRampTotalDurationMs,
  parseRampCriteria,
  parseRampPlan,
  parseRampUploadLevel,
  rampLevelMetricName,
  type RampCriteria,
  type RampPlan,
  type SummaryMetrics,
} from './load-profile.ts'

const plan: RampPlan = {
  holdMs: 60_000,
  levels: [1, 2, 4],
  transitionMs: 10_000,
  vusPerLevel: { changesReaders: 2, writers: 1, wsReaders: 3 },
}

const criteria: RampCriteria = {
  contentChecksPerLevel: 2,
  minimumDeliveryRatio: 0.9,
  minimumSuccessRate: 0.99,
  p95LimitMs: null,
}

const SAMPLES = 100

type LevelSample = {
  changesDelivery?: number | undefined
  changesReaders?: number | undefined
  content?: { verified?: number, mismatch?: number, denied?: number, malformed?: number, unverified?: number }
    | undefined
  changesP95?: number | undefined
  changesSuccess?: number | undefined
  uploadP95?: number | undefined
  uploadSuccess?: number | undefined
  wsAuth?: number | undefined
  wsDelivery?: number | undefined
  wsHandshake?: number | undefined
  wsReaders?: number | undefined
}

function rate(value: number | undefined): { values: Record<string, number> } {
  if (value === undefined) {
    return { values: { fails: 0, passes: 0, rate: 0 } }
  }

  const passes = Math.round(value * SAMPLES)
  return { values: { fails: SAMPLES - passes, passes, rate: value } }
}

// k6 keeps an empty placeholder submetric with a p95 of 0 and a count of 0.
function trend(value: number | undefined): { values: Record<string, number> } {
  return value === undefined
    ? { values: { count: 0, 'p(95)': 0 } }
    : { values: { count: 10, 'p(95)': value } }
}

function counter(value: number): { values: Record<string, number> } {
  return { values: { count: value } }
}

// Delivery ratios are the worst reader's share of the level's uploads; every expected reader reports
// unless a sample says otherwise. Totals follow the same ratio.
function makeMetrics(samples: Record<number, LevelSample>): SummaryMetrics {
  const metrics: SummaryMetrics = { cleanup_success: rate(1) }

  for (const [rawLevel, sample] of Object.entries(samples)) {
    const level = Number(rawLevel)
    const uploads = Math.round((sample.uploadSuccess ?? 0) * SAMPLES)
    const name = (metric: string, tags: Record<string, string> = {}): string =>
      rampLevelMetricName(metric, level, tags)

    metrics[name('shared_drive_ramp_hold_completed')] = counter(1)
    metrics[name('shared_drive_upload_success_rate')] = rate(sample.uploadSuccess)
    metrics[name('shared_drive_changes_success_rate')] = rate(sample.changesSuccess)
    metrics[name('shared_drive_ws_handshake_rate')] = rate(sample.wsHandshake)
    metrics[name('shared_drive_ws_auth_rate')] = rate(sample.wsAuth)
    metrics[name('http_req_duration', { operation: 'file-upload' })] = trend(
      sample.uploadP95,
    )
    metrics[name('shared_drive_changes_duration_ms')] = trend(sample.changesP95)
    metrics[name('shared_drive_ws_events_total', { event: 'CREATED' })] = counter(
      Math.round(
        (sample.wsDelivery ?? 1) * uploads * level * plan.vusPerLevel.wsReaders,
      ),
    )
    metrics[name('shared_drive_changes_items_total')] = counter(
      Math.round(
        (sample.changesDelivery ?? 1) *
          uploads *
          level *
          plan.vusPerLevel.changesReaders,
      ),
    )
    const readers = (ratio: number | undefined, count: number) =>
      ({ values: { count, min: Math.round((ratio ?? 1) * uploads) } })
    metrics[name('shared_drive_ws_reader_unique')] = readers(sample.wsDelivery,
      sample.wsReaders ?? level * plan.vusPerLevel.wsReaders)
    metrics[name('shared_drive_changes_reader_unique')] = readers(sample.changesDelivery,
      sample.changesReaders ?? level * plan.vusPerLevel.changesReaders)
    const { verified = 2, mismatch = 0, denied = 0, malformed = 0, unverified = 0 } = sample.content ?? {}
    metrics[name('shared_drive_content_verified_total')] = counter(verified)
    metrics[name('shared_drive_content_mismatch_total')] = counter(mismatch)
    metrics[name('shared_drive_content_denied_total')] = counter(denied)
    metrics[name('shared_drive_changes_malformed_total')] = counter(malformed)
    metrics[name('shared_drive_content_unverified_total')] = counter(unverified)
  }

  return metrics
}

const healthy: LevelSample = {
  changesP95: 50,
  changesSuccess: 1,
  uploadP95: 100,
  uploadSuccess: 1,
  wsAuth: 1,
  wsHandshake: 1,
}

describe('parseRampPlan', (): void => {
  it('uses the documented defaults', (): void => {
    deepEqual(parseRampPlan({}), {
      holdMs: 60_000,
      levels: [1, 2, 4, 8, 16],
      transitionMs: 10_000,
      vusPerLevel: { changesReaders: 2, writers: 1, wsReaders: 3 },
    })
  })

  it('reads levels, hold duration, and VUs per level', (): void => {
    deepEqual(
      parseRampPlan({
        RAMP_CHANGES_READERS_PER_LEVEL: '1',
        CAPACITY_START: '2',
        CAPACITY_GROWTH: '3',
        CAPACITY_MAX: '6',
        CAPACITY_HOLD: '1m30s',
        RAMP_WRITERS_PER_LEVEL: '2',
        RAMP_WS_READERS_PER_LEVEL: '4',
      }),
      {
        holdMs: 90_000,
        levels: [2, 6],
        transitionMs: 10_000,
        vusPerLevel: { changesReaders: 1, writers: 2, wsReaders: 4 },
      },
    )
  })

  it('rejects hold durations too short to measure a level', (): void => {
    throws(
      () => parseRampPlan({ CAPACITY_HOLD: '4s' }),
      /CAPACITY_HOLD must be at least 5s/,
    )
  })
})

describe('parseRampCriteria', (): void => {
  it('judges no latency by default', (): void => {
    deepEqual(parseRampCriteria({}), criteria)
  })

  it('reads explicit values', (): void => {
    deepEqual(
      parseRampCriteria({
        MIN_DELIVERY_RATIO: '0.8',
        MIN_SUCCESS_RATE: '0.95',
        P95_LIMIT_MS: '500',
        CONTENT_CHECKS_PER_LEVEL: '0',
      }),
      { contentChecksPerLevel: 0, minimumDeliveryRatio: 0.8, minimumSuccessRate: 0.95, p95LimitMs: 500 },
    )
  })

  it('rejects invalid values', (): void => {
    throws(() => parseRampCriteria({ MIN_SUCCESS_RATE: '0' }), /MIN_SUCCESS_RATE/)
    throws(() => parseRampCriteria({ MIN_SUCCESS_RATE: '1.5' }), /MIN_SUCCESS_RATE/)
    throws(() => parseRampCriteria({ MIN_DELIVERY_RATIO: '0' }), /MIN_DELIVERY_RATIO/)
    throws(() => parseRampCriteria({ MIN_DELIVERY_RATIO: '2' }), /MIN_DELIVERY_RATIO/)
    throws(() => parseRampCriteria({ P95_LIMIT_MS: '-1' }), /P95_LIMIT_MS/)
    throws(() => parseRampCriteria({ CONTENT_CHECKS_PER_LEVEL: '1.5' }), /CONTENT_CHECKS_PER_LEVEL/)
  })
})

describe('ramp schedule', (): void => {
  it('completes holds only at their scenario-relative end, including the final hold', (): void => {
    deepEqual(getCompletedRampLevels(plan, 69_999), [])
    deepEqual(getCompletedRampLevels(plan, 70_000), [1])
    deepEqual(getCompletedRampLevels(plan, 139_999), [1])
    deepEqual(getCompletedRampLevels(plan, 140_000), [1, 2])
    deepEqual(getCompletedRampLevels(plan, 209_999), [1, 2])
    deepEqual(getCompletedRampLevels(plan, 210_000), [1, 2, 4])
    deepEqual(getCompletedRampLevels(plan, 250_000), [1, 2, 4])
    deepEqual(getCompletedRampLevels(plan, Number.NaN), [])
    deepEqual(getCompletedRampLevels(plan, Number.POSITIVE_INFINITY), [])
  })

  it('starts every VU in the first half of the transition, then holds', (): void => {
    deepEqual(buildRampingVuStages(plan, 3), [
      { duration: '5000ms', target: 3 },
      { duration: '5000ms', target: 3 },
      { duration: '60000ms', target: 3 },
      { duration: '5000ms', target: 6 },
      { duration: '5000ms', target: 6 },
      { duration: '60000ms', target: 6 },
      { duration: '5000ms', target: 12 },
      { duration: '5000ms', target: 12 },
      { duration: '60000ms', target: 12 },
    ])
    equal(getRampTotalDurationMs(plan), 210_000)
  })

  it('lets readers drain the feed after the last hold', (): void => {
    deepEqual(buildRampingVuStages({ ...plan, levels: [1] }, 2, { drain: true }), [
      { duration: '5000ms', target: 2 },
      { duration: '5000ms', target: 2 },
      { duration: '60000ms', target: 2 },
      { duration: '10000ms', target: 2 },
    ])
  })

  it('maps elapsed time to the held level or a transition', (): void => {
    deepEqual(getRampPhaseAt(plan, 0), { endsAtMs: 10_000, level: null })
    deepEqual(getRampPhaseAt(plan, 10_000), { endsAtMs: 70_000, level: 1 })
    deepEqual(getRampPhaseAt(plan, 69_999), { endsAtMs: 70_000, level: 1 })
    deepEqual(getRampPhaseAt(plan, 75_000), { endsAtMs: 80_000, level: null })
    deepEqual(getRampPhaseAt(plan, 150_000), { endsAtMs: 210_000, level: 4 })
    deepEqual(getRampPhaseAt(plan, 250_000), { endsAtMs: 210_000, level: null })
  })
})

describe('getServedRampPhase', (): void => {
  it('serves the held level during a hold', (): void => {
    deepEqual(getServedRampPhase(plan, 30_000), { endsAtMs: 70_000, level: 1 })
  })

  it('serves the upcoming level, until its hold ends, during a transition', (): void => {
    deepEqual(getServedRampPhase(plan, 0), { endsAtMs: 70_000, level: 1 })
    deepEqual(getServedRampPhase(plan, 72_000), { endsAtMs: 140_000, level: 2 })
  })

  it('serves nothing after the last hold', (): void => {
    deepEqual(getServedRampPhase(plan, 210_000), { endsAtMs: 210_000, level: null })
  })
})

describe('ramp upload names', (): void => {
  it('names an upload after the held level or the transition', (): void => {
    equal(getRampUploadPrefix({ endsAtMs: 70_000, level: 4 }), 'sd-L4-')
    equal(getRampUploadPrefix({ endsAtMs: 10_000, level: null }), 'sd-transition-')
  })

  it('reads the level back from a file name', (): void => {
    equal(parseRampUploadLevel('sd-L16-vu3-iter9-x.bin'), '16')
    equal(parseRampUploadLevel('sd-transition-vu3-iter9-x.bin'), 'unattributed')
    equal(parseRampUploadLevel('other.bin'), 'unattributed')
    equal(parseRampUploadLevel(undefined), 'unattributed')
  })
})

describe('buildRampThresholds', (): void => {
  it('declares a submetric per level for every judged metric', (): void => {
    const thresholds = buildRampThresholds({ ...plan, levels: [1] })

    deepEqual(Object.keys(thresholds).sort(), [
      'cleanup_success',
      'http_req_duration{operation:file-upload,level:1}',
      'http_req_failed',
      'shared_drive_changes_duration_ms{level:1}',
      'shared_drive_changes_items_total{level:1}',
      'shared_drive_changes_malformed_total{level:1}',
      'shared_drive_changes_reader_unique{level:1}',
      'shared_drive_changes_success_rate{level:1}',
      'shared_drive_content_denied_total{level:1}',
      'shared_drive_content_mismatch_total{level:1}',
      'shared_drive_content_unverified_total{level:1}',
      'shared_drive_content_verified_total{level:1}',
      'shared_drive_ramp_hold_completed{level:1}',
      'shared_drive_upload_success_rate{level:1}',
      'shared_drive_ws_auth_rate{level:1}',
      'shared_drive_ws_events_total{event:CREATED,level:1}',
      'shared_drive_ws_handshake_rate{level:1}',
      'shared_drive_ws_reader_unique{level:1}',
    ])
    deepEqual(thresholds['shared_drive_ws_reader_unique{level:1}'], ['min>=0'])
    deepEqual(thresholds.http_req_failed, [
      { abortOnFail: true, delayAbortEval: '30s', threshold: 'rate<0.5' },
    ])
  })
})

describe('evaluateRampLevels', (): void => {
  it('invalidates capacity when cleanup failed or was never measured', (): void => {
    for (const cleanup of [rate(0), rate(undefined), undefined]) {
      const metrics = makeMetrics({ 1: healthy, 2: healthy, 4: healthy })
      metrics.cleanup_success = cleanup
      const report = evaluateRampLevels(metrics, plan, criteria)

      equal(report.status, 'infrastructure_failure')
      equal(report.result_bound, 'inconclusive')
      equal(report.max_sustained_level, null)
      equal(report.cleanup_success_rate, cleanup?.values.rate === 0 && (cleanup.values.fails ?? 0) > 0 ? 0 : null)
      match(report.failure_reason ?? '', /cleanup/)
      match(formatRampReport(report), /Capacity result invalid:.*cleanup/)
      equal(formatRampReport(report).includes('Maximum sustained level:'), false)
      deepEqual(report.levels.map(level => level.status), ['passed', 'passed', 'passed'])
    }
  })

  it('does not sustain a sampled partial hold without an explicit completion signal', (): void => {
    const metrics = makeMetrics({ 1: healthy, 2: healthy, 4: healthy })
    delete metrics[rampLevelMetricName('shared_drive_ramp_hold_completed', 4)]
    const report = evaluateRampLevels(metrics, plan, criteria)

    equal(report.max_sustained_level, 2)
    equal(report.result_bound, 'inconclusive')
    equal(report.levels[2]?.status, 'not_measured')
    deepEqual(report.levels[2]?.reasons, ['hold did not complete'])
    equal(report.levels[2]?.hold_completed, false)
    equal(report.levels[1]?.hold_completed, true)
  })

  it('keeps an incomplete failed sample inconclusive and retains its diagnostics', (): void => {
    const metrics = makeMetrics({ 1: healthy, 2: { ...healthy, wsAuth: 0.5 } })
    metrics[rampLevelMetricName('shared_drive_ramp_hold_completed', 2)] = counter(0)
    const report = evaluateRampLevels(metrics, plan, criteria)

    equal(report.levels[1]?.status, 'not_measured')
    equal(report.result_bound, 'inconclusive')
    equal(report.first_failed_level, null)
    deepEqual(report.levels[1]?.reasons, ['hold did not complete', 'ws auth rate 0.5 is below 0.99'])
  })

  it('reports a lower bound when every level passes', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: healthy, 2: healthy, 4: healthy }),
      plan,
      criteria,
    )

    equal(report.max_sustained_level, 4)
    equal(report.result_bound, 'lower_bound')
    deepEqual(
      report.levels.map(({ status }) => status),
      ['passed', 'passed', 'passed'],
    )
    equal(report.criteria.p95_limit_mode, 'none')
    equal(report.levels[2]?.uploads, 100)
    equal(report.levels[2]?.ws_events, 1_200)
    equal(report.levels[2]?.ws_delivery_ratio, 1)
  })

  it('reports a sparse passing/failing bracket rather than an exact maximum', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 8: healthy, 16: { ...healthy, wsAuth: 0.5 } }),
      { ...plan, levels: [8, 16] },
      criteria,
    )

    equal(report.max_sustained_level, 8)
    equal(report.first_failed_level, 16)
    equal(report.result_bound, 'bracket')
    match(formatRampReport(report), /Sustained level bracket: \[8, 16\)/)
    equal(formatRampReport(report).includes('Maximum sustained level: 8'), false)
  })

  it('stops at the first level whose success rate is too low', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: healthy, 2: { ...healthy, wsAuth: 0.5 }, 4: healthy }),
      plan,
      criteria,
    )

    equal(report.max_sustained_level, 1)
    equal(report.result_bound, 'bracket')
    deepEqual(report.levels[1]?.reasons, ['ws auth rate 0.5 is below 0.99'])
  })

  it('fails a level whose WebSocket readers missed events', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: healthy, 2: { ...healthy, wsDelivery: 0.5 } }),
      plan,
      criteria,
    )

    equal(report.levels[1]?.status, 'failed')
    deepEqual(report.levels[1]?.reasons, [
      'ws event delivery 0.5 is below 0.9',
    ])
  })

  it('fails a level whose changes readers missed changes', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: { ...healthy, changesDelivery: 0.8 } }),
      { ...plan, levels: [1] },
      criteria,
    )

    deepEqual(report.levels[0]?.reasons, ['changes delivery 0.8 is below 0.9'])
  })

  it('accepts more changes than uploads', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: { ...healthy, changesDelivery: 1.5 } }),
      { ...plan, levels: [1] },
      criteria,
    )

    equal(report.levels[0]?.status, 'passed')
  })

  it('records the p95 without judging it when no limit is configured', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: healthy, 2: { ...healthy, uploadP95: 10_000 } }),
      plan,
      criteria,
    )

    equal(report.levels[1]?.status, 'passed')
    equal(report.levels[1]?.upload_p95_ms, 10_000)
  })

  it('fails a level above a configured p95 limit', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: { ...healthy, uploadP95: 400 } }),
      { ...plan, levels: [1] },
      { ...criteria, p95LimitMs: 300 },
    )

    equal(report.levels[0]?.status, 'failed')
    deepEqual(report.levels[0]?.reasons, ['upload p95 400 ms exceeds 300 ms'])
    equal(report.criteria.p95_limit_ms, 300)
    equal(report.criteria.p95_limit_mode, 'absolute')
  })

  it('treats a p95 without samples as missing when a limit is configured', (): void => {
    const metrics = makeMetrics({ 1: healthy })
    metrics[rampLevelMetricName('shared_drive_changes_duration_ms', 1)] = {
      values: { 'p(95)': 0 },
    }

    const report = evaluateRampLevels(
      metrics,
      { ...plan, levels: [1] },
      { ...criteria, p95LimitMs: 1_000 },
    )

    equal(report.levels[0]?.status, 'not_measured')
    deepEqual(report.levels[0]?.reasons, ['changes p95 has no samples'])
  })

  it('ignores a missing p95 when no limit is configured', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: { ...healthy, uploadP95: undefined } }),
      { ...plan, levels: [1] },
      criteria,
    )

    equal(report.levels[0]?.status, 'passed')
    equal(report.levels[0]?.upload_p95_ms, null)
  })

  it('never counts a level with missing success samples as passed', (): void => {
    const report = evaluateRampLevels(
      makeMetrics({ 1: healthy, 2: { ...healthy, wsAuth: undefined } }),
      plan,
      criteria,
    )

    equal(report.levels[1]?.status, 'not_measured')
    deepEqual(report.levels[1]?.reasons, ['ws auth rate has no samples'])
    equal(report.max_sustained_level, 1)
    equal(report.result_bound, 'inconclusive')
  })

  it('cannot judge delivery when no reader reported', (): void => {
    const metrics = makeMetrics({ 1: healthy })
    delete metrics[rampLevelMetricName('shared_drive_ws_reader_unique', 1)]

    const report = evaluateRampLevels(metrics, { ...plan, levels: [1] }, criteria)

    equal(report.levels[0]?.status, 'not_measured')
    deepEqual(report.levels[0]?.reasons, ['ws event delivery has no samples'])
  })

  it('a missing reader fails the level even when the others saw everything', (): void => {
    const report = evaluateRampLevels(makeMetrics({ 1: healthy, 2: { ...healthy, wsReaders: 5 } }), plan, criteria)
    equal(report.levels[1]?.status, 'failed')
    deepEqual(report.levels[1]?.reasons, ['ws event delivery: 5 of 6 readers reported'])
    equal(report.levels[1]?.ws_readers, 5)
  })

  it('repeated events cannot hide one reader missing uploads', (): void => {
    const metrics = makeMetrics({ 1: { ...healthy, changesDelivery: 0.5 } })
    // Duplicates push the total above what every reader should have seen.
    metrics[rampLevelMetricName('shared_drive_changes_items_total', 1)] = counter(SAMPLES * 2 * 3)
    const report = evaluateRampLevels(metrics, { ...plan, levels: [1] }, criteria)
    deepEqual([report.levels[0]?.status, report.levels[0]?.reasons], ['failed', ['changes delivery 0.5 is below 0.9']])
    equal(report.levels[0]?.changes_items, SAMPLES * 6)
  })

  it('wrong content or a refused download fails the level and the whole ramp', (): void => {
    for (const [content, reason] of [[{ mismatch: 1 }, '1 uploads read back different content'],
      [{ denied: 2 }, '2 reader downloads were denied']] as const) {
      const report = evaluateRampLevels(makeMetrics({ 1: healthy, 2: { ...healthy, content } }), plan, criteria)
      deepEqual([report.levels[1]?.status, report.levels[1]?.reasons], ['failed', [reason]])
      deepEqual(report.correctness_failures, [`level 2: ${reason}`])
    }
    const clean = evaluateRampLevels(makeMetrics({ 1: { ...healthy, content: { malformed: 3 } } }),
      { ...plan, levels: [1] }, criteria)
    deepEqual([clean.correctness_failures, clean.levels[0]?.content_verified, clean.levels[0]?.changes_malformed],
      [[], 2, 3])
    equal(clean.criteria.content_checks_per_level, 2)
  })

  it('wrong content outside every held level still fails the ramp', (): void => {
    const metrics = makeMetrics({ 1: healthy })
    // The level saw none; a writer's upload during a transition read back wrong.
    metrics.shared_drive_content_mismatch_total = counter(1)
    metrics.shared_drive_content_denied_total = counter(0)
    const report = evaluateRampLevels(metrics, { ...plan, levels: [1] }, criteria)
    deepEqual(report.correctness_failures, ['outside the held levels: 1 uploads read back different content'])
    equal(report.levels[0]?.status, 'passed')
  })

  it('a content check that read nothing, or no check at all, keeps the level from passing', (): void => {
    const levels = { ...plan, levels: [1] }
    const unread = evaluateRampLevels(makeMetrics({ 1: { ...healthy, content: { verified: 1, unverified: 1 } } }),
      levels, criteria)
    deepEqual([unread.levels[0]?.status, unread.levels[0]?.reasons, unread.correctness_failures],
      ['not_measured', ['1 content checks could not read the file'], []])
    const none = makeMetrics({ 1: { ...healthy, content: { verified: 0 } } })
    deepEqual(evaluateRampLevels(none, levels, criteria).levels[0]?.reasons, ['no upload content was verified'])
    equal(evaluateRampLevels(none, levels, { ...criteria, contentChecksPerLevel: 0 }).levels[0]?.status, 'passed')
  })

  it('marks levels the run never reached', (): void => {
    const report = evaluateRampLevels(makeMetrics({ 1: healthy }), plan, criteria)

    deepEqual(
      report.levels.map(({ status }) => status),
      ['passed', 'not_run', 'not_run'],
    )
    equal(report.result_bound, 'inconclusive')
  })
})

describe('formatRampReport', (): void => {
  it('prints one line per level and the result', (): void => {
    const text = formatRampReport(
      evaluateRampLevels(
        makeMetrics({ 1: healthy, 2: { ...healthy, uploadSuccess: 0.9 } }),
        plan,
        criteria,
      ),
    )

    match(text, /level 1: passed/)
    match(text, /level 2: failed \(upload success rate 0\.9 is below 0\.99\)/)
    match(text, /level 4: not_run/)
    match(text, /Sustained level bracket: \[1, 2\)/)
  })
})
