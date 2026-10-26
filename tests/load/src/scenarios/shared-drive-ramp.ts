import type { Options } from 'k6/options'

import {
  buildRampThresholds,
  buildRampingVuStages,
  evaluateRampLevels,
  formatRampReport,
  parseRampCriteria,
  parseRampPlan,
  type SummaryMetrics,
} from '../lib/load-profile.ts'
import { actorHosts, createSetupData, type SetupData } from './shared-drive-activity.ts'

export {
  changesReaderLoop,
  teardown,
  writerLoop,
  wsReaderLoop,
} from './shared-drive-activity.ts'

// Runs the shared-drive activity mix at increasing levels in one k6 run and
// reports completed passing/failing bounds, never a capacity from a partial
// hold or failed cleanup. Explicit hold signals exclude setup/teardown time.
// k6 owns the ramp; no Node coordinator is involved.

const rampPlan = parseRampPlan(__ENV)
const rampCriteria = parseRampCriteria(__ENV)
const executionId = __ENV.EXECUTION_ID || 'shared-drive-ramp'

export const options: Options = {
  hosts: actorHosts,
  scenarios: {
    writers: {
      executor: 'ramping-vus',
      exec: 'writerLoop',
      stages: buildRampingVuStages(rampPlan, rampPlan.vusPerLevel.writers),
      startVUs: 0,
    },
    ws_readers: {
      executor: 'ramping-vus',
      exec: 'wsReaderLoop',
      stages: buildRampingVuStages(rampPlan, rampPlan.vusPerLevel.wsReaders),
      startVUs: 0,
    },
    changes_readers: {
      executor: 'ramping-vus',
      exec: 'changesReaderLoop',
      stages: buildRampingVuStages(
        rampPlan,
        rampPlan.vusPerLevel.changesReaders,
        { drain: true },
      ),
      startVUs: 0,
    },
  },
  // `count` lets handleSummary() tell an empty level from a p95 of 0.
  summaryTrendStats: [
    'avg',
    'min',
    'med',
    'max',
    'p(90)',
    'p(95)',
    'p(99)',
    'count',
  ],
  thresholds: buildRampThresholds(rampPlan),
}

export function setup(): SetupData {
  return createSetupData(rampPlan)
}

export function handleSummary(data: {
  metrics: SummaryMetrics
}): Record<string, string> {
  const report = evaluateRampLevels(data.metrics, rampPlan, rampCriteria)

  return {
    stdout: formatRampReport(report),
    [`/results/${executionId}-shared-drive-ramp.json`]: `${JSON.stringify(report, null, 2)}\n`,
  }
}
