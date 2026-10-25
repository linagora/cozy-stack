import { deepEqual, equal, match } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  buildGrafanaLink,
  buildWindowLink,
  campaignOutcome,
  classifyStep,
  describeStepResult,
  formatCampaignSummary,
  parseEnvironment,
  parseSteps,
  type CampaignStep,
} from './campaign-summary.ts'

const uploadStep: CampaignStep = {
  endedAt: '2026-10-18T21:20:00Z',
  exitCode: 0,
  id: 'run-upload-1m',
  kind: 'upload-capacity',
  name: 'upload-capacity-1M',
  producerExitCode: null,
  startedAt: '2026-10-18T21:10:00Z',
}

describe('parseSteps', (): void => {
  it('reads one step per tab-separated line and ignores blank lines', (): void => {
    deepEqual(
      parseSteps(
        'upload-capacity-1M\tupload-capacity\trun-upload-1m\t2026-10-18T21:10:00Z\t2026-10-18T21:20:00Z\t0\n\n',
      ),
      [uploadStep],
    )
  })

  it('reads the producer exit status, kept apart from the exit make reports', (): void => {
    deepEqual(
      parseSteps('upload-capacity-1M\tupload-capacity\trun-upload-1m\t2026-10-18T21:10:00Z\t2026-10-18T21:20:00Z\t2\t1\n'),
      [{ ...uploadStep, exitCode: 2, producerExitCode: 1 }],
    )
  })
})

describe('parseEnvironment', (): void => {
  it('reads key=value lines and keeps values containing =', (): void => {
    deepEqual(parseEnvironment('commit=abc123\nlimits=cpus=2\n# comment\n'), {
      commit: 'abc123',
      limits: 'cpus=2',
    })
  })
})

describe('describeStepResult', (): void => {
  it('reports an exact upload capacity', (): void => {
    equal(
      describeStepResult(uploadStep, {
        maximum_concurrent_uploads: 48,
        result_bound: 'exact',
        status: 'completed',
      }),
      'maximum concurrent uploads = 48',
    )
  })

  it('reports a lower bound with the failing reason absent', (): void => {
    equal(
      describeStepResult(uploadStep, {
        maximum_concurrent_uploads: 1024,
        result_bound: 'lower_bound',
        status: 'completed',
      }),
      'maximum concurrent uploads ≥ 1024 (cap reached)',
    )
  })

  it('reports why a capacity campaign stopped', (): void => {
    equal(
      describeStepResult(uploadStep, {
        failure_reason: 'baseline failed because of test infrastructure',
        status: 'infrastructure_failure',
      }),
      'stopped: baseline failed because of test infrastructure',
    )
  })

  it('reports a stopped ramp when cleanup invalidated otherwise passing samples', (): void => {
    equal(describeStepResult({ ...uploadStep, kind: 'shared-drive-ramp', exitCode: 99 }, {
      failure_reason: 'test-directory cleanup failed',
      levels: [{ level: 8, reasons: [], status: 'passed' }],
      max_sustained_level: null,
      result_bound: 'inconclusive',
      status: 'infrastructure_failure',
    }), 'stopped: test-directory cleanup failed')
  })

  it('does not claim a sparse ramp maximum in campaign summaries', (): void => {
    equal(describeStepResult({ ...uploadStep, kind: 'shared-drive-ramp' }, {
      first_failed_level: 16,
      levels: [{ level: 8, reasons: [], status: 'passed' },
        { level: 16, reasons: ['upload success rate is too low'], status: 'failed' }],
      max_sustained_level: 8,
      result_bound: 'bracket',
      status: 'completed',
    }), 'sustained level bracket [8, 16); level 16 failed: upload success rate is too low')
  })

  it('labels a partial hold inconclusive rather than an exact capacity', (): void => {
    equal(describeStepResult({ ...uploadStep, kind: 'shared-drive-ramp', exitCode: 108 }, {
      levels: [{ level: 8, reasons: [], status: 'passed' },
        { level: 16, reasons: ['hold did not complete'], status: 'not_measured' }],
      max_sustained_level: 8,
      result_bound: 'inconclusive',
    }), 'inconclusive; highest completed passing level 8; level 16 not_measured: hold did not complete')
  })

  it('reports the sustained ramp level and the first failing reasons', (): void => {
    equal(
      describeStepResult(
        { ...uploadStep, kind: 'shared-drive-ramp' },
        {
          levels: [
            { level: 1, reasons: [], status: 'passed' },
            { level: 2, reasons: ['ws auth rate 0.5 is below 0.99'], status: 'failed' },
          ],
          max_sustained_level: 1,
          result_bound: 'exact',
        },
      ),
      'sustained level bracket [1, 2); level 2 failed: ws auth rate 0.5 is below 0.99',
    )
  })

  it('reports a ramp where every level passed', (): void => {
    equal(
      describeStepResult(
        { ...uploadStep, kind: 'shared-drive-ramp' },
        { levels: [], max_sustained_level: 64, result_bound: 'lower_bound' },
      ),
      'maximum sustained level ≥ 64 (every level passed)',
    )
  })

  it('reports a missing result file through the exit code', (): void => {
    equal(describeStepResult({ ...uploadStep, exitCode: 2 }, null), 'no result file (exit 2)')
    equal(
      describeStepResult({ ...uploadStep, kind: 'prepare', id: '' }, null),
      'done',
    )
  })
})

describe('fanout campaign results', (): void => {
  const step = { ...uploadStep, kind: 'org-drive-fanout' }

  it('reports all-recipient verified lower bounds rather than upload capacity', (): void => {
    equal(describeStepResult(step, { status: 'passed',
      capacity: { kind: 'lower_bound', lower: 50, upper_exclusive: null, max: null } }),
    'member batch ≥ 50 recipients (ready, access and revocation verified)')
  })

  it('keeps uncertain fanout results inconclusive with separately named observation counts', (): void => {
    equal(describeStepResult(step, { status: 'inconclusive',
      capacity: { kind: 'inconclusive', lower: null },
      points: [{ n: 75, owner: { add: { ready_recipients: 72 } },
        recipients: [{ add: { converged: true } }, { add: { converged: false } }] }] }),
    'inconclusive; batch 75; owner ready 72; recipient access proofs 1')
  })

  it('links native overview without losing the explicit campaign identifier', (): void => {
    match(buildGrafanaLink('http://grafana.test', step, 'parent-campaign') ?? '',
      /\/d\/twake-drive-capacity\/stack-capacity-overview\?var-campaign=parent-campaign&/)
  })
})

describe('buildGrafanaLink', (): void => {
  it('opens the matching dashboard on the step campaign and time range', (): void => {
    equal(
      buildGrafanaLink('http://grafana.test/', uploadStep),
      'http://grafana.test/d/twake-drive-k6/twake-drive-k6-overview?var-campaignid=run-upload-1m&from=1792357740000&to=1792358460000',
    )
    match(
      buildGrafanaLink('http://grafana.test', { ...uploadStep, kind: 'shared-drive-ramp' }) ?? '',
      /\/d\/twake-drive-shared-drive\/twake-drive-shared-drive-activity\?var-campaignid=run-upload-1m&/,
    )
  })

  it('has no link for steps without a campaign', (): void => {
    equal(buildGrafanaLink('http://grafana.test', { ...uploadStep, id: '', kind: 'prepare' }), null)
  })
})

describe('formatCampaignSummary', (): void => {
  it('lists the context and one row per step', (): void => {
    const text = formatCampaignSummary({
      environment: { commit: 'abc123', limits: 'cozy-stack cpus=2 mem=4g', topology: 'single-host' },
      grafanaUrl: 'http://grafana.test',
      name: 'quick',
      results: new Map([['run-upload-1m', { attempts: [{ cleanup_rate: 1, consistency_rate: 1 }], maximum_concurrent_uploads: 48, result_bound: 'exact', status: 'completed' }]]),
      runId: '20261018T211000Z',
      steps: [uploadStep, { ...uploadStep, exitCode: 0, id: '', kind: 'prepare', name: 'shared-drive-prepare' }],
    })

    match(text, /^# Campaign quick \(20261018T211000Z\)/)
    match(text, /- Topology: single-host/)
    match(text, /- Commit: abc123/)
    match(text, /- Limits: cozy-stack cpus=2 mem=4g/)
    match(text, /- Outcome: 0 \(completed\)/)
    match(text, /\| upload-capacity-1M \| maximum concurrent uploads = 48 \| 0 \| \[Grafana\]\(http:\/\/grafana\.test\/d\/twake-drive-k6\//)
    match(text, /\| shared-drive-prepare \| done \| 0 \|/)
  })

  it('reports an interrupted campaign inconclusive with each step reason', (): void => {
    const text = formatCampaignSummary({
      environment: { interrupted: 'TERM' },
      grafanaUrl: 'http://grafana.test',
      name: 'quick',
      results: new Map(),
      runId: '20261018T211000Z',
      steps: [{ ...uploadStep, exitCode: 2, producerExitCode: 143 }],
    })

    match(text, /- Outcome: 2 \(inconclusive\)\n- Interrupted by SIGTERM/)
    match(text, /\| upload-capacity-1M \| no result file \(exit 2\) \| 2: no result file \(exit 143\) \|/)
  })
})

describe('buildWindowLink', (): void => {
  it('narrows the shared DEV dashboard to each step, in the summary too', (): void => {
    const dev = 'https://grafana.dev.test/d/stack?orgId=1'
    const window = `from=${Date.parse(uploadStep.startedAt)}&to=${Date.parse(uploadStep.endedAt)}`
    equal(buildWindowLink(dev, uploadStep), `${dev}&${window}`)
    equal(buildWindowLink('not a url', uploadStep), null)
    equal(buildWindowLink(dev, { ...uploadStep, endedAt: '' }), null)
    const text = formatCampaignSummary({
      devMonitoringUrl: dev, environment: {}, grafanaUrl: 'http://grafana.test', name: 'dev-capacity',
      results: new Map(), runId: 'run', steps: [uploadStep],
    })
    match(text, new RegExp(`\\[DEV\\]\\(${dev.replace(/[.?]/g, '\\$&')}&${window}\\) \\|$`, 'm'))
  })
})

describe('classifyStep', (): void => {
  const ramp: CampaignStep = { ...uploadStep, kind: 'shared-drive-ramp', producerExitCode: 0 }
  const fanout: CampaignStep = { ...uploadStep, kind: 'org-drive-fanout', producerExitCode: 0 }
  const upload: CampaignStep = { ...uploadStep, producerExitCode: 0 }
  const completedUpload = { attempts: [{ cleanup_rate: 1, consistency_rate: 1 }], maximum_concurrent_uploads: 48, result_bound: 'exact', status: 'completed' }
  const rampBracket = { max_sustained_level: 8, result_bound: 'bracket', status: 'completed' }
  const fanoutBracket = { capacity: { kind: 'bracket', lower: 2, upper_exclusive: 4 }, status: 'failed' }

  it('accepts a completed capacity for every producer, a bracket included', (): void => {
    deepEqual(classifyStep(upload, completedUpload), { code: 0, reason: 'completed' })
    deepEqual(classifyStep(ramp, rampBracket), { code: 0, reason: 'completed' })
    deepEqual(classifyStep(ramp, { ...rampBracket, result_bound: 'lower_bound' }), { code: 0, reason: 'completed' })
    deepEqual(classifyStep(fanout, fanoutBracket), { code: 0, reason: 'completed' })
    deepEqual(classifyStep(fanout, { capacity: { kind: 'lower_bound', lower: 16 }, status: 'passed' }),
      { code: 0, reason: 'completed' })
  })

  it('leaves a completed report without its evidence inconclusive', (): void => {
    const invalid = { code: 2, reason: 'completed report lacks its capacity or verification evidence' }
    deepEqual(classifyStep(upload, { status: 'completed' }), invalid)
    deepEqual(classifyStep(upload, { ...completedUpload, attempts: [{ cleanup_rate: 0, consistency_rate: 1 }] }), invalid)
    deepEqual(classifyStep(upload, { ...completedUpload, result_bound: 'bracket' }), invalid)
    deepEqual(classifyStep(ramp, { result_bound: 'lower_bound', status: 'completed' }),
      { code: 2, reason: 'ramp report has no sustained level' })
  })

  it('fails a fanout that read different bytes even after a measured bracket', (): void => {
    deepEqual(classifyStep({ ...fanout, exitCode: 2, producerExitCode: 1 },
      { ...fanoutBracket, content_mismatch: true, error: 'Recipient read different marker bytes' }),
    { code: 1, reason: 'Recipient read different marker bytes' })
  })

  it('accepts a ramp that k6 stopped on mostly failed requests once cleanup is verified', (): void => {
    deepEqual(classifyStep({ ...ramp, exitCode: 2, producerExitCode: 99 }, rampBracket), { code: 0, reason: 'completed' })
  })

  it('does not let a completed report explain an unexpected producer exit', (): void => {
    deepEqual(classifyStep({ ...ramp, exitCode: 2, producerExitCode: 107 }, rampBracket),
      { code: 2, reason: 'exit 107 although the report completed' })
    deepEqual(classifyStep({ ...upload, exitCode: 2, producerExitCode: 99 }, completedUpload),
      { code: 2, reason: 'exit 99 although the report completed' })
    deepEqual(classifyStep({ ...fanout, exitCode: 2, producerExitCode: 1 }, fanoutBracket),
      { code: 2, reason: 'exit 1 although the report completed' })
    deepEqual(classifyStep({ ...ramp, exitCode: 2, producerExitCode: null }, rampBracket),
      { code: 2, reason: 'exit 2 although the report completed' })
  })

  it('reports a content mismatch as a correctness failure even inside a completed search', (): void => {
    deepEqual(classifyStep({ ...upload, exitCode: 2, producerExitCode: 1 },
      { ...completedUpload, attempts: [{ consistency_rate: 1 }, { consistency_rate: 0.75 }] }),
    { code: 1, reason: 'uploaded content did not match after download' })
  })

  it('keeps a cleanup failure next to the correctness failure that takes precedence', (): void => {
    deepEqual(classifyStep({ ...upload, exitCode: 2, producerExitCode: 1 }, {
      attempts: [{ cleanup_rate: 1, consistency_rate: 0.5 }, { cleanup_rate: 0, consistency_rate: 1 }],
      failure_reason: 'test-directory cleanup failed',
      status: 'infrastructure_failure',
    }), { code: 1, reason: 'uploaded content did not match after download; test-directory cleanup failed' })
    deepEqual(classifyStep({ ...upload, exitCode: 2, producerExitCode: 1 }, {
      attempts: [{ cleanup_rate: 0, consistency_rate: 0.5 }],
      failure_reason: 'test-directory cleanup failed; uploaded content did not match after download',
      status: 'infrastructure_failure',
    }), { code: 1, reason: 'test-directory cleanup failed; uploaded content did not match after download' })
  })

  it('leaves cleanup and setup failures inconclusive', (): void => {
    deepEqual(classifyStep({ ...upload, exitCode: 2, producerExitCode: 2 }, {
      attempts: [{ cleanup_rate: 0, consistency_rate: 1 }],
      failure_reason: 'test-directory cleanup failed',
      status: 'infrastructure_failure',
    }), { code: 2, reason: 'test-directory cleanup failed' })
    deepEqual(classifyStep({ ...ramp, exitCode: 2, producerExitCode: 99 }, {
      failure_reason: 'test-directory cleanup failed', result_bound: 'inconclusive', status: 'infrastructure_failure',
    }), { code: 2, reason: 'test-directory cleanup failed' })
    deepEqual(classifyStep({ ...ramp, exitCode: 2, producerExitCode: 99 },
      { max_sustained_level: 8, result_bound: 'inconclusive', status: 'completed' }),
    { code: 2, reason: 'ramp result is inconclusive' })
    deepEqual(classifyStep({ ...fanout, exitCode: 2, producerExitCode: 2 },
      { capacity: { kind: 'inconclusive' }, error: 'Observation SLA deadline exceeded', status: 'inconclusive' }),
    { code: 2, reason: 'Observation SLA deadline exceeded' })
    deepEqual(classifyStep({ ...uploadStep, exitCode: 2 }, null), { code: 2, reason: 'no result file (exit 2)' })
    deepEqual(classifyStep({ ...uploadStep, exitCode: 2, id: '', kind: 'prepare' }, null),
      { code: 2, reason: 'step failed (exit 2)' })
    deepEqual(classifyStep({ ...uploadStep, id: '', kind: 'prepare' }, null), { code: 0, reason: 'completed' })
  })

  it('fails a search whose first level failed, for every producer', (): void => {
    deepEqual(classifyStep({ ...upload, exitCode: 2, producerExitCode: 1 }, { ...completedUpload, maximum_concurrent_uploads: 0 }),
      { code: 1, reason: 'a single upload failed the criteria' })
    deepEqual(classifyStep(ramp, { ...rampBracket, max_sustained_level: 0 }), { code: 1, reason: 'the first ramp level failed' })
    deepEqual(classifyStep({ ...fanout, exitCode: 2, producerExitCode: 1 }, { capacity: { kind: 'inconclusive' }, status: 'failed' }),
      { code: 1, reason: 'the first fanout level missed its deadline' })
  })
})

describe('campaignOutcome', (): void => {
  const ok = { code: 0, reason: 'completed' } as const
  const inconclusive = { code: 2, reason: 'no result file (exit 2)' } as const
  const corrupt = { code: 1, reason: 'uploaded content did not match after download' } as const

  it('lets a correctness failure take precedence over inconclusive steps and interruptions', (): void => {
    equal(campaignOutcome([ok, inconclusive, corrupt], false), 1)
    equal(campaignOutcome([corrupt], true), 1)
  })

  it('keeps an early failure even when the final step completed', (): void => {
    equal(campaignOutcome([inconclusive, ok], false), 2)
  })

  it('succeeds only when every step completed and the run was neither empty nor interrupted', (): void => {
    equal(campaignOutcome([ok, ok], false), 0)
    equal(campaignOutcome([ok], true), 2)
    equal(campaignOutcome([], false), 2)
  })
})
