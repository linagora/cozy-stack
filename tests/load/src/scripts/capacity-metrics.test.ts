import assert from 'node:assert/strict'
import test from 'node:test'
import { getReportSamples, renderMetrics, type ReportFile } from './capacity-metrics.ts'
import { uploadEnvelope } from './result-envelope.ts'

const counts = { valid: 1, invalid: 0, unsupported: 0 }

test('run metadata preserves packaged Stack versions', () => {
  const stack: string = '1:1.2~20261026~bookworm.build5-880035feabccc8e1e118dffbdfc49eb493c22956'
  const output: string = renderMetrics([report('upload', { versions: { stack } })], counts)
  assert.ok(samples(output, 'run_info')[0]!.includes(`stack_revision="${stack}"`))
})

test('activity profiles distinguish owner credentials from member identities', () => {
  const owner: string = renderMetrics([report('shared_drive', {})], counts)
  assert.match(samples(owner, 'run_info')[0]!, /profile="Writer \/ WebSocket \/ changes roles · one identity"/)
  const members: string = renderMetrics([report('shared_drive', {
    actors: { placement: [{ role: 'owner' }, { role: 'writer' }, { role: 'reader' }] },
  })], counts)
  assert.match(samples(members, 'run_info')[0]!, /profile="Owner, writer and read-only member identities"/)
})

test('the same four verdicts describe both modes on either environment', () => {
  for (const load_target of ['local', 'dev']) {
    for (const mode of ['regression', 'capacity']) {
      const output = renderMetrics([report('upload', {
        load_target, mode, outcome: 0, status: 'completed',
        maximum_concurrent_uploads: 2, result_bound: 'lower_bound',
        criteria: { p95_limit_mode: 'none' },
        attempts: [{ vus: 2, iterations_per_vu: 10, completed_iterations: 20,
          consistency_rate: 1, success_rate: 1, cleanup_rate: 1 }],
      })], counts)
      const verdict = samples(output, 'verdict_info')[0]!
      assert.match(verdict, new RegExp(`load_target="${load_target}",mode="${mode}"`))
      assert.match(verdict, /execution="Completed",correctness="Verified",cleanup="Verified"/)
      assert.match(verdict, mode === 'regression' ? /performance="Observational"/ : /performance=">= 2"/)
      if (mode === 'regression') assert.equal(value(output, 'capacity_lower'), undefined)
    }
  }
})

test('regression latency gates come from its criteria; missing criteria stay unknown', () => {
  const gated = renderMetrics([report('upload', {
    mode: 'regression', criteria: { maximum_p95_ms: 500, p95_limit_mode: 'absolute' },
  })], counts)
  assert.match(gated, /performance="Configured gate"/)
  assert.match(gated, /metric="p95_limit_ms"} 500/)
  const absent = renderMetrics([report('upload', { mode: 'regression' })], counts)
  assert.match(absent, /performance="Not recorded"/)
  assert.doesNotMatch(absent, /No latency gate|Observational/)
})

test('unknown, partial, failed and cleanup evidence never become green by inference', () => {
  const attempt = { vus: 2, iterations_per_vu: 10, completed_iterations: 20,
    consistency_rate: 1, success_rate: 1, cleanup_rate: 1 }
  for (const [data, expected] of [
    [{}, /execution="Not recorded",correctness="Not measured",cleanup="Not recorded"/],
    [{ outcome: 2, attempts: [{ ...attempt, completed_iterations: 1, cleanup: { remaining: ['private-id'] } }] },
      /execution="Inconclusive",correctness="Partial",cleanup="Unverified"/],
    [{ outcome: 1, attempts: [{ ...attempt, consistency_rate: 0, cleanup_rate: null }] },
      /execution="Failed",correctness="Failed",cleanup="Unverified"/],
    [{ outcome: 2, status: 'infrastructure_failure', result_bound: 'lower_bound', maximum_concurrent_uploads: 8 },
      /performance="Inconclusive",performance_detail="Retained evidence: >= 8"/],
    [{ attempts: [attempt, {}] }, /correctness="Partial",cleanup="Unverified"/],
  ] as const) {
    const output = renderMetrics([report('upload', data)], counts)
    assert.match(samples(output, 'verdict_info')[0]!, expected)
    assert.doesNotMatch(output, /private-id/)
  }
  const ramp = renderMetrics([report('shared_drive', {
    outcome: 0, cleanup: { remaining: [] }, levels: [{ level: 1, status: 'passed' }],
  })], counts)
  assert.match(ramp, /correctness="Not measured",cleanup="Verified"/)
  const fanout = { n: 1, recipients: [{ add: { converged: true, marker_matches: true },
    remove: { converged: true } }] }
  assert.match(renderMetrics([report('fanout', { points: [fanout], retained: true })], counts),
    /correctness="Verified",cleanup="Retained"/)
  assert.match(renderMetrics([report('fanout', { points: [{ n: 1, recipients: [] }] })], counts),
    /correctness="Partial"/)
  assert.match(renderMetrics([report('fanout', { points: [fanout], content_mismatch: true })], counts),
    /correctness="Failed"/)
})

test('run-window links use only valid recorded timestamps', () => {
  const input = { ...report('upload', {}), startedAt: 1, endedAt: 10 }
  assert.match(renderMetrics([input], counts), /start_ms="1000",end_ms="10000"/)
  assert.match(renderMetrics([input], counts), /focus_from_ms="0",focus_to_ms="70000"/)
  for (const endedAt of [0, NaN, Infinity, 8.64e12]) {
    assert.equal(samples(renderMetrics([{ ...input, endedAt }], counts), 'window_info').length, 0)
  }
  assert.equal(samples(renderMetrics([report('upload', {})], counts), 'window_info').length, 0)
})

test('upload root accepts lower bounds and zero but rejects absent or unsafe capacity', () => {
  for (const bound of ['exact', 'lower_bound']) {
    const output = renderMetrics([report('upload', {
      status: 'completed', result_bound: bound, maximum_concurrent_uploads: 0,
    })], counts)
    assert.equal(value(output, 'capacity_lower'), 0)
    assert.match(samples(output, 'result_info')[0]!, new RegExp(`bound="${bound}"`))
  }
  for (const lower of [undefined, null, -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1, '57']) {
    const output = renderMetrics([report('upload', {
      status: 'completed', result_bound: 'exact', maximum_concurrent_uploads: lower,
    })], counts)
    assert.equal(value(output, 'capacity_lower'), undefined)
    assert.match(samples(output, 'result_info')[0]!, /status="inconclusive"/)
  }
  for (const status of ['running', 'inconclusive', 'private-error', undefined]) {
    const output = renderMetrics([report('upload', {
      status, result_bound: 'exact', maximum_concurrent_uploads: 57,
      attempts: [{ vus: 57, result: 'passed' }],
    })], counts)
    assert.equal(value(output, 'capacity_lower'), undefined)
    if (status === 'running') assert.match(output, /status="running",bound="inconclusive",result="Running"/)
  }
})

test('activity capacity projects only completed lower_bound or ordered bracket roots', () => {
  for (const diagnostics of [{}, {
    cleanup_success_rate: 0, criteria: {}, plan: {}, levels: [{ level: 99, status: 'passed' }],
  }]) {
    for (const result_bound of ['lower_bound', 'bracket']) {
      const output = renderMetrics([report('shared_drive', {
        status: 'completed', result_bound, max_sustained_level: 0, first_failed_level: 3, ...diagnostics,
      })], counts)
      assert.equal(value(output, 'capacity_lower'), 0)
      assert.equal(value(output, 'capacity_upper'), result_bound === 'bracket' ? 3 : undefined)
      assert.match(output, /scope="activity_mix",status="completed"/)
      assert.match(output, result_bound === 'bracket' ? /result="\[0, 3\)"/ : /result=">= 0"/)
    }
  }
  for (const override of [
    { status: undefined }, { status: 'inconclusive' }, { status: 'unknown' },
    { result_bound: 'exact' }, { result_bound: undefined }, { max_sustained_level: null },
    { max_sustained_level: -1 }, { max_sustained_level: 1.5 },
    { first_failed_level: 0 }, { first_failed_level: null }, { first_failed_level: Infinity },
  ]) {
    const output = renderMetrics([report('shared_drive', {
      status: 'completed', result_bound: 'bracket', max_sustained_level: 0, first_failed_level: 3,
      levels: [{ level: 1, status: 'passed' }, { level: 3, status: 'failed' }], ...override,
    })], counts)
    assert.equal(value(output, 'capacity_lower'), undefined)
    assert.equal(value(output, 'capacity_upper'), undefined)
    assert.match(output, /bound="inconclusive",result="Inconclusive"/)
  }
})

test('fanout capacity projects producer passed lower bounds and failed brackets only', () => {
  for (const diagnostics of [{}, {
    settings: { stable_ms: -1 }, points: [{ n: 99, status: 'inconclusive' }], error: 'private',
  }]) {
    for (const status of ['passed', 'failed']) {
      const output = renderMetrics([report('fanout', {
        status, capacity: { kind: status === 'passed' ? 'lower_bound' : 'bracket',
          lower: 0, upper_exclusive: 5 }, ...diagnostics,
      })], counts)
      assert.equal(value(output, 'capacity_lower'), 0)
      assert.equal(value(output, 'capacity_upper'), status === 'failed' ? 5 : undefined)
      assert.match(output, /scope="recipients",status="completed"/)
    }
  }
  for (const override of [
    { status: 'inconclusive' }, { status: undefined }, { status: 'unknown' },
    { status: 'passed' }, { capacity: { kind: 'lower_bound', lower: 0 } },
    { capacity: { kind: 'bracket', lower: 5, upper_exclusive: 5 } },
    { capacity: { kind: 'bracket', lower: 6, upper_exclusive: 5 } },
    { capacity: { kind: 'bracket', lower: null, upper_exclusive: 5 } },
    { capacity: { kind: 'bracket', lower: 0, upper_exclusive: null } },
    { capacity: { kind: 'bracket', lower: 0, upper_exclusive: 1.5 } },
  ]) {
    const output = renderMetrics([report('fanout', {
      status: 'failed', capacity: { kind: 'bracket', lower: 0, upper_exclusive: 5 },
      points: [{ n: 4, status: 'passed' }], ...override,
    })], counts)
    assert.equal(value(output, 'capacity_lower'), undefined)
    assert.equal(value(output, 'capacity_upper'), undefined)
  }
})

test('metadata is allowlisted and identities are escaped in Prometheus text', () => {
  const input = report('upload', {
    status: 'completed', result_bound: 'exact', maximum_concurrent_uploads: 2,
    file_size: '1M', upload_target: 'shared-drive', verdict: 'secret-token', reason: 'private-error',
    domain: 'private.example', token: 'secret-token',
  })
  input.campaign = 'line\n"quoted"\\campaign'
  const output = renderMetrics([input], { valid: 1, invalid: 2, unsupported: 3 })
  assert.match(output, /campaign="line\\n\\"quoted\\"\\\\campaign"/)
  assert.match(output, /file_size="1M",upload_target="shared-drive",reason="recorded"/)
  assert.equal(value(output, 'result_timestamp_seconds'), 123)
  for (const family of ['result_info', 'result_timestamp_seconds', 'capacity_lower', 'exporter_reports']) {
    assert.equal(output.split('\n').filter(line => line === `# TYPE cozy_load_${family} gauge`).length, 1)
  }
  assert.deepEqual(samples(output, 'exporter_reports'), [
    'cozy_load_exporter_reports{state="valid"} 1',
    'cozy_load_exporter_reports{state="invalid"} 2',
    'cozy_load_exporter_reports{state="unsupported"} 3',
  ])
  assert.doesNotMatch(output, /private|secret/)
  input.data.file_size = 'private.example'
  input.data.upload_target = 'secret-token'
  input.timestamp = NaN
  const unknown = renderMetrics([input], { valid: Infinity, invalid: 0, unsupported: 0 })
  assert.match(unknown, /file_size="n\/a",upload_target="n\/a"/)
  assert.equal(value(unknown, 'result_timestamp_seconds'), undefined)
  assert.doesNotMatch(unknown, /private|secret|NaN|Infinity/)
})

test('upload diagnostics project recorded statuses and numbers without reindexing malformed loads', () => {
  const output = renderMetrics([report('upload', {
    criteria: { minimum_success_rate: 0.9, confirmation_iterations_per_vu: 10, token: 'secret-token' },
    p95_limit_ms: null, configured_max_vus: 20,
    attempts: [null, { vus: '2' }, { vus: 2, phase: 'confirmation', result: 'passed',
      p95_ms: 0, success_rate: 0.1, cleanup_rate: 0, consistency_rate: 0,
      completed_iterations: 3, dropped_iterations: 4, iterations_per_vu: 5, error: 'private-error',
    }, { vus: 3, phase: 'private-domain', result: 'private-error', p95_ms: Infinity },
    { vus: -1 }, { vus: 4, phase: 'baseline', result: 'infrastructure_failure' }],
  })], counts)
  assert.equal(value(output, 'capacity_lower'), undefined)
  assert.deepEqual(samples(output, 'point_info').map(line => line.match(/index="(\d+)"/)![1]), ['3', '4', '6'])
  assert.match(samples(output, 'point_info')[0]!,
    /load="2",phase="confirmation",status="passed",reason="recorded"/)
  assert.match(samples(output, 'point_info')[1]!, /phase="unknown",status="inconclusive",reason="missing_result"/)
  assert.match(samples(output, 'point_info')[2]!, /status="infrastructure_failure",reason="recorded"/)
  for (const [metric, amount] of Object.entries({
    upload_p95_ms: 0, success_rate: 0.1, cleanup_rate: 0, consistency_rate: 0,
    completed_iterations: 3, dropped_iterations: 4, iterations_per_vu: 5,
  })) assert.match(output, new RegExp(`index="3"[^\\n]+metric="${metric}"} ${amount}(?:\\n|$)`))
  for (const [metric, amount] of Object.entries({
    minimum_success_rate: 0.9, confirmation_iterations_per_vu: 10, configured_max_vus: 20,
  })) assert.match(output, new RegExp(`cozy_load_result_value{[^\\n]+metric="${metric}"} ${amount}\\n`))
  assert.doesNotMatch(output, /metric="p95_limit_ms"|private|secret|Infinity|NaN|validated/)
})

test('activity diagnostics preserve native statuses, scalar criteria and numeric point values', () => {
  const output = renderMetrics([report('shared_drive', {
    status: 'inconclusive', result_bound: 'bracket', max_sustained_level: 3, first_failed_level: 4,
    cleanup_success_rate: 0, criteria: {
      minimum_success_rate: 0.95, minimum_delivery_ratio: 0.9, p95_limit_ms: 500,
    },
    plan: { hold_ms: 5000, vus_per_level: { writers: 2, wsReaders: 3, changesReaders: 4 } },
    levels: [{ level: null }, { level: 3, status: 'passed', upload_p95_ms: 12,
      upload_success_rate: 0, ws_delivery_ratio: 1.2, changes_delivery_ratio: null,
    }, { level: 4, status: 'not_run' }, { level: 5, status: 'not_measured' },
    { level: 6, status: 'inconclusive' }, { level: 7, status: 'private-error' }],
  })], counts)
  assert.equal(value(output, 'capacity_lower'), undefined)
  const points = samples(output, 'point_info')
  assert.equal(points.length, 5)
  assert.match(points[0]!, /index="2",load="3",phase="hold",status="passed",reason="recorded"/)
  assert.match(points[1]!, /status="not_run",reason="recorded"/)
  assert.match(points[2]!, /status="not_measured",reason="recorded"/)
  assert.match(points[3]!, /status="inconclusive",reason="recorded"/)
  assert.match(points[4]!, /status="inconclusive",reason="missing_result"/)
  for (const [metric, amount] of Object.entries({
    upload_p95_ms: 12, success_rate: 0, ws_delivery_ratio: 1.2, writers: 6, ws_readers: 9, changes_readers: 12,
  })) assert.match(output, new RegExp(`index="2"[^\\n]+metric="${metric}"} ${amount}\\n`))
  for (const [metric, amount] of Object.entries({
    cleanup_rate: 0, minimum_success_rate: 0.95, minimum_delivery_ratio: 0.9, p95_limit_ms: 500,
    hold_ms: 5000, writers_per_level: 2, ws_readers_per_level: 3, changes_readers_per_level: 4,
  })) assert.match(output, new RegExp(`cozy_load_result_value{[^\\n]+metric="${metric}"} ${amount}\\n`))
  assert.doesNotMatch(output, /metric="changes_delivery_ratio"|private|validated/)
})

test('fanout projects native owner and recipient observations without rebuilding proofs', () => {
  const output = renderMetrics([report('fanout', {
    settings: { timeout_ms: 100, stable_ms: 10, observer_concurrency: 2, effective_member_cap: 40 },
    points: [null, { n: '3' }, { n: 3, status: 'passed', phase: 'complete', observer_concurrency: 2,
      owner: { add: { ready_recipients: 57 }, remove: { revoked_recipients: 3 } },
      recipients: [{ domain: 'private.example', add: { converged: true } },
        { add: { converged: false } }, { add: { converged: 'true' } }],
      add: { elapsed_ms: 90 }, remove: { status: 'passed', elapsed_ms: 80 },
    }],
  })], counts)
  assert.equal(value(output, 'capacity_lower'), undefined)
  assert.match(samples(output, 'point_info')[0]!, /index="3",load="3",phase="complete",status="passed"/)
  for (const [metric, amount] of Object.entries({
    recipients: 3, observer_concurrency: 2, owner_ready: 57, owner_revoked: 3,
    recipient_access_proofs: 1, add_elapsed_ms: 90, remove_elapsed_ms: 80,
    remove_started: 1, remove_verified: 1,
  })) assert.match(output, new RegExp(`index="3"[^\\n]+metric="${metric}"} ${amount}\\n`))
  for (const [metric, amount] of Object.entries({
    budget_ms: 100, stable_ms: 10, observer_concurrency: 2, configured_member_cap: 40,
  })) assert.match(output, new RegExp(`cozy_load_result_value{[^\\n]+metric="${metric}"} ${amount}\\n`))
  assert.doesNotMatch(output, /private|metric="owner_pending"|validated/)
})

test('fanout removal flags follow only recognized native phase statuses', () => {
  for (const [status, started, verified] of [
    ['not_started', 0, undefined], ['observing', 1, undefined], ['passed', 1, 1],
    ['failed', 1, 0], ['inconclusive', 1, 0], [undefined, undefined, undefined],
    [null, undefined, undefined], ['private-error', undefined, undefined],
  ] as const) {
    const output = renderMetrics([report('fanout', {
      points: [{ n: 1, status: 'inconclusive', phase: 'remove', remove: { status },
        owner: { add: { ready_recipients: 1, pending_recipients: 0 } },
        recipients: [{ add: { converged: false } }],
      }],
    })], counts)
    const metric = (name: string) => {
      const line = samples(output, 'point_value').find(line => line.includes(`metric="${name}"`))
      return line === undefined ? undefined : Number(line.slice(line.lastIndexOf(' ') + 1))
    }
    assert.equal(metric('remove_started'), started, String(status))
    assert.equal(metric('remove_verified'), verified, String(status))
    assert.equal(metric('owner_pending'), 0)
    assert.equal(metric('recipient_access_proofs'), 0)
    assert.doesNotMatch(output, /private/)
  }
  const absent = renderMetrics([report('fanout', { points: [{ n: 1 }] })], counts)
  assert.doesNotMatch(absent, /metric="(?:recipient_access_proofs|owner_ready|owner_pending|remove_verified)"/)
})

test('recorded campaign start and end timestamps are optional scalar values, never inferred', () => {
  for (const kind of ['upload', 'shared_drive', 'fanout'] as const) {
    const input = { ...report(kind, { started_at: 'private-date', ended_at: 999 }), startedAt: 0, endedAt: 10 }
    const output = renderMetrics([input], counts)
    assert.match(output, /metric="started_at_seconds"} 0\n/)
    assert.match(output, /metric="ended_at_seconds"} 10\n/)
    for (const missing of [report(kind, {}), { ...input, startedAt: undefined, endedAt: NaN }]) {
      assert.doesNotMatch(renderMetrics([missing], counts), /metric="(?:started|ended)_at_seconds"/)
    }
    assert.doesNotMatch(output, /private-date|999/)
  }
})

test('enum-shaped arrays are not accepted as producer enums', () => {
  const upload = renderMetrics([report('upload', {
    status: 'completed', result_bound: ['exact'], maximum_concurrent_uploads: 57,
    file_size: ['1M'], upload_target: ['personal'],
  })], counts)
  assert.equal(value(upload, 'capacity_lower'), undefined)
  assert.match(upload, /file_size="n\/a",upload_target="n\/a"/)
  const fanout = renderMetrics([report('fanout', {
    points: [{ n: 1, status: ['passed'], phase: ['complete'], remove: { status: ['passed'] } }],
  })], counts)
  assert.doesNotMatch(fanout, /metric="remove_(?:started|verified)"/)
  assert.match(fanout, /phase="unknown",status="inconclusive",reason="missing_result"/)
})

test('nonpositive and malformed point loads are omitted without changing source indexes', () => {
  const invalidLoads = [0, -1, 1.5, null, undefined, '2', NaN, Infinity, Number.MAX_SAFE_INTEGER + 1]
  for (const [kind, collection, loadKey] of [
    ['upload', 'attempts', 'vus'], ['shared_drive', 'levels', 'level'], ['fanout', 'points', 'n'],
  ] as const) {
    const points = [...invalidLoads, 2].map(load => ({ [loadKey]: load, status: 'passed', result: 'passed' }))
    const output = renderMetrics([report(kind, { [collection]: points })], counts)
    assert.equal(samples(output, 'point_info').length, 1)
    assert.match(samples(output, 'point_info')[0]!, /index="10",load="2"/)
  }
})

function report(kind: ReportFile['kind'], data: Record<string, unknown>): ReportFile {
  return { campaign: 'campaign', run: 'run', kind, timestamp: 123, data }
}

function samples(output: string, family: string): string[] {
  return output.split('\n').filter(line => line.startsWith(`cozy_load_${family}{`))
}

function value(output: string, family: string): number | undefined {
  const line = samples(output, family)[0]
  return line === undefined ? undefined : Number(line.slice(line.lastIndexOf(' ') + 1))
}

test('upload capacity follows the completed producer root, not optional diagnostics', () => {
  const root = { status: 'completed', result_bound: 'exact', maximum_concurrent_uploads: 57 }
  for (const diagnostics of [{}, { criteria: {}, attempts: [] }, {
    criteria: { minimum_success_rate: 1 },
    attempts: [{ vus: 99, result: 'failed', cleanup_rate: 0 }],
  }]) {
    const output = renderMetrics([report('upload', { ...root, ...diagnostics })], counts)
    assert.equal(value(output, 'capacity_lower'), 57)
    assert.equal(value(output, 'capacity_upper'), undefined)
    assert.match(samples(output, 'result_info')[0]!, /status="completed",bound="exact",result="= 57"/)
    assert.match(samples(output, 'result_info')[0]!, /reason="recorded"/)
  }
})

test('a regression run shows its verdict and environment, never a capacity', () => {
  for (const [status, verdict, outcome] of [['completed', 'passed', 0], ['regression_failure', 'failed', 1],
    ['correctness_failure', 'failed', 1], ['infrastructure_failure', 'inconclusive', 2]] as const) {
    const output = renderMetrics([report('upload', {
      mode: 'regression', load_target: 'dev', status, outcome, maximum_concurrent_uploads: 2,
      attempts: [{ phase: 'regression', vus: 2, result: verdict === 'inconclusive' ? 'infrastructure_failure' : verdict }],
    })], counts)
    assert.equal(value(output, 'capacity_lower'), undefined)
    assert.equal(value(output, 'capacity_evidence_lower'), undefined)
    assert.match(samples(output, 'result_info')[0]!, new RegExp(
      `status="${verdict}",bound="none",result="${verdict[0]!.toUpperCase()}${verdict.slice(1)}",.*` +
      `reason="recorded",outcome="${outcome}",evidence="none"`))
    assert.match(samples(output, 'point_info')[0]!, /phase="regression"/)
  }
})

test('an unverified run keeps its recorded bound as evidence, never as a capacity', () => {
  const cases: Array<[ReportFile['kind'], Record<string, unknown>, string]> = [
    ['upload', { status: 'infrastructure_failure', result_bound: 'lower_bound', maximum_concurrent_uploads: 2 }, '>= 2'],
    ['shared_drive', { status: 'infrastructure_failure', result_bound: 'inconclusive', max_sustained_level: 8,
      first_failed_level: 16 }, '[8, 16)'],
    ['fanout', { status: 'inconclusive', capacity: { kind: 'lower_bound', lower: 16 } }, '>= 16'],
  ]
  for (const [kind, data, evidence] of cases) {
    const output = renderMetrics([report(kind, { ...data, outcome: 2, load_target: 'dev' })], counts)
    assert.equal(value(output, 'capacity_lower'), undefined)
    assert.match(samples(output, 'result_info')[0]!, new RegExp(
      `status="inconclusive",.*outcome="2",evidence="${evidence.replace(/[[\]()]/g, '\\$&')}"`))
    assert.equal(value(output, 'capacity_evidence_lower'), Number(evidence.match(/\d+/)![0]))
  }
  // A completed result is a capacity, not evidence.
  const completed = renderMetrics([report('upload', {
    status: 'completed', result_bound: 'exact', maximum_concurrent_uploads: 18, outcome: 0 })], counts)
  assert.equal(value(completed, 'capacity_lower'), 18)
  assert.equal(value(completed, 'capacity_evidence_lower'), undefined)
  assert.match(samples(completed, 'result_info')[0]!, /evidence="none"/)
})

test('reports without environment metadata show it unknown', () => {
  const output = renderMetrics([report('upload', {
    status: 'completed', result_bound: 'exact', maximum_concurrent_uploads: 57, load_target: 'staging', outcome: 7,
  })], counts)
  assert.match(samples(output, 'result_info')[0]!, /load_target="unknown",mode="capacity",.*outcome="unknown"/)
})

test('planned and unmeasured levels never become the largest tested load', () => {
  const output = renderMetrics([report('shared_drive', { levels: [
    { level: 1, status: 'passed' }, { level: 2, status: 'not_measured' },
    { level: 4, status: 'not_run' }, { level: 8 },
  ] })], counts)
  const loads = samples(output, 'point_value').filter(line => line.includes('metric="load"'))
  assert.equal(loads.length, 1)
  assert.equal(samples(output, 'point_value').filter(line => line.includes('metric="point_index"')).length, 1)
  assert.match(loads[0]!, /load="1"/)
  assert.equal(samples(output, 'point_info').length, 4, 'planned points stay visible as evidence')
})

test('timeline bounds carry the actual execution identity and activity counts come from recorded uploads', () => {
  const rows = getReportSamples([report('upload', { attempts: [{ vus: 2, result: 'passed',
    execution_id: 'test-attempt', started_at: '2026-10-26T10:00:00Z', ended_at: '2026-10-26T10:00:10Z',
  }] }), report('shared_drive', { levels: [
    { level: 1, status: 'passed', uploads: 0 }, { level: 2, status: 'passed' },
  ] })], counts)
  const bounds = rows.filter(row => ['started_at_seconds', 'ended_at_seconds'].includes(row.labels.metric ?? '') && row.name.endsWith('point_value'))
  assert.equal(bounds.length, 2)
  assert.ok(bounds.every(row => row.labels.executionid === 'test-attempt'))
  assert.equal(bounds[1]!.value - bounds[0]!.value, 10)
  const uploads = rows.filter(row => row.labels.metric === 'completed_uploads')
  assert.deepEqual(uploads.map(row => [row.labels.index, row.value]), [['1', 0]])
})

test('recipient revocation counts require recipient observations even when the owner reports success', () => {
  const points = [
    { n: 2, status: 'passed', owner: { remove: { revoked_recipients: 2 } } },
    { n: 2, status: 'failed', recipients: [{ remove: { converged: true } }, { remove: { converged: false } }] },
  ]
  const rows = getReportSamples([report('fanout', { points })], counts)
    .filter(row => row.labels.metric === 'recipient_revocation_proofs')
  assert.deepEqual(rows.map(row => [row.labels.index, row.value]), [['2', 1]])
})

test('upload work summaries require actual success and complete readback coverage', () => {
  const attempt = { vus: 2, iterations_per_vu: 10, completed_iterations: 20,
    success_rate: 1, consistency_rate: 1, cleanup_rate: 1, phase: 'confirmation', result: 'passed',
    p95_ms: 250, execution_id: 'run-confirmation-v2-a1', started_at: '2026-10-26T10:00:00Z', ended_at: '2026-10-26T10:00:10Z' }
  const good = renderMetrics([report('upload', { file_size: '1M', attempts: [attempt] })], counts)
  assert.match(good, /completed="20 \/ 20",readback="20 matched"/)
  assert.match(good, /metric="payload_bytes"} 20971520/)
  assert.match(good, /metric="point_uploads_per_second"} 2\n/)
  assert.match(samples(good, 'point_evidence_info')[0]!, /execution_id="run-confirmation-v2-a1"/)
  const partial = renderMetrics([report('upload', { file_size: '1M', attempts: [{ ...attempt, success_rate: 0.5 }] })], counts)
  assert.match(partial, /completed="Not recorded \/ 20",readback="Readback coverage incomplete"/)
  assert.equal(value(partial, 'failed_uploads'), undefined)
  assert.equal(value(partial, 'payload_bytes'), undefined)
})

test('point explanations are safe categories and legacy attempt IDs come only from the known summary path', () => {
  const output = renderMetrics([report('upload', { attempts: [
    { vus: 1, result: 'failed', reason: 'private-token p95 limit exceeded at private.example',
      summary_path: 'data/results/run-baseline-v1-a1-summary.json' },
    { vus: 2, result: 'failed', summary_path: '/private/secret-summary.json' },
  ] })], counts)
  const points = samples(output, 'point_evidence_info')
  assert.match(points[0]!, /execution_id="run-baseline-v1-a1"/)
  assert.match(points[0]!, /reason="Latency limit exceeded"/)
  assert.match(points[1]!, /execution_id="not-recorded"/)
  assert.match(points[1]!, /reason="Reason not recorded"/)
  assert.doesNotMatch(output, /private-token|private.example|\/private\/|secret-summary/)
})

test('headline verdicts follow a valid envelope and an invalid envelope cannot become green', () => {
  const data = { status: 'completed', outcome: 0, mode: 'capacity', load_target: 'local',
    maximum_concurrent_uploads: 2, result_bound: 'lower_bound',
    attempts: [{ vus: 2, iterations_per_vu: 10, completed_iterations: 20, success_rate: 1,
      consistency_rate: 1, cleanup: { remaining: [] } }] }
  const envelope = uploadEnvelope(data, { interrupted: false, versions: { stack: 'test', harness: 'test' },
    topology: 'single-host', start: '2026-10-26T10:00:00Z', end: '2026-10-26T10:01:00Z' })
  const failed = { ...envelope, outcome: 1, correctness: 'failed',
    capacity: { ...envelope.capacity!, verified: false } }
  const output = renderMetrics([report('upload', { ...data, result: failed })], counts)
  assert.match(samples(output, 'verdict_info')[0]!, /execution="Failed",correctness="Failed",cleanup="Verified"/)
  const invalid = renderMetrics([report('upload', { ...data, result: { ...failed, outcome: 0 } })], counts)
  assert.match(samples(invalid, 'verdict_info')[0]!, /execution="Inconclusive",correctness="Unverified",cleanup="Unverified"/)
})

test('point identities belong to their run and incomplete checks stay visually unverified', () => {
  const attempt = { vus: 1, iterations_per_vu: 1, completed_iterations: 1,
    success_rate: 1, consistency_rate: 1, cleanup_rate: 1, result: 'passed' }
  for (const run of ['first', 'second']) {
    for (const [change, expected] of [
      [{}, 'Verified'], [{ cleanup_rate: 0 }, 'Unverified'],
      [{ consistency_rate: 0 }, 'Failed'], [{ result: 'failed' }, 'Failed'],
    ] as const) {
      const output = renderMetrics([{ ...report('upload', { attempts: [{ ...attempt, ...change }] }), run }], counts)
      const point = samples(output, 'point_evidence_info')[0]!
      assert.match(point, new RegExp(`point_id="${run}-point-1"`))
      assert.match(point, new RegExp(`evidence="${expected}"`))
    }
  }
})
