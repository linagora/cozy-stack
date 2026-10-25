export type ReportFile = {
  campaign: string
  run: string
  kind: 'upload' | 'shared_drive' | 'fanout'
  timestamp: number
  startedAt?: number | undefined
  endedAt?: number | undefined
  data: Record<string, unknown>
}

type Labels = Record<string, string>

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

// A regression run's verdict, by its recorded status; it never claims a capacity.
const regressionVerdicts: Record<string, string> = {
  completed: 'passed', regression_failure: 'failed', correctness_failure: 'failed',
  infrastructure_failure: 'inconclusive', running: 'running',
}

function numbers(source: Record<string, unknown>, mapping: Record<string, string>): Record<string, number> {
  return Object.fromEntries(Object.entries(mapping).flatMap(([metric, key]) =>
    finite(source[key]) ? [[metric, source[key]]] : []))
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
  if (remove.status === 'not_started') values.remove_started = 0
  if (allowed(remove.status, ['observing', 'passed', 'failed', 'inconclusive'], '') !== '') {
    values.remove_started = 1
  }
  if (allowed(remove.status, ['passed', 'failed', 'inconclusive'], '') !== '') {
    values.remove_verified = remove.status === 'passed' ? 1 : 0
  }
  return values
}

export function renderMetrics(
  reports: ReportFile[],
  counts: { valid: number; invalid: number; unsupported: number },
): string {
  const output: string[] = []
  const families = new Set<string>()
  const emit = (name: string, dimensions: Labels, value: unknown) => {
    if (!finite(value)) return
    const labels = Object.entries(dimensions).map(([key, text]) => {
      const escaped = text.replaceAll('\\', '\\\\').replaceAll('\n', '\\n').replaceAll('"', '\\"')
      return `${key}="${escaped}"`
    }).join(',')
    if (!families.has(name)) {
      output.push(`# TYPE cozy_load_${name} gauge`)
      families.add(name)
    }
    output.push(`cozy_load_${name}{${labels}} ${value}`)
  }
  for (const { campaign, run, kind, data, timestamp, startedAt, endedAt } of reports) {
    const base = { campaign, run, kind }
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
    const describe = (low: number, high: number | undefined, exact: boolean) =>
      high !== undefined ? `[${low}, ${high})` : exact ? `= ${low}` : `>= ${low}`
    const verdict = regression ? regressionVerdicts[String(data.status)] : undefined
    emit('result_info', {
      ...base,
      scope: kind === 'fanout' ? 'recipients' : kind === 'shared_drive' ? 'activity_mix' : 'upload_workers',
      status: regression ? verdict ?? 'inconclusive'
        : running ? 'running' : lower === undefined ? 'inconclusive' : 'completed',
      bound: regression ? 'none' : bound,
      result: regression ? (verdict ?? 'inconclusive').replace(/^./, c => c.toUpperCase())
        : running ? 'Running' : lower === undefined ? 'Inconclusive' : describe(lower, upper, bound === 'exact'),
      file_size: allowed(data.file_size, ['1K', '100K', '1M', '10M', '100M', '1G'], 'n/a'),
      upload_target: allowed(data.upload_target, ['shared-drive', 'personal'], 'n/a'),
      reason: (regression ? verdict === undefined : lower === undefined) ? 'missing_result' : 'recorded',
      // Recorded by the producer: legacy reports show an unknown environment and outcome.
      load_target: allowed(data.load_target, ['local', 'dev'], 'unknown'),
      mode: allowed(data.mode, ['capacity', 'regression'], 'capacity'),
      outcome: [0, 1, 2].includes(data.outcome as number) ? String(data.outcome) : 'unknown',
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
        ...numbers(data, { p95_limit_ms: 'p95_limit_ms', configured_max_vus: 'configured_max_vus' }),
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
    } else {
      values = numbers(record(data.settings), {
        budget_ms: 'timeout_ms', stable_ms: 'stable_ms', observer_concurrency: 'observer_concurrency',
        configured_member_cap: 'effective_member_cap',
      })
    }
    for (const [metric, value] of Object.entries(values)) emit('result_value', { ...base, metric }, value)
    const rawPoints = kind === 'upload' ? data.attempts : kind === 'shared_drive' ? data.levels : data.points
    const points = Array.isArray(rawPoints) ? rawPoints : []
    points.forEach((raw, index) => {
      const point = record(raw)
      const load = kind === 'upload' ? point.vus : kind === 'shared_drive' ? point.level : point.n
      if (!integer(load) || load === 0) return
      const phase = kind === 'upload' ? allowed(point.phase,
        ['baseline', 'discovery', 'discovery-retry', 'discovery-decider', 'confirmation', 'regression'], 'unknown')
        : kind === 'shared_drive' ? 'hold'
        : allowed(point.phase, ['setup', 'add', 'remove', 'complete'], 'unknown')
      const recordedStatus = kind === 'upload' ? point.result : point.status
      const status = allowed(recordedStatus, kind === 'upload'
        ? ['passed', 'failed', 'infrastructure_failure', 'stopped']
        : kind === 'shared_drive' ? ['passed', 'failed', 'not_run', 'not_measured', 'inconclusive']
        : ['passed', 'failed', 'inconclusive'], 'missing_result')
      const dimensions = { ...base, index: String(index + 1), load: String(load), phase }
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
      }) : fanoutValues(point)
      if (kind === 'shared_drive') {
        for (const [metric, key] of Object.entries({
          writers: 'writers', ws_readers: 'wsReaders', changes_readers: 'changesReaders',
        })) {
          if (finite(roles[key])) values[metric] = load * roles[key]
        }
      }
      for (const [metric, value] of Object.entries(values)) emit('point_value', { ...dimensions, metric }, value)
    })
  }
  for (const state of ['valid', 'invalid', 'unsupported'] as const) {
    emit('exporter_reports', { state }, counts[state])
  }
  return `${output.join('\n')}\n`
}
