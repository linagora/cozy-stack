import { validateEnvelope } from './result-envelope.ts'

export type ReportFile = {
  campaign: string
  run: string
  kind: 'upload' | 'shared_drive' | 'fanout' | 'mixed'
  timestamp: number
  startedAt?: number | undefined
  endedAt?: number | undefined
  data: Record<string, unknown>
}

type Labels = Record<string, string>
export interface ReportSample { name: string; labels: Labels; value: number }

function allowed(value: unknown, choices: string[], fallback: string): string {
  return typeof value === 'string' && choices.includes(value) ? value : fallback
}

function finite(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0
}

function integer(value: unknown): value is number {
  return finite(value) && Number.isSafeInteger(value)
}

function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown> : {}
}

function getRevision(value: unknown): string {
  return typeof value === 'string' && /^[A-Za-z0-9._+:~-]{1,96}$/.test(value) ? value : 'Not recorded'
}

function records(value: unknown): Record<string, unknown>[] {
  return Array.isArray(value) ? value.map(record) : []
}

function reasonCategory(value: unknown, passed = false): string {
  // A search's stop names its level first (result-envelope.ts); the category comes from why.
  const text = typeof value === 'string' ? value.replace(/^level \d+ \([^)]*\): /, '') : ''
  for (const [pattern, category] of [
    [/^every level passed/i, 'Every level passed'],
    [/cleanup|reconcil|remaining|retained/i, 'Cleanup unverified'],
    [/content.*(match|mismatch)|checksum/i, 'Content mismatch'],
    [/p95|latency/i, 'Latency limit exceeded'],
    [/success rate|upload.*fail/i, 'Upload success rate below minimum'],
    [/delivery|readers reported/i, 'Delivery below minimum'],
    [/dropped/i, 'Arrivals dropped'],
    [/iteration|incomplete|interrupt|stopped/i, 'Work incomplete'],
    [/deadline|timeout|timed out|SLA/i, 'Observation deadline exceeded'],
    [/budget|MAX_(RUNTIME|VUS|UPLOAD_BYTES|REQUESTS)/i, 'Experiment limit reached'],
    [/HTTP 5\d\d|busy/i, 'Server error'],
    [/summary|metric|report|envelope/i, 'Report evidence unavailable'],
  ] as const) if (pattern.test(text)) return category
  return text === '' ? passed ? 'Required checks passed' : 'Reason not recorded' : 'See report diagnostics'
}

function parseTimestamp(value: unknown): number | null {
  const parsed = typeof value === 'string' ? Date.parse(value) : NaN
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : null
}

const fileBytes: Record<string, number> = {
  '1K': 1024, '100K': 102400, '1M': 1048576, '10M': 10485760, '100M': 104857600, '1G': 1073741824,
}

// These describe recorded checks, not a new verdict or a reconstruction of the capacity search.
function checks(kind: ReportFile['kind'], data: Record<string, unknown>): { correctness: string, cleanup: string } {
  const attempts = records(data.attempts)
  const points = records(data.points)
  let correctness = 'Not measured'
  if (data.status === 'correctness_failure' || data.content_mismatch === true ||
      attempts.some(point => finite(point.consistency_rate) && point.consistency_rate < 1)) {
    correctness = 'Failed'
  } else if (kind === 'upload' && attempts.some(point => finite(point.consistency_rate))) {
    correctness = attempts.every(point => point.consistency_rate === 1 && point.success_rate === 1 &&
      integer(point.completed_iterations) && point.completed_iterations > 0 &&
      integer(point.vus) && integer(point.iterations_per_vu) &&
      point.completed_iterations === point.vus * point.iterations_per_vu) ? 'Verified' : 'Partial'
  } else if (kind === 'fanout' && points.length) {
    correctness = points.every(point => {
      const recipients = records(point.recipients)
      return integer(point.n) && point.n > 0 && recipients.length === point.n &&
        recipients.every(recipient => record(recipient.add).converged === true &&
          record(recipient.add).marker_matches === true && record(recipient.remove).converged === true)
    }) ? 'Verified' : 'Partial'
  } else if (kind === 'mixed' && points.length) {
    // Each rate checked every successful metadata read and upload against the fixture.
    correctness = points.some(point => Array.isArray(point.correctness_failures) && point.correctness_failures.length)
      ? 'Failed' : points.every(point => record(point.correctness).complete === true) ? 'Verified' : 'Partial'
  } else if (kind === 'shared_drive' && points.length) {
    // Readers download a sample of each level's uploads back, as the producer judges a level.
    correctness = points.some(point => point.content_mismatches || point.content_denied) ? 'Failed'
      : points.every(point => integer(point.content_verified) && point.content_verified > 0 && !point.content_unverified)
        ? 'Verified' : 'Partial'
  }
  let cleanup = 'Not recorded'
  const cleared = (point: Record<string, unknown>): boolean => {
    const remaining = record(point.cleanup).remaining
    return Array.isArray(remaining) ? remaining.length === 0 : point.cleanup_rate === 1
  }
  if (kind === 'upload' && attempts.length) {
    cleanup = attempts.every(cleared) ? 'Verified' : 'Unverified'
  } else if (kind === 'shared_drive') {
    const remaining = record(data.cleanup).remaining
    if (Array.isArray(remaining)) cleanup = remaining.length === 0 ? 'Verified' : 'Unverified'
    else if (finite(data.cleanup_success_rate)) {
      cleanup = data.cleanup_success_rate === 1 ? 'Verified' : 'Unverified'
    }
  } else if (kind === 'mixed' && points.length) {
    cleanup = points.every(cleared) ? 'Verified' : 'Unverified'
  } else if (kind === 'fanout') {
    const retained = record(data.cleanup).retained
    if (Array.isArray(retained)) cleanup = retained.length === 0 ? 'Verified' : 'Unverified'
    else if (data.retained === true) cleanup = 'Retained'
  }
  return { correctness, cleanup }
}

// A regression run's verdict, by its recorded status; it never claims a capacity.
const regressionVerdicts: Record<string, string> = {
  completed: 'passed', regression_failure: 'failed', correctness_failure: 'failed',
  infrastructure_failure: 'inconclusive', running: 'running',
}

function numbers(source: Record<string, unknown>, mapping: Record<string, string>): Record<string, number> {
  return Object.fromEntries(Object.entries(mapping).flatMap(([metric, key]) =>
    finite(source[key]) ? [[metric, source[key]]] : []))
}

// The slowest operation's p95 at one rate of a mixed search.
function worstP95(level: Record<string, unknown>): number | undefined {
  const p95s = Object.values(record(level.operations)).map(op => record(op).p95_ms).filter(finite)
  return p95s.length ? Math.max(...p95s) : undefined
}

function mixedValues(level: Record<string, unknown>): Record<string, number> {
  const values = numbers(level, {
    achieved_rate: 'achieved_rate', achieved_workflows: 'achieved_workflows', dropped_workflows: 'dropped_iterations',
  })
  for (const [name, op] of Object.entries(record(level.operations))) {
    if (!/^[a-z]+$/.test(name)) continue
    const { p95_ms: p95, error_rate: errors } = record(op)
    if (finite(p95)) values[`${name}_p95_ms`] = p95
    if (finite(errors) && errors <= 1) values[`${name}_success_rate`] = 1 - errors
  }
  return values
}

function fanoutValues(point: Record<string, unknown>): Record<string, number> {
  const owner = record(point.owner)
  const remove = record(point.remove)
  const values = {
    ...numbers(point, { recipients: 'n', observer_concurrency: 'observer_concurrency' }),
    ...numbers(record(owner.add), { owner_ready: 'ready_recipients', owner_pending: 'pending_recipients' }),
    ...numbers(record(owner.remove), { owner_revoked: 'revoked_recipients' }),
    ...numbers(record(point.add), { add_elapsed_ms: 'elapsed_ms' }),
    ...numbers(remove, { remove_elapsed_ms: 'elapsed_ms' }),
  }
  const recipients = Array.isArray(point.recipients) ? point.recipients.map(record) : []
  const adds = recipients.map(recipient => record(recipient.add))
  if (adds.some(add => typeof add.converged === 'boolean')) {
    values.recipient_access_proofs = adds.filter(add => add.converged === true).length
  }
  const removals = recipients.map(recipient => record(recipient.remove))
  if (removals.some(removal => typeof removal.converged === 'boolean')) {
    values.recipient_revocation_proofs = removals.filter(removal => removal.converged === true).length
  }
  if (remove.status === 'not_started') values.remove_started = 0
  if (allowed(remove.status, ['observing', 'passed', 'failed', 'inconclusive'], '') !== '') {
    values.remove_started = 1
  }
  if (allowed(remove.status, ['passed', 'failed', 'inconclusive'], '') !== '') {
    values.remove_verified = remove.status === 'passed' ? 1 : 0
  }
  return values
}

export function getReportSamples(
  reports: ReportFile[],
  counts: { valid: number; invalid: number; unsupported: number },
): ReportSample[] {
  const output: ReportSample[] = []
  const emit = (name: string, dimensions: Labels, value: unknown) => {
    if (!finite(value)) return
    output.push({ name: `cozy_load_${name}`, labels: dimensions, value })
  }
  for (const { campaign, run, kind, data, timestamp, startedAt, endedAt } of reports) {
    const base = { campaign, run, kind,
      load_target: allowed(data.load_target, ['local', 'dev'], 'unknown'),
      mode: allowed(data.mode, ['capacity', 'regression'], 'capacity') }
    let lower: number | undefined
    let upper: number | undefined
    let bound = 'inconclusive'
    if (kind === 'upload' && data.status === 'completed' &&
        (data.result_bound === 'exact' || data.result_bound === 'lower_bound') &&
        integer(data.maximum_concurrent_uploads)) {
      lower = data.maximum_concurrent_uploads
      bound = data.result_bound
    } else if (kind === 'shared_drive' && data.status === 'completed' &&
        integer(data.max_sustained_level)) {
      const bracket = data.result_bound === 'bracket' && integer(data.first_failed_level) &&
        data.first_failed_level > data.max_sustained_level
      if (data.result_bound === 'lower_bound' || bracket) {
        lower = data.max_sustained_level
        upper = bracket ? data.first_failed_level as number : undefined
        bound = bracket ? 'bracket' : 'lower_bound'
      }
    } else if (kind === 'fanout') {
      const capacity = record(data.capacity)
      const bracket = data.status === 'failed' && capacity.kind === 'bracket' &&
        integer(capacity.lower) && integer(capacity.upper_exclusive) &&
        capacity.upper_exclusive > capacity.lower
      const passed = data.status === 'passed' && capacity.kind === 'lower_bound' && integer(capacity.lower)
      if (passed || bracket) {
        lower = capacity.lower as number
        upper = bracket ? capacity.upper_exclusive as number : undefined
        bound = bracket ? 'bracket' : 'lower_bound'
      }
    }
    const running = data.status === 'running'
    const regression = kind === 'upload' && data.mode === 'regression'
    if (regression) lower = upper = undefined
    // A runner that could not verify a run keeps the bound it measured, shown only as evidence.
    let evidence: { lower: number, upper?: number | undefined, exact: boolean } | undefined
    const capacity = record(data.capacity)
    if (!regression && lower === undefined) {
      if (kind === 'upload' && data.status === 'infrastructure_failure' && integer(data.maximum_concurrent_uploads)) {
        evidence = { lower: data.maximum_concurrent_uploads, exact: data.result_bound === 'exact' }
      } else if (kind === 'shared_drive' && ['completed', 'infrastructure_failure'].includes(String(data.status)) &&
          integer(data.max_sustained_level)) {
        const failed = integer(data.first_failed_level) && data.first_failed_level > data.max_sustained_level
        evidence = { lower: data.max_sustained_level, upper: failed ? data.first_failed_level as number : undefined, exact: false }
      } else if (kind === 'fanout' && data.status === 'inconclusive' && integer(capacity.lower)) {
        const bracket = capacity.kind === 'bracket' && integer(capacity.upper_exclusive) &&
          capacity.upper_exclusive > capacity.lower
        evidence = { lower: capacity.lower, upper: bracket ? capacity.upper_exclusive as number : undefined, exact: false }
      }
    }
    // The producer's envelope replaces every reading above; an invalid one claims nothing.
    const declared = data.result === undefined ? undefined : validateEnvelope(data.result)
    if (declared !== undefined) {
      const c = declared?.capacity
      lower = upper = evidence = undefined
      bound = 'inconclusive'
      if (c?.verified) [lower, upper, bound] = [c.lower, c.upper_exclusive ?? undefined, c.kind]
      else if (c) evidence = { lower: c.lower, upper: c.upper_exclusive ?? undefined, exact: c.kind === 'exact' }
    }
    const describe = (low: number, high: number | undefined, exact: boolean) =>
      high !== undefined ? `[${low}, ${high})` : exact ? `= ${low}` : `>= ${low}`
    const verdict = !regression ? undefined : declared === undefined ? regressionVerdicts[String(data.status)]
      : (['passed', 'failed', 'inconclusive'] as const)[declared?.outcome ?? 2]
    const p95Limit = regression ? record(data.criteria).maximum_p95_ms : data.p95_limit_ms
    const observational = record(data.criteria).p95_limit_mode === 'none'
    const outcome = declared === undefined ? data.outcome : declared?.outcome ?? 2
    const execution = running ? 'Running' : outcome === 0 ? 'Completed'
      : outcome === 1 ? 'Failed' : outcome === 2 ? 'Inconclusive' : 'Not recorded'
    const recordedChecks = checks(kind, data)
    if (declared !== undefined) {
      const verification = { verified: 'Verified', failed: 'Failed', not_verified: 'Unverified' }
      recordedChecks.correctness = declared ? verification[declared.correctness] : 'Unverified'
      recordedChecks.cleanup = declared ? verification[declared.cleanup] : 'Unverified'
      if (declared?.cleanup === 'not_verified' && kind === 'fanout' && data.retained === true) {
        recordedChecks.cleanup = 'Retained'
      }
    }
    const size = allowed(data.file_size, Object.keys(fileBytes), 'n/a')
    const target = allowed(data.upload_target, ['shared-drive', 'personal'], 'n/a')
    const workload = kind === 'upload' ? `${target === 'personal' ? 'Personal' : target === 'shared-drive' ? 'Shared-drive' : 'Legacy'} uploads`
      : kind === 'fanout' ? 'Organization sharing' : kind === 'mixed' ? 'Mixed workflows' : 'Shared-drive activity'
    const recordedStart = declared?.time.start ?? (finite(startedAt) && startedAt < 8.64e12
      ? new Date(startedAt * 1000).toISOString() : '')
    const versions = record(declared?.versions ?? data.versions)
    const budgets = record(record(declared?.parameters).budgets ?? data.budgets)
    // A completed search stopped for its own reason: a failing level, a budget or its last level.
    const runReason = declared === null ? 'Invalid result envelope'
      : reasonCategory(declared ? declared.reason || declared.capacity?.stop : data.failure_reason ?? data.error,
        outcome === 0)
    const runtime = typeof budgets.max_runtime === 'string' && /^\d+(ms|s|m|h)$/.test(budgets.max_runtime)
      ? budgets.max_runtime : 'Not recorded'
    const activityProfile: string = records(record(data.actors).placement).some(actor => actor.role === 'writer')
      ? 'Owner, writer and read-only member identities' : 'Writer / WebSocket / changes roles · one identity'
    emit('run_info', { ...base, title: `${workload}${size === 'n/a' ? '' : ` · ${size}`} · ${recordedStart.slice(5, 19).replace('T', ' ')} UTC`,
      workload, profile: kind === 'upload' ? `${size} files · ${target}`
        : kind === 'fanout' ? 'Distinct organization recipients'
        : kind === 'mixed' ? 'Tree fixture · list, metadata check and upload per workflow' : activityProfile,
      file_size: size, upload_target: target,
      reason: runReason,
      schema: declared === undefined ? 'legacy' : declared === null ? 'invalid' : declared.schema,
      notice: outcome !== 0 || data.budget_reached === true ? 'yes' : 'no',
      stack_revision: getRevision(versions.stack), harness_revision: getRevision(versions.harness),
      runtime_budget: runtime,
      vus_budget: finite(budgets.max_vus) ? String(budgets.max_vus) : 'Not recorded',
      bytes_budget: finite(budgets.max_upload_bytes) ? String(budgets.max_upload_bytes) : 'Not recorded',
      dataset: declared?.dataset ? `${declared.dataset.profile} v${declared.dataset.version}`
        : declared?.dataset === null ? 'None' : 'Not recorded',
      topology: allowed(declared?.topology, ['single-host', 'shared-dev', 'distributed'], 'Not recorded'),
    }, 1)
    emit('verdict_info', {
      ...base, execution, ...recordedChecks,
      performance: regression
        ? (declared === null ? 'Inconclusive' : finite(p95Limit)
          ? declared?.performance === 'failed' ? 'Gate failed'
            : declared?.performance === 'passed' ? 'Gate passed' : 'Configured gate'
          : observational ? 'Observational' : 'Not recorded')
        : running ? 'Running' : lower === undefined ? 'Inconclusive' : describe(lower, upper, bound === 'exact'),
      performance_detail: regression ? (finite(p95Limit) ? 'See recorded p95 criterion'
        : observational ? 'No latency gate' : 'Latency criterion not recorded')
        : evidence ? `Retained evidence: ${describe(evidence.lower, evidence.upper, evidence.exact)}`
        : kind === 'upload' ? 'Upload workers' : kind === 'fanout' ? 'Recipients'
        : kind === 'mixed' ? 'Workflows per second' : 'Activity mix units',
      outcome: [0, 1, 2].includes(outcome as number) ? String(outcome) : 'Not recorded',
    }, 1)
    if (finite(startedAt) && finite(endedAt) && endedAt >= startedAt && endedAt < 8.64e12) {
      const start = new Date(startedAt * 1000).toISOString()
      const end = new Date(endedAt * 1000).toISOString()
      emit('window_info', { ...base, start, end, window: `${start.slice(11, 19)} – ${end.slice(11, 19)} UTC`,
        start_ms: String(startedAt * 1000), end_ms: String(endedAt * 1000),
        // Match campaign links: leave time for Prometheus to scrape the completed report.
        focus_from_ms: String(Math.max(0, startedAt * 1000 - 60_000)),
        focus_to_ms: String(endedAt * 1000 + 60_000),
        open_run: 'Open run + context', run_only: 'Run only', all_points: 'All points',
        evidence: 'Download evidence',
      }, 1)
    }
    emit('result_info', {
      ...base,
      scope: kind === 'fanout' ? 'recipients' : kind === 'shared_drive' ? 'activity_mix'
        : kind === 'mixed' ? 'workflow_rate' : 'upload_workers',
      status: regression ? verdict ?? 'inconclusive'
        : running ? 'running' : lower === undefined ? 'inconclusive' : 'completed',
      bound: regression ? 'none' : bound,
      result: regression ? (verdict ?? 'inconclusive').replace(/^./, c => c.toUpperCase())
        : running ? 'Running' : lower === undefined ? 'Inconclusive' : describe(lower, upper, bound === 'exact'),
      file_size: size,
      upload_target: target,
      reason: (regression ? verdict === undefined : lower === undefined) ? 'missing_result' : 'recorded',
      // Recorded by the producer: legacy reports show an unknown environment and outcome.
      load_target: allowed(data.load_target, ['local', 'dev'], 'unknown'),
      mode: allowed(data.mode, ['capacity', 'regression'], 'capacity'),
      outcome: declared !== undefined ? String(declared?.outcome ?? 2)
        : [0, 1, 2].includes(data.outcome as number) ? String(data.outcome) : 'unknown',
      evidence: evidence === undefined ? 'none' : describe(evidence.lower, evidence.upper, evidence.exact),
    }, 1)
    emit('result_timestamp_seconds', base, timestamp)
    emit('capacity_lower', base, lower)
    emit('capacity_upper', base, upper)
    emit('capacity_evidence_lower', base, evidence?.lower)
    emit('capacity_evidence_upper', base, evidence?.upper)
    emit('result_value', { ...base, metric: 'started_at_seconds' }, startedAt)
    emit('result_value', { ...base, metric: 'ended_at_seconds' }, endedAt)
    const plan = record(data.plan)
    const roles = record(plan.vus_per_level)
    let values: Record<string, number> = {}
    if (kind === 'upload') {
      values = {
        ...numbers(record(data.criteria), {
          minimum_success_rate: 'minimum_success_rate',
          confirmation_iterations_per_vu: 'confirmation_iterations_per_vu',
        }),
        ...numbers({ ...data, p95_limit_ms: p95Limit }, {
          p95_limit_ms: 'p95_limit_ms', configured_max_vus: 'configured_max_vus',
        }),
      }
    } else if (kind === 'shared_drive') {
      values = {
        ...numbers(data, { cleanup_rate: 'cleanup_success_rate' }),
        ...numbers(record(data.criteria), {
          minimum_success_rate: 'minimum_success_rate', minimum_delivery_ratio: 'minimum_delivery_ratio',
          p95_limit_ms: 'p95_limit_ms',
        }),
        ...numbers(plan, { hold_ms: 'hold_ms' }),
        ...numbers(roles, {
          writers_per_level: 'writers', ws_readers_per_level: 'wsReaders',
          changes_readers_per_level: 'changesReaders',
        }),
      }
    } else if (kind === 'mixed') {
      const levels = records(data.levels)
      const sum = (key: string) => levels.length && levels.every(l => finite(l[key]))
        ? levels.reduce((total, l) => total + (l[key] as number), 0) : undefined
      // The highest rate that passed, and its slowest operation.
      const passing = levels.filter(l => l.status === 'passed' && Array.isArray(l.problems) && !l.problems.length).at(-1)
      const worst = passing && worstP95(passing)
      values = {
        ...numbers(record(data.criteria), { minimum_success_rate: 'minimum_success_rate', p95_limit_ms: 'p95_limit_ms' }),
        ...numbers(plan, { duration_s: 'duration_s' }),
        ...numbers({ workflows: sum('achieved_workflows'), dropped: sum('dropped_iterations'),
          highest: levels.length ? Math.max(...levels.map(l => finite(l.rate) ? l.rate : 0)) : undefined, worst }, {
          workflows_completed: 'workflows', dropped_workflows: 'dropped', highest_tested_rate: 'highest',
          worst_passing_p95_ms: 'worst',
        }),
      }
    } else {
      values = numbers(record(data.settings), {
        budget_ms: 'timeout_ms', stable_ms: 'stable_ms', observer_concurrency: 'observer_concurrency',
        configured_member_cap: 'effective_member_cap',
      })
    }
    for (const [metric, value] of Object.entries(values)) emit('result_value', { ...base, metric }, value)
    const rawPoints = kind === 'upload' ? data.attempts : kind === 'shared_drive' || kind === 'mixed' ? data.levels
      : data.points
    const points = Array.isArray(rawPoints) ? rawPoints : []
    const attempts = records(rawPoints)
    if (kind === 'upload' && attempts.length > 0) {
      const aggregate = (metric: string, get: (point: Record<string, unknown>) => unknown): void => {
        const amounts = attempts.map(get)
        if (amounts.every(finite)) emit('result_value', { ...base, metric }, amounts.reduce((a, b) => a + b, 0))
      }
      aggregate('failed_uploads', p => p.success_rate === 1 ? 0 : undefined)
      aggregate('payload_bytes', p => p.success_rate === 1 && finite(p.completed_iterations) && size !== 'n/a'
        ? p.completed_iterations * fileBytes[size]! : undefined)
      const latencies = attempts.map(p => p.p95_ms)
      if (latencies.every(finite)) emit('result_value', { ...base, metric: 'worst_final_p95_ms' }, Math.max(...latencies))
      const completed = attempts.map(p => p.completed_iterations)
      const planned = attempts.map(p => integer(p.vus) && integer(p.iterations_per_vu) ? p.vus * p.iterations_per_vu : null)
      const count = (amounts: unknown[]): string => amounts.every(finite) ? String(amounts.reduce((a, b) => a + b, 0)) : 'Not recorded'
      emit('work_info', { ...base, completed: `${attempts.every(p => p.success_rate === 1) ? count(completed) : 'Not recorded'} / ${count(planned)}`,
        readback: checks(kind, data).correctness === 'Verified' ? `${count(completed)} matched` : 'Readback coverage incomplete',
      }, 1)
    } else if (kind === 'shared_drive' && attempts.length > 0) {
      const ran = attempts.filter(level => finite(level.uploads) && level.uploads > 0)
      const matched = ran.reduce((sum, level) => sum + (finite(level.content_verified) ? level.content_verified : 0), 0)
      emit('work_info', { ...base, completed: `${attempts.length} recorded levels`,
        readback: checks(kind, { points: ran }).correctness === 'Verified' ? `${matched} content checks matched` : 'Content not verified',
      }, 1)
    } else if (kind === 'mixed' && attempts.length > 0) {
      const workflows = attempts.map(level => level.achieved_workflows)
      const checked = attempts.map(level => record(level.correctness))
      emit('work_info', { ...base,
        completed: workflows.every(finite) ? `${workflows.reduce((a, b) => a + b, 0)} workflows` : 'Not recorded',
        readback: checked.every(c => c.complete === true && finite(c.metadata_checks) && finite(c.upload_checks))
          ? `${checked.reduce((total, c) => total + (c.metadata_checks as number) + (c.upload_checks as number), 0)} responses checked`
          : 'Readback coverage incomplete',
      }, 1)
    } else if (kind === 'fanout' && attempts.length > 0) {
      emit('work_info', { ...base, completed: `${attempts.length} recorded recipient batches`,
        readback: recordedChecks.correctness,
      }, 1)
    }
    points.forEach((raw, index) => {
      const point = record(raw)
      const load = kind === 'upload' ? point.vus : kind === 'shared_drive' ? point.level
        : kind === 'mixed' ? point.rate : point.n
      if (!integer(load) || load === 0) return
      const phase = kind === 'upload' ? allowed(point.phase,
        ['baseline', 'discovery', 'discovery-retry', 'discovery-decider', 'confirmation', 'regression'], 'unknown')
        : kind === 'shared_drive' ? 'hold' : kind === 'mixed' ? 'rate'
        : allowed(point.phase, ['setup', 'add', 'remove', 'complete'], 'unknown')
      const recordedStatus = kind === 'upload' ? point.result : point.status
      const status = allowed(recordedStatus, kind === 'upload'
        ? ['passed', 'failed', 'infrastructure_failure', 'stopped']
        : kind === 'shared_drive' ? ['passed', 'failed', 'not_run', 'not_measured', 'inconclusive']
        : kind === 'mixed' ? ['passed', 'failed', 'generator_saturated', 'not_measured']
        : ['passed', 'failed', 'inconclusive'], 'missing_result')
      const dimensions = { ...base, index: String(index + 1), load: String(load), phase }
      const pointId = `${run}-point-${index + 1}`
      const legacyId = typeof point.summary_path === 'string'
        ? /^data\/results\/([A-Za-z0-9][A-Za-z0-9_-]{0,127})-summary\.json$/.exec(point.summary_path)?.[1] : null
      const executionId = typeof point.execution_id === 'string' && /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(point.execution_id)
        ? point.execution_id : legacyId ?? 'not-recorded'
      const pointStart = parseTimestamp(point.started_at)
      const pointEnd = parseTimestamp(point.ended_at)
      const pointWindow = pointStart !== null && pointEnd !== null && pointEnd >= pointStart
      const unit = kind === 'upload' ? 'workers' : kind === 'fanout' ? 'recipients'
        : kind === 'mixed' ? 'workflows/s' : 'mix units'
      const phaseTitle = phase.charAt(0).toUpperCase() + phase.slice(1)
      const planned = kind === 'upload' && integer(point.iterations_per_vu) ? load * point.iterations_per_vu
        : kind === 'mixed' && integer(point.offered_workflows) ? point.offered_workflows : null
      const complete = kind === 'upload' && integer(point.completed_iterations) ? point.completed_iterations
        : kind === 'mixed' && integer(point.achieved_workflows) ? point.achieved_workflows : null
      // The ramp cleans every level's uploads once, after the last level.
      const pointChecks = kind === 'upload' ? checks(kind, { attempts: [point] }) : checks(kind, { points: [point],
        ...kind === 'shared_drive' ? { cleanup: data.cleanup, cleanup_success_rate: data.cleanup_success_rate } : {} })
      const safeReason = reasonCategory(point.reason ?? point.error ?? (Array.isArray(point.reasons) ? point.reasons.join('; ') : ''), status === 'passed')
      const fanout = kind === 'fanout' ? fanoutValues(point) : {}
      const pointP95 = kind === 'mixed' ? worstP95(point) : point.p95_ms
      emit('point_evidence_info', { ...dimensions, execution_id: executionId,
        point_title: `${load} ${unit} · ${phaseTitle} · point ${index + 1}`, status,
        completed: complete === null ? 'Not recorded' : String(complete),
        planned: planned === null ? 'Not recorded' : String(planned),
        correctness: pointChecks.correctness, cleanup: pointChecks.cleanup,
        access: finite(fanout.recipient_access_proofs) ? `${fanout.recipient_access_proofs} / ${load}` : 'Not recorded',
        revoked: finite(fanout.owner_revoked) ? `${fanout.owner_revoked} / ${load}` : 'Not recorded',
        recipient_revoked: finite(fanout.recipient_revocation_proofs)
          ? `${fanout.recipient_revocation_proofs} / ${load}` : 'Not recorded',
        p95: finite(pointP95) ? `${pointP95.toFixed(1)} ms` : 'Not recorded',
        reason: safeReason,
        start_ms: !pointWindow ? finite(startedAt) ? String(Math.max(0, startedAt * 1000 - 60_000)) : ''
          : String(Math.max(0, pointStart - 30_000)),
        end_ms: !pointWindow ? finite(endedAt) ? String(endedAt * 1000 + 60_000) : ''
          : String(pointEnd + 60_000),
        window_recorded: pointWindow ? 'yes' : 'no',
        point_id: pointId,
        raw_start_ms: pointWindow ? String(pointStart) : finite(startedAt) ? String(startedAt * 1000) : '',
        raw_end_ms: pointWindow ? String(pointEnd) : finite(endedAt) ? String(endedAt * 1000) : '',
        inspect: 'Inspect →',
        point_window: pointWindow ? 'Point + context' : 'Run window (point time unavailable)',
        point_only: pointWindow ? 'Point only' : 'Run only (point time unavailable)',
        evidence: pointChecks.correctness === 'Failed' || status === 'failed' ? 'Failed'
          : pointChecks.cleanup === 'Retained' ? 'Retained'
          : pointChecks.correctness === 'Verified' && pointChecks.cleanup === 'Verified' ? 'Verified' : 'Unverified',
      }, 1)
      if (!['not_run', 'not_measured', 'missing_result'].includes(status)) {
        emit('point_value', { ...dimensions, executionid: executionId, metric: 'load' }, load)
        emit('point_value', { ...dimensions, executionid: executionId, metric: 'point_index' }, index + 1)
      }
      emit('point_info', {
        ...dimensions, status: status === 'missing_result' ? 'inconclusive' : status,
        reason: status === 'missing_result' ? 'missing_result' : 'recorded',
      }, 1)
      const values = kind === 'upload' ? numbers(point, {
        upload_p95_ms: 'p95_ms', success_rate: 'success_rate', cleanup_rate: 'cleanup_rate',
        consistency_rate: 'consistency_rate', completed_iterations: 'completed_iterations',
        dropped_iterations: 'dropped_iterations', iterations_per_vu: 'iterations_per_vu',
      }) : kind === 'shared_drive' ? numbers(point, {
        upload_p95_ms: 'upload_p95_ms', success_rate: 'upload_success_rate',
        ws_delivery_ratio: 'ws_delivery_ratio', changes_delivery_ratio: 'changes_delivery_ratio',
        completed_uploads: 'uploads',
      }) : kind === 'mixed' ? mixedValues(point) : fanoutValues(point)
      if (pointStart !== null && pointEnd !== null && pointEnd > pointStart) {
        values.started_at_seconds = pointStart / 1000
        values.ended_at_seconds = pointEnd / 1000
        if (point.success_rate === 1 && complete !== null) {
          values.point_uploads_per_second = complete / ((pointEnd - pointStart) / 1000)
        }
      }
      if (kind === 'shared_drive') {
        for (const [metric, key] of Object.entries({
          writers: 'writers', ws_readers: 'wsReaders', changes_readers: 'changesReaders',
        })) {
          if (finite(roles[key])) values[metric] = load * roles[key]
        }
      }
      if (kind === 'upload' && point.success_rate === 1 && complete !== null && size !== 'n/a') {
        values.payload_bytes = complete * fileBytes[size]!
        values.failed_uploads = 0
      }
      if (kind === 'upload' && planned !== null) values.planned_iterations = planned
      for (const [metric, value] of Object.entries(values)) {
        emit('point_value', { ...dimensions, executionid: executionId, metric }, value)
      }
    })
  }
  for (const state of ['valid', 'invalid', 'unsupported'] as const) {
    emit('exporter_reports', { state }, counts[state])
  }
  return output
}

export function renderMetrics(reports: ReportFile[], counts: { valid: number; invalid: number; unsupported: number }): string {
  const families = new Set<string>()
  const output: string[] = []
  for (const { name, labels, value } of getReportSamples(reports, counts)) {
    if (!families.has(name)) { output.push(`# TYPE ${name} gauge`); families.add(name) }
    const dimensions = Object.entries(labels).map(([key, text]) =>
      `${key}="${text.replaceAll('\\', '\\\\').replaceAll('\n', '\\n').replaceAll('"', '\\"')}"`).join(',')
    output.push(`${name}{${dimensions}} ${value}`)
  }
  return `${output.join('\n')}\n`
}
