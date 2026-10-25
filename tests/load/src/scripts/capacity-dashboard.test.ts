import assert from 'node:assert/strict'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import test, { type TestContext } from 'node:test'

import { readResults } from './serve-capacity-results.ts'

const dashboardPath = new URL('../../config/grafana/dashboards/capacity-overview.json', import.meta.url)

type Target = { expr: string, instant: boolean, range: boolean, format: string, refId: string }
type Override = { matcher: { id: string, options: string }, properties: { id: string, value: unknown }[] }
type Panel = {
  id: number
  title: string
  type: string
  description: string
  datasource?: { type: string, uid: string }
  gridPos: { x: number, y: number, w: number, h: number }
  targets?: Target[]
  panels?: Panel[]
  collapsed?: boolean
  options: { textMode?: string, mode?: string, content?: string, [key: string]: unknown }
  fieldConfig?: { defaults: Record<string, unknown>, overrides: Override[] }
  transformations?: { id: string, options: Record<string, unknown> }[]
}
type Dashboard = {
  uid: string
  title: string
  panels: Panel[]
  templating: { list: {
    name: string, type: string, multi: boolean, includeAll: boolean, allValue?: string,
    query: { query: string }, datasource: { uid: string },
  }[] }
  links: { url: string, keepTime: boolean, includeVars: boolean }[]
}

function readDashboard(): Dashboard {
  assert.ok(existsSync(dashboardPath), 'the versioned native capacity dashboard must exist')
  return JSON.parse(readFileSync(dashboardPath, 'utf8')) as Dashboard
}

function allPanels(panels: Panel[]): Panel[] {
  return panels.flatMap(panel => [panel, ...allPanels(panel.panels ?? [])])
}

function panelByTitle(dashboard: Dashboard, title: string): Panel {
  const panel = allPanels(dashboard.panels).find(panel => panel.title === title)
  assert.ok(panel, `missing native panel: ${title}`)
  return panel
}

test('native capacity summaries distinguish upload workers, activity units and recipient batches', () => {
  const dashboard = readDashboard()
  assert.equal(dashboard.uid, 'twake-drive-capacity')
  assert.equal(dashboard.title, 'Stack Capacity Overview')
  const panels = allPanels(dashboard.panels)
  assert.equal(new Set(panels.map(panel => panel.id)).size, panels.length)
  for (const panel of allPanels(dashboard.panels)) {
    assert.ok(['stat', 'table', 'text', 'row', 'xychart'].includes(panel.type), panel.type)
    assert.ok(panel.gridPos.x >= 0 && panel.gridPos.x + panel.gridPos.w <= 24)
    assert.ok(panel.gridPos.h > 0 && panel.gridPos.y >= 0)
    if (panel.targets?.length) assert.deepEqual(panel.datasource, { type: 'prometheus', uid: 'prometheus' })
    if (panel.type === 'text') {
      assert.equal(panel.options.mode, 'markdown')
      assert.doesNotMatch(panel.options.content ?? '', /<iframe|<script|<style|<html/i)
    }
  }
  const upload = panelByTitle(dashboard, 'Upload worker capacity')
  const activity = panelByTitle(dashboard, 'Shared-drive activity capacity')
  const fanout = panelByTitle(dashboard, 'Group member batch capacity')
  assert.deepEqual([upload.gridPos.y, activity.gridPos.y, fanout.gridPos.y], [0, 0, 0])
  assert.match(activity.description, /profile/i)
  assert.match(activity.description, /one identity/i)
  assert.match(activity.description, /not distinct users/i)
  assert.doesNotMatch(JSON.stringify(activity), /NOT MEASURED/)
  for (const [panel, kind] of [[upload, 'upload'], [activity, 'shared_drive'], [fanout, 'fanout']] as const) {
    assert.equal(panel.type, 'stat', 'the actual report result must lead, not a raw metadata table')
    assert.equal(panel.targets?.length, 1)
    const selectedRun = kind === 'upload' ? '$upload_run' : '$run'
    assert.equal(panel.targets?.[0]?.expr,
      `cozy_load_result_info{campaign=~"$campaign",run=~"${selectedRun}",kind="${kind}"}`)
    assert.equal(panel.targets?.[0]?.format, 'table', 'expose each report as a row, not an info gauge')
    assert.equal(panel.fieldConfig?.defaults.noValue, 'No run')
    assert.equal(panel.fieldConfig?.defaults.unit, 'none')
    assert.equal(panel.options.textMode, 'value_and_name', 'strings render as values, not invisible names')
    assert.equal(panel.options.graphMode, 'none')
    assert.deepEqual(panel.options.reduceOptions, { values: true, calcs: ['lastNotNull'], fields: '/.*/' })
    assert.deepEqual(panel.transformations, [{
      id: 'rowsToFields',
      options: { mappings: [
        { fieldName: 'run', handlerKey: 'field.name' },
        { fieldName: 'result', handlerKey: 'field.value' },
      ] },
    }], 'result label is the value; every report row becomes a field with untouched metadata labels')
    const name = String(panel.fieldConfig?.defaults.displayName)
    assert.match(name, /\$\{__field.labels.status\}/)
    assert.doesNotMatch(name, /__field.labels.reason/, 'raw evidence codes belong in expert detail')
    assert.doesNotMatch(name, /__field.name|__field.labels.run/, 'raw run IDs do not dominate the cards')
    assert.ok(Array.isArray(panel.fieldConfig?.defaults.mappings), 'bound/state colors use native mappings')
  }
  assert.match(upload.description, /not measured in-flight requests/i)
  assert.match(fanout.description, /all N/i)
  assert.match(fanout.description, /access/i)
})

test('upload summaries select one visible size and target instead of shrinking every campaign result', () => {
  const dashboard = readDashboard()
  const size = dashboard.templating.list.find(variable => variable.name === 'size')
  const target = dashboard.templating.list.find(variable => variable.name === 'target')
  const selected = dashboard.templating.list.find(variable => variable.name === 'upload_run')
  for (const variable of [size, target]) {
    assert.ok(variable, 'the original overview has explicit upload size and target controls')
    assert.equal(variable.includeAll, false, 'a headline compares one declared upload workload')
    assert.equal(variable.multi, false)
  }
  assert.ok(selected, 'derive the upload run selection from recorded labels, never filename patterns')
  assert.equal(selected.includeAll, true, 'retain retries and inconclusive reports for the selected workload')
  assert.equal(selected.allValue, undefined, 'All must expand matching options, not bypass the workload filter')
  assert.match(selected.query.query, /run=~"\$run"/)
  assert.match(selected.query.query, /file_size="\$size"/)
  assert.match(selected.query.query, /upload_target="\$target"/)
  for (const title of ['Upload worker capacity', 'Upload p95 · by workers', 'Upload success · by workers']) {
    assert.match(panelByTitle(dashboard, title).targets![0]!.expr, /run=~"\$upload_run"/)
  }
})

test('one campaign and optional run selection never aggregate or replace absent report evidence', () => {
  const dashboard = readDashboard()
  const variables = dashboard.templating.list
  assert.deepEqual(variables.map(variable => variable.name), ['campaign', 'run', 'size', 'target', 'upload_run'])
  const [campaign, run] = variables
  assert.equal(campaign?.multi, false)
  assert.equal(campaign?.includeAll, false, 'campaigns cannot be silently mixed')
  assert.equal(campaign?.query.query, 'label_values(cozy_load_result_info, campaign)')
  assert.equal(run?.multi, false)
  assert.equal(run?.includeAll, true)
  assert.equal(run?.allValue, '.*')
  assert.equal(run?.query.query, 'label_values(cozy_load_result_info{campaign=~"$campaign"}, run)')
  for (const variable of variables) assert.equal(variable.datasource.uid, 'prometheus')
  const families = [
    'cozy_load_result_info', 'cozy_load_result_value', 'cozy_load_point_info',
    'cozy_load_point_value', 'cozy_load_result_timestamp_seconds',
  ]
  for (const panel of allPanels(dashboard.panels)) {
    for (const target of panel.targets ?? []) {
      assert.ok(families.some(family => target.expr.startsWith(`${family}{`)), target.expr)
      assert.match(target.expr, /campaign=~"\$campaign"/)
      assert.match(target.expr, /run=~"\$(run|upload_run)"/)
      assert.equal(target.instant, true)
      assert.equal(target.range, false)
      assert.doesNotMatch(target.expr, /\b(max|min|sum|avg|topk|bottomk|vector|timestamp|time)\s*\(/)
      assert.doesNotMatch(target.expr, /\bor\b|\[[^\]]*\]|\boffset\b/)
      assert.doesNotMatch(target.expr, /status="completed"/, 'inconclusive and running reports must stay visible')
    }
  }
  const semantics = panelByTitle(dashboard, 'Reading the evidence').options.content ?? ''
  for (const state of ['No run', 'NOT MEASURED', 'Inconclusive', 'Running', 'Not recorded']) {
    assert.ok(semantics.includes(state), `explain ${state} without inventing a zero`)
  }
})

test('scenario tables retain scalar labels, criteria and each actual tested point without joins', () => {
  const dashboard = readDashboard()
  const scenarios = [['Upload', 'upload'], ['Group batch', 'fanout'], ['Activity mix', 'shared_drive']] as const
  const suffixes = [
    ['runs - bounds and status', 'cozy_load_result_info', true],
    ['criteria', 'cozy_load_result_value', false],
    ['tested points', 'cozy_load_point_info', true],
    ['point evidence', 'cozy_load_point_value', false],
  ] as const
  for (const [title, kind] of scenarios) {
    for (const [suffix, family, info] of suffixes) {
      const panel = panelByTitle(dashboard, `${title} ${suffix}`)
      assert.equal(panel.type, 'table')
      assert.equal(panel.targets?.length, 1, 'one scalar table frame; no fragile label joins')
      const target = panel.targets![0]!
      assert.equal(target.expr, `${family}{campaign=~"$campaign",run=~"$run",kind="${kind}"}`)
      assert.equal(target.format, 'table', 'instant Prometheus table responses already expose labels as fields')
      assert.equal(panel.fieldConfig?.defaults.noValue, 'Not recorded')
      assert.deepEqual(panel.transformations?.map(transform => transform.id), ['organize'])
      const organize = panel.transformations![0]!.options
      const excluded = organize.excludeByName as Record<string, boolean>
      assert.equal(excluded.Time, true, 'query evaluation time is not report or point time')
      assert.equal(excluded.Value === true, info, 'hide only the info gauge; retain measured evidence')
      for (const field of ['campaign', 'run', 'kind']) assert.notEqual(excluded[field], true)
      const fields = info && family.includes('result')
        ? ['status', 'bound', 'result', 'reason', 'scope', 'file_size', 'upload_target']
        : family.includes('point') ? ['index', 'load', 'phase', info ? 'status' : 'metric'] : ['metric']
      for (const field of fields) assert.notEqual(excluded[field], true, `${title}: keep ${field}`)
    }
  }
  const criteria = panelByTitle(dashboard, 'Group batch criteria').description
  assert.match(criteria, /budget_ms/)
  assert.match(criteria, /stable_ms/)
  const evidence = panelByTitle(dashboard, 'Group batch point evidence').description
  assert.match(evidence, /owner_ready/)
  assert.match(evidence, /recipient_access_proofs/)
  assert.match(panelByTitle(dashboard, 'Group batch revoke evidence').description, /not started.*not success/i)
  assert.match(panelByTitle(dashboard, 'Activity mix runs - bounds and status').description, /one identity/)
  assert.ok(panelByTitle(dashboard, 'Activity mix runs - bounds and status').gridPos.y > 0)
})

test('result tables put conclusions before repeated run metadata', () => {
  const dashboard = readDashboard()
  for (const scenario of ['Upload', 'Group batch', 'Activity mix']) {
    const panel = panelByTitle(dashboard, `${scenario} runs - bounds and status`)
    const order = panel.transformations![0]!.options.indexByName as Record<string, number>
    assert.deepEqual(Object.keys(order).sort((a, b) => order[a]! - order[b]!), [
      'result', 'status', 'bound', 'reason', 'file_size', 'upload_target', 'scope', 'campaign', 'run', 'kind',
    ], `${scenario}: classified conclusion must not be offscreen behind metadata`)
  }
})

test('scalar criteria and evidence put metrics and measured values before repeated metadata', () => {
  const panels = allPanels(readDashboard().panels).filter(panel => panel.type === 'table' &&
    panel.targets?.some(target => /^cozy_load_(result|point)_value\{/.test(target.expr)))
  assert.deepEqual(panels.map(panel => panel.title).sort(), [
    'Activity mix criteria', 'Activity mix point evidence', 'Group batch criteria',
    'Group batch point evidence', 'Group batch revoke evidence', 'Upload criteria', 'Upload point evidence',
  ])
  for (const panel of panels) {
    assert.deepEqual(panel.transformations?.map(transform => transform.id), ['organize'])
    const organize = panel.transformations![0]!.options
    const order = organize.indexByName as Record<string, number>
    const pointFields = panel.targets![0]!.expr.startsWith('cozy_load_point_value{')
      ? ['load', 'index', 'phase'] : []
    assert.deepEqual(Object.keys(order).sort((a, b) => order[a]! - order[b]!), [
      'metric', 'Value', ...pointFields, 'campaign', 'run', 'kind',
    ], `${panel.title}: numeric evidence must not be offscreen behind metadata`)
    assert.deepEqual(organize.excludeByName, { Time: true, __name__: true, job: true, instance: true },
      'reordering must not hide any evidence or run metadata')
    assert.deepEqual(organize.renameByName, {})
  }
})

test('freshness uses report mtime and missing provenance stays explicit', () => {
  const dashboard = readDashboard()
  const freshness = panelByTitle(dashboard, 'Report update time (filesystem mtime)')
  assert.equal(freshness.type, 'table')
  assert.equal(freshness.targets?.[0]?.expr,
    'cozy_load_result_timestamp_seconds{campaign=~"$campaign",run=~"$run"} * 1000')
  assert.equal(freshness.fieldConfig?.defaults.unit, 'none', 'campaign, run and kind are text, not dates')
  assert.deepEqual(freshness.fieldConfig?.overrides, [{
    matcher: { id: 'byName', options: 'Report updated at' },
    properties: [{ id: 'unit', value: 'dateTimeAsIso' }],
  }], 'only the renamed numeric report timestamp receives date formatting')
  assert.deepEqual(freshness.transformations?.map(transform => transform.id), ['organize'])
  const organize = freshness.transformations![0]!.options
  assert.deepEqual(organize.renameByName, { Value: 'Report updated at' })
  const excluded = organize.excludeByName as Record<string, boolean>
  for (const field of ['campaign', 'run', 'kind', 'Value']) assert.notEqual(excluded[field], true)
  assert.match(freshness.description, /not test start|not run start/i)
  const provenance = panelByTitle(dashboard, 'Recorded environment and timing')
  assert.match(provenance.options.content ?? '', /Not recorded/)
  assert.match(provenance.options.content ?? '', /version.*resources.*revision/i)
  assert.match(provenance.description, /start.*end.*Not recorded/i)
  assert.doesNotMatch(JSON.stringify(dashboard), /localhost|127\.0\.0\.1|file:\/\/|demo|synthetic/i)

  const scope = panelByTitle(dashboard, 'Diagnostic scope').options.content ?? ''
  assert.match(scope, /campaign only/i, 'the old activity dashboard has no execution variable')
  assert.match(scope, /selected.*range/i)
})

test('the provisioned layout has no overlaps and sources fit 115 columns except native drilldown URLs', () => {
  function checkLayout(panels: Panel[]) {
    for (const [index, panel] of panels.entries()) {
      for (const other of panels.slice(index + 1)) {
        const a = panel.gridPos
        const b = other.gridPos
        const overlaps = a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h
        assert.equal(overlaps, false, `${panel.title} overlaps ${other.title}`)
      }
      checkLayout(panel.panels ?? [])
    }
  }
  checkLayout(readDashboard().panels)
  // JSON cannot split strings. Keep the explicit filters rather than duplicate var-run via __all_variables.
  const selfLink = '/d/twake-drive-capacity?${__url_time_range}&var-campaign=$campaign' +
    '&var-size=$size&var-target=$target&var-run='
  const nativeUrls = new Set(['${__field.name}', '${__data.fields.run}'].map(token => `${selfLink}${token}`))
  for (const source of [dashboardPath, new URL(import.meta.url)]) {
    const wide = readFileSync(source, 'utf8').split('\n').flatMap((line, index) => {
      const url = line.trim().match(/^"(.+)",$/u)?.[1]
      const nativeUrl = source === dashboardPath && url !== undefined && nativeUrls.has(url)
      return line.length > 115 && !nativeUrl ? [`${index + 1}: ${line.length} columns`] : []
    })
    assert.deepEqual(wide, [], `${source.pathname} must remain reviewable`)
  }
})

test('overview point evidence follows upload controls while retaining non-upload workloads', () => {
  const points = panelByTitle(readDashboard(), 'Tested points · outcome and reason')
  assert.deepEqual(points.targets?.map(target => target.expr), [
    'cozy_load_point_info{campaign=~"$campaign",run=~"$run",kind!="upload"}',
    'cozy_load_point_info{campaign=~"$campaign",run=~"$upload_run",kind="upload"}',
  ])
  assert.equal(points.transformations?.[0]?.id, 'merge',
    'concatenate actual rows with run, kind and attempt intact; never reduce them into a new verdict')
  const organize = points.transformations!.find(transform => transform.id === 'organize')!.options
  const hidden = organize.excludeByName as Record<string, boolean>
  for (const field of ['Value', 'Value #A', 'Value #B']) {
    assert.equal(hidden[field], true, 'info-gauge columns must stay hidden after the native merge')
  }
})

test('overview leads with conclusions and actual tested points, with expert detail collapsed', () => {
  const dashboard = readDashboard()
  const expert = panelByTitle(dashboard, 'Expert detail · reports, criteria and diagnostics')
  assert.equal(expert.type, 'row')
  assert.equal(expert.collapsed, true, 'technical inventory must not consume the opening viewport')
  const conclusions = panelByTitle(dashboard, 'Conclusions · each report')
  const points = panelByTitle(dashboard, 'Tested points · outcome and reason')
  assert.deepEqual(dashboard.panels.filter(panel => panel.type === 'table'), [points],
    'one compact evidence table leads; the complete report inventory stays in expert detail')
  assert.ok(allPanels(expert.panels ?? []).includes(conclusions))
  assert.ok(points.gridPos.y >= 5 && points.gridPos.y < expert.gridPos.y)
  for (const panel of [conclusions, points]) {
    assert.equal(panel.type, 'table')
    assert.ok(panel.gridPos.h <= 6)
    assert.equal(panel.gridPos.w, 24, 'one readable table, not two cramped metadata inventories')
    assert.equal(panel.options.cellHeight, 'sm')
    const organize = panel.transformations!.find(transform => transform.id === 'organize')!.options
    const hidden = organize.excludeByName as Record<string, boolean>
    for (const field of ['Time', 'Value', '__name__', 'job', 'instance', 'campaign']) {
      assert.equal(hidden[field], true, `${panel.title} hides repeated technical ${field}`)
    }
    const run = panel.fieldConfig!.overrides.find(override => override.matcher.options === 'run')
    assert.ok(run?.properties.some(property => property.id === 'custom.hidden' && property.value === true),
      'retain the exact run for data links but hide it from the front-screen columns')
    assert.equal(panel.targets?.length, panel === points ? 2 : 1,
      'keep the selected upload scope and non-upload rows without reducing any report')
  }
  assert.match(conclusions.targets![0]!.expr, /^cozy_load_result_info/)
  assert.match(points.targets![0]!.expr, /^cozy_load_point_info/)
  const conclusionOrder = conclusions.transformations![0]!.options.indexByName as Record<string, number>
  assert.ok(conclusionOrder.result! < conclusionOrder.run!)
  const pointOrder = points.transformations!.find(transform => transform.id === 'organize')!
    .options.indexByName as Record<string, number>
  assert.ok(pointOrder.load! < pointOrder.run!)
  for (const field of ['load', 'phase', 'status', 'reason', 'index']) {
    assert.ok(field in pointOrder, `every actual point retains ${field}`)
  }
  for (const title of ['Reading the evidence', 'Bound semantics',
    'Report update time (filesystem mtime)', 'Recorded environment and timing',
    'Upload runs - bounds and status', 'Group batch runs - bounds and status',
    'Activity mix runs - bounds and status']) {
    assert.ok(allPanels(expert.panels ?? []).includes(panelByTitle(dashboard, title)), title)
  }
})

test('native load curves plot measured scalars against numeric load without combining reports or phases', () => {
  const dashboard = readDashboard()
  const definitions = [
    ['Upload p95 · by workers', 'upload', 'upload_p95_ms', 'ms'],
    ['Upload success · by workers', 'upload', 'success_rate', 'percentunit'],
    ['Group add elapsed · by recipients', 'fanout', 'add_elapsed_ms', 'ms'],
  ] as const
  for (const [title, kind, metric, unit] of definitions) {
    const panel = panelByTitle(dashboard, title)
    assert.equal(panel.type, 'xychart', 'a static classified report is not a chronological time series')
    assert.ok(dashboard.panels.includes(panel))
    assert.ok(panel.gridPos.y >= panelByTitle(dashboard, 'Tested points · outcome and reason').gridPos.y + 6)
    assert.ok(panel.gridPos.y < panelByTitle(dashboard,
      'Expert detail · reports, criteria and diagnostics').gridPos.y)
    const selectedRun = kind === 'upload' ? '$upload_run' : '$run'
    assert.equal(panel.targets?.[0]?.expr,
      `cozy_load_point_value{campaign=~"$campaign",run=~"${selectedRun}",kind="${kind}"}`)
    assert.equal(panel.targets?.[0]?.format, 'table')
    assert.deepEqual(panel.transformations?.map(transform => transform.id), [
      'filterByValue', 'organize', 'convertFieldType', 'sortBy', 'partitionByValues',
    ])
    const [filter, organize, convert, sort, partition] = panel.transformations!
    assert.deepEqual(filter!.options, { type: 'include', match: 'all', filters: [
      { fieldName: 'metric', config: { id: 'equal', options: { value: metric } } },
    ] })
    assert.deepEqual(convert!.options, { conversions: [{ targetField: 'load', destinationType: 'number' }] })
    assert.deepEqual(sort!.options, { sort: [{ field: 'load', desc: false }] })
    assert.deepEqual(partition!.options, {
      fields: ['run', 'phase'], keepFields: false, naming: { asLabels: true },
    }, 'never draw one curve through different runs or confirmation phases')
    const excluded = organize!.options.excludeByName as Record<string, boolean>
    assert.equal(excluded.Time, true, 'query evaluation time is not a report point')
    assert.notEqual(excluded.Value, true)
    assert.deepEqual(organize!.options.indexByName, { load: 0, Value: 1, run: 2, phase: 3, index: 4 })
    assert.equal(panel.options.mapping, 'auto', 'native XY auto selects first numeric field as X')
    assert.deepEqual(panel.options.legend, { showLegend: false },
      'raw run IDs belong in tooltips, not a tall legend')
    assert.equal(panel.fieldConfig?.defaults.unit, 'none', 'load is never formatted as milliseconds or percent')
    const measured = panel.fieldConfig?.overrides.find(override => override.matcher.options === 'Value')
    assert.ok(measured?.properties.some(property => property.id === 'unit' && property.value === unit))
    assert.equal(panel.fieldConfig?.defaults.noValue, 'Not recorded')
    assert.doesNotMatch(panel.description, /throughput|time.to.ready/i)
  }
  assert.equal(allPanels(dashboard.panels).some(panel => panel.type === 'timeseries'), false)
  const notes = panelByTitle(dashboard, 'Available measurement scope').options.content ?? ''
  assert.match(notes, /throughput.*not (exported|recorded)/i)
  assert.match(notes, /readiness.*(no|not).*time.series/i)
  assert.match(notes, /p95.*not.*gate/i, 'an observed p95 is not a fabricated SLO')
})

test('native XY selects worker and value fields across every frame, not only the first baseline', () => {
  for (const panel of allPanels(readDashboard().panels).filter(panel => panel.type === 'xychart')) {
    assert.equal((panel as Panel & { pluginVersion?: string }).pluginVersion, '13.2.0',
      'the installed XY migration must not treat current matcher objects as legacy string fields')
    assert.deepEqual(panel.options.series, [{
      x: { matcher: { id: 'byName', options: 'load' } },
      y: { matcher: { id: 'byName', options: 'Value' } },
    }], 'explicit field matchers prevent a default frame-zero migration hiding discovery and confirmation')
  }
})

test('native states preserve bound text and color exported statuses without green unknowns', () => {
  const dashboard = readDashboard()
  type RegexMapping = { type: string, options: { pattern: string, result: { color: string, text?: string } } }
  for (const title of [
    'Upload worker capacity', 'Shared-drive activity capacity', 'Group member batch capacity',
  ]) {
    const panel = panelByTitle(dashboard, title)
    const mappings = panel.fieldConfig!.defaults.mappings as RegexMapping[]
    for (const [result, color] of [
      ['=57', 'green'], ['= 57', 'green'], ['>=64', 'blue'], ['>= 64', 'blue'],
      ['[8,16)', 'blue'], ['[8, 16)', 'blue'], ['Inconclusive', 'orange'],
      ['Running', 'blue'], ['Unknown', 'text'], ['No run', 'text'],
    ]) {
      const mapping = mappings.find(mapping => new RegExp(mapping.options.pattern).test(result!))
      assert.equal(mapping?.options.result.color, color, `${title}: ${result}`)
      assert.equal(mapping?.options.result.text, undefined, 'never replace the measured result string')
    }
    assert.deepEqual(panel.fieldConfig!.defaults.color, { mode: 'fixed', fixedColor: 'text' })
    assert.ok(mappings.every(mapping => !new RegExp(mapping.options.pattern).test('1')),
      'the info-gauge value 1 is never a displayed capacity')
    const links = panel.fieldConfig!.defaults.links as { url: string }[]
    assert.match(links?.[0]?.url ?? '', /var-run=\$\{__field.name\}/,
      'a compact label still opens the exact original report run')
  }
  for (const title of ['Conclusions · each report', 'Tested points · outcome and reason']) {
    const panel = panelByTitle(dashboard, title)
    const outcome = panel.fieldConfig!.overrides.find(override => override.matcher.options === 'Outcome')!
    const maps = outcome.properties.find(property => property.id === 'mappings')!.value as
      { options: Record<string, { color: string, text: string }> }[]
    const states = maps[0]!.options
    for (const [status, color] of [
      ['passed', 'green'], ['failed', 'red'], ['infrastructure_failure', 'orange'],
      ['inconclusive', 'orange'], ['running', 'blue'], ['not_run', 'text'],
      ['not_measured', 'text'], ['unknown', 'text'],
    ]) assert.equal(states[status!]?.color, color, `${title}: actual producer status ${status}`)
    assert.equal(states.completed?.color, 'blue', 'completed does not mean every point passed')
    const reason = panel.fieldConfig!.overrides.find(override =>
      override.matcher.options === 'Deciding evidence')
    assert.ok(reason?.properties.some(property => property.id === 'mappings'),
      'classified evidence codes have readable display labels, not rewritten source identifiers')
  }
})

test('native self-drilldowns route to the exact report with upload filters and selected time intact', () => {
  const panels = allPanels(readDashboard().panels)
  const campaign = '20261023T112515Z-all-load'
  const run = `${campaign}-personal-1K`
  const sources = [
    ['Upload worker capacity', '${__field.name}'],
    ['Shared-drive activity capacity', '${__field.name}'],
    ['Group member batch capacity', '${__field.name}'],
    ['Conclusions · each report', '${__data.fields.run}'],
    ['Tested points · outcome and reason', '${__data.fields.run}'],
  ] as const
  for (const [title, runToken] of sources) {
    const panel = panels.find(panel => panel.title === title)!
    const links = [panel.fieldConfig!.defaults.links,
      ...panel.fieldConfig!.overrides.flatMap(override =>
        override.properties.filter(property => property.id === 'links').map(property => property.value))]
      .flat().filter(Boolean) as { title: string, url: string, targetBlank: boolean }[]
    assert.equal(links.length, panel.type === 'stat' ? 1 : 2, `${title}: test every native report link`)
    for (const link of links) {
      assert.equal(link.title, 'Inspect this report')
      assert.equal(link.targetBlank, false)
      assert.ok(link.url.includes(runToken), `${title}: use the clicked report, not the All-run selection`)
      for (const time of ['from=now-2d&to=now&timezone=browser', 'from=1000&to=2000']) {
        const expanded = link.url.replaceAll(runToken, run)
          .replaceAll('$campaign', campaign).replaceAll('$size', '1K').replaceAll('$target', 'personal')
          .replace('${__url_time_range}', time)
        // Grafana resolves native data links against the app base, not the current dashboard path.
        const url = new URL(expanded, 'https://grafana.invalid/')
        assert.equal(url.pathname, '/d/twake-drive-capacity', `${title}: never navigate to Grafana Home`)
        assert.deepEqual([...url.searchParams], [
          ...new URLSearchParams(time), ['var-campaign', campaign], ['var-size', '1K'],
          ['var-target', 'personal'], ['var-run', run],
        ], `${title}: preserve nondefault filters and time, with one exact run and no stale derived selection`)
        assert.doesNotMatch(expanded, /\$/u, 'all native data-link tokens must be accounted for')
      }
    }
  }
})

test('dashboard fixtures do not depend on a machine-specific temporary directory', () => {
  assert.doesNotMatch(readFileSync(new URL(import.meta.url), 'utf8'), /['"]\/(home|Users)\/[^/]+\//)
})

function linkResults(t: TestContext) {
  const root = mkdtempSync(join(tmpdir(), 'capacity-dashboard-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))
  for (const run of ['standalone-upload', 'parent-upload']) {
    writeFileSync(join(root, `${run}-upload-capacity.json`),
      JSON.stringify({ campaign_id: run }))
  }
  writeFileSync(join(root, 'parent-activity-shared-drive-ramp.json'), '{}')
  const campaign = join(root, 'campaigns', 'parent')
  mkdirSync(campaign, { recursive: true })
  writeFileSync(join(campaign, 'environment.txt'), 'run_id=parent\n')
  writeFileSync(join(campaign, 'steps.tsv'), [
    'upload\tupload-capacity\tparent-upload\t\t\t0',
    'activity\tshared-drive-ramp\tparent-activity\t\t\t0',
  ].join('\n'))
  const snapshot = readResults(root)
  assert.deepEqual(snapshot.counts, { valid: 3, invalid: 0, unsupported: 0 })
  return snapshot.reports
}

function rowTelemetryUrl(row: { campaign: string, run: string, kind: string }, selectedRun = row.run): URL {
  const scenario = row.kind === 'upload' ? 'Upload' : 'Activity mix'
  const panel = panelByTitle(readDashboard(), `${scenario} runs - bounds and status`)
  const run = panel.fieldConfig?.overrides.find(override =>
    override.matcher.id === 'byName' && override.matcher.options === 'run')
  const links = run?.properties.find(property => property.id === 'links')?.value as
    { url: string, targetBlank: boolean }[] | undefined
  assert.equal(links?.length, 1, 'drill down from an actual report row, not a campaign-wide guess')
  assert.equal(links![0]!.targetBlank, false)
  const expanded = links![0]!.url.replaceAll('${__value.raw}', row.run)
    .replaceAll('$campaign', row.campaign).replaceAll('$run', selectedRun)
    .replace('${__url_time_range}', 'from=1000&to=2000')
  const url = new URL(expanded, 'https://grafana.invalid')
  assert.equal(url.searchParams.get('from'), '1000',
    'the link supplies ?, because the rendered Grafana time-range token does not')
  assert.equal(url.searchParams.get('to'), '2000', 'preserve selected time, never infer it from report mtime')
  return url
}

function uploadTelemetry(run: string) {
  const makefile = readFileSync(new URL('../../Makefile', import.meta.url), 'utf8')
  const recipe = makefile.split('upload: upload-binary observability-up\n')[1]!.split('\nupload-soak:')[0]!
  assert.ok(recipe.includes('--tag campaignid="$(or $(CAPACITY_RUN_ID),$(EXECUTION_ID))"'))
  assert.ok(recipe.includes('--tag executionid="$(EXECUTION_ID)"'))
  const producer = readFileSync(new URL('./upload-campaign.ts', import.meta.url), 'utf8')
  const runner = readFileSync(new URL('./upload/exact-work.ts', import.meta.url), 'utf8')
  assert.ok(producer.includes('campaignId: config.campaignId,') && producer.includes('    executionId,\n'))
  assert.ok(runner.includes('`CAPACITY_RUN_ID=${work.campaignId}`'))
  assert.ok(runner.includes('`EXECUTION_ID=${work.executionId}`'))
  const template = producer.match(/const executionId = `([^`]+)`/)?.[1]
  assert.ok(template, 'derive attempt IDs from the actual upload producer')
  return ['baseline', 'confirmation'].map((phase, index) => ({
    campaignid: run,
    executionid: template.replace('${config.campaignId}', run).replace('${phase}', phase)
      .replace('${vus}', '8').replace('${state.attemptNumber}', String(index + 1)),
  }))
}

function assertUploadSelection(url: URL, run: string) {
  assert.equal(url.pathname, '/d/twake-drive-k6')
  const target = JSON.parse(readFileSync(
    new URL('../../config/grafana/dashboards/k6-overview.json', import.meta.url), 'utf8')) as Dashboard
  const execution = target.templating.list.find(variable => variable.name === 'executionid')
  assert.equal(execution?.includeAll, true)
  assert.equal(execution?.allValue, undefined, 'use Grafana All, not an escaped literal regex value')
  for (const tags of uploadTelemetry(run)) {
    assert.notEqual(tags.executionid, run, 'a report run is not an individual k6 execution')
    assert.equal(url.searchParams.get('var-campaignid'), tags.campaignid)
  }
  assert.equal(url.searchParams.get('var-executionid'), '$__all', 'include every phase/VU/attempt')
}

test('standalone upload row links select its producer campaign and all attempt executions', t => {
  const row = linkResults(t).find(result => result.run === 'standalone-upload')!
  assert.equal(row.campaign, row.run)
  assertUploadSelection(rowTelemetryUrl(row), row.run)
})

test('grouped upload row links select the step campaign, not the reader parent campaign', t => {
  const row = linkResults(t).find(result => result.run === 'parent-upload')!
  assert.equal(row.campaign, 'parent', 'real steps.tsv membership changes only the reader campaign')
  assertUploadSelection(rowTelemetryUrl(row), row.run)
})

test('grouped activity row links select the producer step without an unsupported execution filter', t => {
  const row = linkResults(t).find(result => result.run === 'parent-activity')!
  assert.equal(row.campaign, 'parent')
  const makefile = readFileSync(new URL('../../Makefile', import.meta.url), 'utf8')
  const recipe = makefile.split('shared-drive-ramp-run: upload-binary observability-up\n')[1]!.split('\n\n')[0]!
  assert.ok(recipe.includes('--tag campaignid="$(EXECUTION_ID)"'))
  assert.ok(!recipe.includes('--tag executionid='))
  const target = JSON.parse(readFileSync(
    new URL('../../config/grafana/dashboards/shared-drive.json', import.meta.url), 'utf8')) as Dashboard
  assert.ok(!target.templating.list.some(variable => variable.name === 'executionid'))
  const url = rowTelemetryUrl(row)
  assert.equal(url.pathname, '/d/twake-drive-shared-drive')
  assert.equal(url.searchParams.get('var-campaignid'), row.run, 'EXECUTION_ID is the step/report run')
  assert.equal(url.searchParams.has('var-executionid'), false)
})

test('All-run selection offers only row-scoped links and cannot mix unrelated producer campaigns', t => {
  const dashboard = readDashboard()
  assert.deepEqual(dashboard.links, [], 'a parent campaign cannot map to one k6 campaignid')
  assert.equal(dashboard.templating.list.find(variable => variable.name === 'run')?.allValue, '.*')
  const rows = linkResults(t).filter(result => result.campaign === 'parent')
  assert.deepEqual(rows.map(row => row.run).sort(), ['parent-activity', 'parent-upload'])
  for (const row of rows) {
    const url = rowTelemetryUrl(row, '.*')
    assert.equal(url.searchParams.get('var-campaignid'), row.run)
    assert.notEqual(url.searchParams.get('var-campaignid'), 'standalone-upload')
    if (row.kind === 'upload') assertUploadSelection(url, row.run)
    else assert.equal(url.searchParams.has('var-executionid'), false)
  }
})
