import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

type Query = { group: string, datasource: { name: string }, spec: { expr?: string, query?: string } }
type Panel = { kind: string, spec: { id: number, title: string, description?: string,
  data: { spec: { queries: { spec: { query: Query } }[], transformations: { kind: string, spec: Record<string, unknown> }[] } },
  vizConfig: { group: string, spec: { options: Record<string, unknown>, fieldConfig: {
    defaults: Record<string, unknown>, overrides: { matcher: { options: string }, properties: { id: string, value: unknown }[] }[],
  } } } } }
type Variable = { kind: string, spec: { name: string, hide: string, includeAll?: boolean, allValue?: string,
  regex?: string, refresh?: string, query?: { spec: { query: string } } } }
type Layout = { kind: string, name?: string, spec?: { title?: string, hideHeader?: boolean, collapse?: boolean,
  rows?: Layout[], tabs?: Layout[], items?: Layout[], layout?: Layout, element?: Layout,
  x?: number, y?: number, width?: number, height?: number,
  conditionalRendering?: { spec: { items: { spec: { variable: string, operator: string, value: string } }[] } },
  variables?: Variable[],
} }
type Dashboard = { kind: string, apiVersion: string, metadata: { name: string }, spec: {
  variables: Variable[], elements: Record<string, Panel>, layout: Layout, links: { url: string }[],
} }
const dashboard = JSON.parse(readFileSync(new URL('../../config/grafana/dashboards/capacity-overview.json', import.meta.url), 'utf8')) as Dashboard
const panels = Object.values(dashboard.spec.elements)
function queries(panel: Panel): Query[] { return panel.spec.data.spec.queries.map(item => item.spec.query) }
function walk(layout: Layout): Layout[] {
  return [layout, ...[...(layout.spec?.rows ?? []), ...(layout.spec?.tabs ?? []), ...(layout.spec?.items ?? []),
    ...(layout.spec?.layout ? [layout.spec.layout] : []), ...(layout.spec?.element ? [layout.spec.element] : [])].flatMap(walk)]
}
function visible(layout: Layout, values: Record<string, string>): Panel[] {
  const rules = layout.spec?.conditionalRendering?.spec.items ?? []
  if (rules.some(({ spec: rule }) => rule.operator === 'equals' ? values[rule.variable] !== rule.value
    : rule.operator === 'notEquals' ? values[rule.variable] === rule.value : true)) return []
  if (layout.kind === 'ElementReference') return [dashboard.spec.elements[layout.name!]!]
  return [...(layout.spec?.rows ?? []), ...(layout.spec?.tabs ?? []), ...(layout.spec?.items ?? []),
    ...(layout.spec?.layout ? [layout.spec.layout] : []), ...(layout.spec?.element ? [layout.spec.element] : [])]
    .flatMap(child => visible(child, values))
}
function named(title: string): Panel { const panel = panels.find(p => p.spec.title === title); assert.ok(panel, title); return panel }

test('one native dashboard has three tabs and only Environment, Mode and Run in the main bar', () => {
  assert.equal(dashboard.apiVersion, 'dashboard.grafana.app/v2')
  assert.equal(dashboard.metadata.name, 'twake-drive-capacity')
  assert.equal(dashboard.spec.layout.kind, 'TabsLayout')
  assert.deepEqual(dashboard.spec.layout.spec!.tabs!.map(tab => tab.spec!.title), ['Overview', 'Workload', 'Infrastructure'])
  assert.deepEqual(dashboard.spec.variables.filter(v => v.spec.hide === 'dontHide').map(v => v.spec.name), ['env', 'mode', 'run'])
  for (const variable of dashboard.spec.variables.filter(v => ['env', 'mode', 'run'].includes(v.spec.name))) {
    assert.equal(variable.spec.includeAll, false)
  }
  assert.ok(!dashboard.spec.variables.some(v => ['size', 'target', 'concurrency', 'phase'].includes(v.spec.name)))
})

test('every layout reference is unique and resolves to a panel; each fixed row has no overlaps', () => {
  const nodes = walk(dashboard.spec.layout), refs = nodes.filter(n => n.kind === 'ElementReference').map(n => n.name!)
  assert.equal(new Set(refs).size, refs.length)
  assert.deepEqual(refs.slice().sort(), Object.keys(dashboard.spec.elements).sort())
  assert.equal(new Set(panels.map(p => p.spec.id)).size, panels.length)
  for (const grid of nodes.filter(n => n.kind === 'GridLayout')) {
    const items = grid.spec!.items!.map(i => i.spec!)
    for (const [index, a] of items.entries()) {
      assert.ok(a.x! >= 0 && a.x! + a.width! <= 24 && a.height! > 0)
      for (const b of items.slice(index + 1)) assert.equal(a.x! < b.x! + b.width! && b.x! < a.x! + a.width!
        && a.y! < b.y! + b.height! && b.y! < a.y! + a.height!, false)
    }
  }
})

test('workload-specific evidence is conditional on the run, independently of its environment', () => {
  for (const env of ['local', 'dev']) for (const kind of ['upload', 'shared_drive', 'fanout', 'mixed']) {
    const overview = visible(dashboard.spec.layout.spec!.tabs![0]!, { env, kind, mode: 'capacity', dev_source: '' })
    const titles = overview.map(p => p.spec.title)
    for (const title of ['Execution', 'Correctness', 'Cleanup', 'Performance']) assert.ok(titles.includes(title))
    assert.equal(titles.includes('Tested upload points'), kind === 'upload')
    assert.equal(titles.includes('Tested recipient batches'), kind === 'fanout')
    assert.equal(titles.includes('Tested activity levels'), kind === 'shared_drive')
    assert.equal(titles.includes('Uploads completed / planned'), kind === 'upload')
    assert.equal(titles.includes('Tested rates'), kind === 'mixed')
    if (kind === 'fanout') assert.ok(!overview.some(p => p.spec.vizConfig.group === 'timeseries'))
  }
})

test('a mixed search shows each rate, and its k6 runs follow the selected rates', () => {
  const overview = visible(dashboard.spec.layout.spec!.tabs![0]!, { env: 'local', kind: 'mixed', mode: 'capacity' })
  assert.deepEqual(overview.filter(p => p.spec.vizConfig.group === 'xychart').map(p => p.spec.title),
    ['Achieved rate by tested rate', 'p95 by tested rate'])
  const workload = visible(dashboard.spec.layout.spec!.tabs![1]!, { env: 'local', kind: 'mixed', mode: 'capacity' })
  const telemetry = workload.filter(p => p.spec.vizConfig.group === 'timeseries').flatMap(queries)
  assert.equal(telemetry.length, 2)
  for (const query of telemetry) assert.match(query.spec.expr!, /campaignid="\$run",.*executionid=~"\$execution"/)
  // Selected-rate totals show only when every selected rate recorded them.
  const completed = workload.find(p => p.spec.title === 'Workflows completed')!
  assert.match(queries(completed)[0]!.spec.expr!, /index=~"\$point_index".*== count\(cozy_load_point_evidence_info/)
})

test('regression does not present capacity curves and the producer owns the four verdicts', () => {
  const overview = visible(dashboard.spec.layout.spec!.tabs![0]!, { env: 'local', kind: 'upload', mode: 'regression' })
  assert.ok(!overview.some(p => p.spec.vizConfig.group === 'xychart'))
  for (const title of ['Execution', 'Correctness', 'Cleanup', 'Performance']) {
    assert.match(queries(named(title))[0]!.spec.expr!, /^cozy_load_verdict_info/)
  }
  assert.match(JSON.stringify(named('Performance')), /performance_detail/)
})

test('DEV infrastructure cannot display local metrics or query an absent remote datasource', () => {
  const tab = dashboard.spec.layout.spec!.tabs![2]!
  const missing = visible(tab, { env: 'dev', dev_source: '' })
  assert.ok(missing.some(p => p.spec.title === 'DEV infrastructure unavailable'))
  assert.ok(missing.flatMap(queries).every(q => q.datasource.name === 'load-reports'))
  const ready = visible(tab, { env: 'dev', dev_source: 'dev-influxdb' })
  assert.ok(!ready.some(p => p.spec.title === 'DEV infrastructure unavailable'))
  assert.ok(ready.flatMap(queries).filter(q => q.group === 'influxdb').length > 10)
  assert.ok(!ready.flatMap(queries).some(q => q.spec.expr?.includes('job="$stackjob"')))
  const local = visible(tab, { env: 'local', dev_source: '' })
  assert.ok(local.flatMap(queries).some(q => q.spec.expr?.includes('job="$stackjob"')))
  assert.ok(local.flatMap(queries).every(q => q.group !== 'influxdb'))
})

test('telemetry uses recorded attempt bounds pinned independently of the chart window', () => {
  for (const title of ['Successful uploads per second', 'Upload latency']) {
    for (const query of queries(named(title)).filter(q => q.datasource.name === 'prometheus')) {
      assert.match(query.spec.expr!, /campaignid="\$run",load_target="\$env",mode="\$mode"/)
      assert.match(query.spec.expr!, /executionid=~"\$execution"/)
      assert.match(query.spec.expr!, /metric="started_at_seconds"} @ \$report_at <= time\(\)/)
      assert.match(query.spec.expr!, /metric="ended_at_seconds"} @ \$report_at >= time\(\)/)
      assert.match(query.spec.expr!, /unless on\(executionid\)/)
      assert.doesNotMatch(query.spec.expr!, /avg\(|vector\(0\)/)
    }
  }
  const reportTime = dashboard.spec.variables.find(v => v.spec.name === 'report_at')!
  assert.match(reportTime.spec.query!.spec.query, /max\(timestamp\(cozy_load_run_info/)
  assert.equal(reportTime.spec.refresh, 'onTimeRangeChanged')
  // Grafana renders a query_result as '{} <value> <evaluation time in ms>'; @ takes the value in seconds.
  const reportRegex = new RegExp(reportTime.spec.regex!.slice(1, -1))
  assert.equal('{} 1793107927.232 1793108054978'.match(reportRegex)?.[1], '1793107927.232')
  for (const query of queries(named('Upload latency')).filter(q => q.datasource.name === 'prometheus')) {
    assert.match(query.spec.expr!, /and on\(\) \(count\(/)
    assert.match(query.spec.expr!, /== 1\)$/)
  }
  const workers = queries(named('Configured upload workers'))[0]!.spec.expr!
  assert.match(workers, /metric="load"} @ \$report_at/)
  assert.doesNotMatch(workers, /max\(k6_vus|vector\(0\)/)
  for (const name of ['execution', 'activity_level']) {
    const variable = dashboard.spec.variables.find(v => v.spec.name === name)!
    assert.match(variable.spec.query!.spec.query, /point_id=~"\$point"/)
    assert.equal(variable.spec.allValue, undefined, 'All expands only the selected point’s recorded values')
  }
})

test('untimed upload points use fresh worker samples without inventing point bounds', () => {
  const timelines = panels.filter(panel => ['Configured upload workers', 'Upload phases'].includes(panel.spec.title))
  for (const panel of timelines) {
    const expression = queries(panel)[0]!.spec.expr!
    assert.match(expression, /metric="load"} @ \$report_at and on\(executionid\)/)
    assert.match(expression, /k6_vus\{campaignid="\$run",load_target="\$env",mode="\$mode",executionid=~"\$execution"}/)
    assert.match(expression, /timestamp\(k6_vus\{.*}\) > time\(\) - 5/)
    assert.match(expression, /unless on\(executionid\) cozy_load_point_value\{.*metric="ended_at_seconds"} @ \$report_at/)
    assert.doesNotMatch(expression, /vector\(0\)/)
  }
})

test('run and point links carry both the actual identity and the recorded time window', () => {
  const window = named('Run time range')
  const windowLinks = window.spec.vizConfig.spec.fieldConfig.overrides
    .flatMap(o => o.properties.filter(p => p.id === 'links').flatMap(p => p.value as { url: string }[]))
  const all = windowLinks.find(l => l.url.includes('dtab=overview') && l.url.includes('focus_from_ms'))!
  assert.ok(all)
  assert.match(all.url, /var-point=\$__all/)
  assert.match(all.url, /from=\$\{__data.fields.focus_from_ms\}&to=\$\{__data.fields.focus_to_ms\}/)
  assert.doesNotMatch(all.url, /now-14d|__url_time_range/)
  const table = named('Tested upload points')
  const load = table.spec.vizConfig.spec.fieldConfig.overrides.find(o => o.matcher.options === 'Inspect')!
  const link = (load.properties.find(p => p.id === 'links')!.value as { url: string }[])[0]!.url
  const expanded = link.replace('${env:percentencode}', 'local').replace('${mode:percentencode}', 'capacity')
    .replace('${run:percentencode}', 'actual-run').replace('${__data.fields.point_id}', 'actual-run-point-5')
    .replace('${__data.fields.start_ms}', '1000').replace('${__data.fields.end_ms}', '2000')
  const url = new URL(expanded, 'https://grafana.invalid')
  assert.equal(url.pathname, '/d/twake-drive-capacity')
  assert.equal(url.searchParams.get('var-run'), 'actual-run')
  assert.equal(url.searchParams.get('var-point'), 'actual-run-point-5')
  assert.equal(url.searchParams.get('dtab'), 'workload')
  assert.equal(url.searchParams.get('from'), '1000')
  assert.equal(url.searchParams.get('to'), '2000')
  assert.doesNotMatch(expanded, /\$/)
  assert.ok(!dashboard.spec.links.some(link => link.url.includes('now-14d')))
})

test('report facts use their own datasource and every tab can restore the full recorded window', () => {
  for (const panel of panels) for (const query of queries(panel)) {
    if (query.spec.expr?.includes('cozy_load_')) {
      assert.equal(query.datasource.name, query.spec.expr.includes('@ $report_at') ? 'prometheus' : 'load-reports')
    }
    if (query.spec.expr?.includes('k6_')) assert.equal(query.datasource.name, 'prometheus')
  }
  for (const tab of dashboard.spec.layout.spec!.tabs!) {
    const displayed = visible(tab, { env: 'local', kind: 'upload', mode: 'capacity' })
    const titles = displayed.map(p => p.spec.title)
    assert.ok(titles.includes('Selected run'))
    assert.ok(titles.includes('Run time range'))
    const controls = displayed.find(p => p.spec.title === 'Run time range')!
    for (const url of controls.spec.vizConfig.spec.fieldConfig.overrides
      .flatMap(o => o.properties.filter(p => p.id === 'links').flatMap(p => p.value as { url: string }[]))) {
      if (url.url.includes('dtab=')) assert.ok(url.url.includes(`dtab=${tab.spec!.title!.toLowerCase()}`))
    }
  }
  const point = dashboard.spec.variables.find(v => v.spec.name === 'point')!
  assert.match(point.spec.regex!, /point_id/)
})

test('partial measurements cannot turn a selected-point failure count into zero', () => {
  const selected = panels.filter(p => p.spec.title === 'Failed uploads')
    .flatMap(queries).find(q => q.spec.expr?.includes('cozy_load_point_value'))!
  assert.match(selected.spec.expr!, /and on\(\).*count\(cozy_load_point_value.*== count\(cozy_load_point_evidence_info/)
})

test('table links retain their hidden identity/time fields after transformations', () => {
  for (const panel of panels.filter(p => p.spec.vizConfig.group === 'table')) {
    const links = panel.spec.vizConfig.spec.fieldConfig.overrides
      .flatMap(o => o.properties.filter(p => p.id === 'links').flatMap(p => p.value as { url: string }[]))
    const options = panel.spec.data.spec.transformations.find(t => t.kind === 'organize')?.spec.options as
      { excludeByName?: Record<string, boolean>, includeByName?: Record<string, boolean> } | undefined
    for (const link of links) for (const match of link.url.matchAll(/\$\{__data\.fields\.([a-z_]+)\}/g)) {
      assert.ok(!options?.excludeByName?.[match[1]!], `${panel.spec.title}: ${match[1]} removed`)
      if (options?.includeByName) assert.ok(options.includeByName[match[1]!])
      const hidden = panel.spec.vizConfig.spec.fieldConfig.overrides.find(o => o.matcher.options === match[1])
      assert.ok(hidden?.properties.some(p => p.id === 'custom.hideFrom.viz' && p.value === true),
        `${panel.spec.title}: ${match[1]} must stay available to links and hidden in Grafana 13.2`)
    }
    assert.ok(!panel.spec.vizConfig.spec.fieldConfig.overrides.some(o => o.properties.some(p => p.id === 'custom.hidden')))
  }
})

test('an unrelated time window shows a recovery action instead of empty workload timelines', () => {
  const tab = dashboard.spec.layout.spec!.tabs![1]!
  const values = { env: 'local', kind: 'upload', mode: 'capacity', window_state: 'outside' }
  const outside = visible(tab, values).map(p => p.spec.title)
  assert.ok(outside.includes('Selected time range is outside this run or point'))
  assert.ok(outside.includes('Selected run'))
  for (const title of ['Upload phases', 'Successful uploads per second', 'Upload latency', 'Configured upload workers']) {
    assert.ok(!outside.includes(title), title)
    assert.ok(visible(tab, { ...values, window_state: 'inside' }).some(p => p.spec.title === title), title)
  }
  const recovery = named('Selected time range is outside this run or point')
  const links = recovery.spec.vizConfig.spec.fieldConfig.defaults.links as { url: string }[]
  assert.match(links[0]!.url, /var-point=\$__all/)
  assert.match(links[0]!.url, /from=\$\{__field.labels.focus_from_ms\}&to=\$\{__field.labels.focus_to_ms\}/)
})

test('point controls allow switching directly from one selected point to another', () => {
  for (const panel of panels.filter(p => p.spec.title === 'Choose a point')) {
    assert.doesNotMatch(queries(panel)[0]!.spec.expr!, /point_id=~"\$point"/)
  }
  const selectors = walk(dashboard.spec.layout).filter(node => node.spec?.title === 'Point: ${point:text} · change')
  assert.equal(selectors.length, 2)
  assert.ok(selectors.every(node => node.spec!.collapse))
  assert.ok(!panels.some(p => p.spec.title === 'Selected point'))
})

test('summary ratios use the last measured batch and keep recipient revocation distinct from owner status', () => {
  for (const title of ['Access verified · last batch', 'Revocation verified · last batch', 'Delivery · last selected level']) {
    for (const panel of panels.filter(p => p.spec.title === title)) for (const query of queries(panel)) {
      assert.match(query.spec.expr!, /and on\(run,index\) topk\(1, .*metric="point_index"/)
      assert.doesNotMatch(query.spec.expr!, /owner_revoked|vector\(0\)/)
    }
  }
  assert.match(JSON.stringify(named('Revocation verified · last batch').spec.data.spec.transformations), /recipient_revoked/)
})

test('the window check refreshes with the time picker and follows the selected point with a run-time fallback', () => {
  const state = dashboard.spec.variables.find(v => v.spec.name === 'window_state')!
  assert.equal(state.spec.refresh, 'onTimeRangeChanged')
  assert.match(state.spec.query!.spec.query, /\$\{__from\}/)
  assert.match(state.spec.query!.spec.query, /\$\{__to\}/)
  assert.match(state.spec.query!.spec.query, /index=~"\$point_index"/)
  assert.match(state.spec.query!.spec.query, /or on\(run\) cozy_load_result_value/)
})

test('local diagnostics distinguish no HTTP errors, missing scrapes, process cores and actual GC quantile labels', () => {
  const cpu = named('Peak process CPU')
  assert.equal(cpu.spec.vizConfig.spec.fieldConfig.defaults.unit, 'suffix: cores')
  const gc = named('Stack GC pause')
  assert.match(queries(gc)[0]!.spec.expr!, /quantile=~"1\|1.0"/)
  const errors = queries(named('Stack HTTP error rate'))[0]!.spec.expr!
  assert.match(errors, /or 0 \* sum\(increase\(http_all_total_duration_count/)
  assert.doesNotMatch(errors, /vector\(0\)/)
})

test('old dashboard bookmarks contain only a link to the shared views', () => {
  for (const name of ['k6-overview', 'shared-drive', 'local-infrastructure']) {
    const alias = JSON.parse(readFileSync(new URL(`../../config/grafana/dashboards/${name}.json`, import.meta.url), 'utf8')) as { panels: { options: { content: string }, targets?: unknown[] }[] }
    assert.equal(alias.panels.length, 1)
    assert.match(alias.panels[0]!.options.content, /\/d\/twake-drive-capacity\?/)
    assert.equal(alias.panels[0]!.targets, undefined)
  }
})
