import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

type Query = { group: string, datasource: { name: string }, spec: { query?: string } }
type Panel = { spec: { title: string, description: string, data: { spec: { queryOptions: { interval?: string }, queries: { spec: { query: Query } }[] } }, vizConfig: { group: string, spec: { fieldConfig: { defaults: { custom?: { drawStyle: string, spanNulls: boolean } } } } } } }
const dashboard = JSON.parse(readFileSync(new URL('../../config/grafana/dashboards/capacity-overview.json', import.meta.url), 'utf8')) as { spec: { elements: Record<string, Panel> } }
const panels = Object.values(dashboard.spec.elements).filter(p => p.spec.data.spec.queries.some(q => q.spec.query.group === 'influxdb'))
const queries = panels.flatMap(p => p.spec.data.spec.queries.map(q => q.spec.query.spec.query!))

test('DEV panels use only the dedicated datasource, default bucket and DEV data without private host inventory', () => {
  assert.ok(panels.length > 10)
  for (const panel of panels) for (const { spec: { query } } of panel.spec.data.spec.queries) {
    assert.equal(query.datasource.name, '$dev_source')
    assert.match(query.spec.query!, /bucket: v\.defaultBucket/)
    assert.match(query.spec.query!, /r\.env == "dev"/)
    assert.doesNotMatch(query.spec.query!, /r\.host ==|r\.host =~ \/\^[a-z]/)
  }
})

test('DEV counters differentiate before aggregation and host filtering applies to worker panels', () => {
  for (const query of queries.filter(q => q.includes('derivative('))) {
    assert.ok(query.indexOf('derivative(') < query.indexOf('sum()'))
  }
  for (const query of queries.filter(q => q.includes('workers_'))) assert.match(query, /r\.host =~ \/\^\$\{stack:regex\}\$\//)
  assert.match(queries.find(q => q.includes('workers_exec_durations'))!, /r\.sum \/ r\.count/)
})

test('DEV samples remain discrete, preserve missing intervals and never pool host p99 values', () => {
  for (const panel of panels.filter(p => p.spec.vizConfig.group === 'timeseries')) {
    assert.equal(panel.spec.data.spec.queryOptions.interval, '1m')
    assert.equal(panel.spec.vizConfig.spec.fieldConfig.defaults.custom!.drawStyle, 'points')
    assert.equal(panel.spec.vizConfig.spec.fieldConfig.defaults.custom!.spanNulls, false)
  }
  for (const query of queries) assert.doesNotMatch(query, /fill\(|createEmpty: true/)
  const p99 = panels.find(p => p.spec.title.includes('p99'))!
  assert.match(p99.spec.description, /Not a pooled percentile/)
  assert.match(JSON.stringify(p99), /group\(columns: \[\\"host\\"\]\)/)
})

test('DEV host metrics share the Stack host selector and peak RSS/CPU combine hosts before taking a peak', () => {
  for (const query of queries) assert.doesNotMatch(query, /\$\{node:regex\}/)
  for (const panel of panels.filter(p => p.spec.title.includes('selected hosts combined'))) {
    const query = panel.spec.data.spec.queries[0]!.spec.query.spec.query!
    assert.match(query, /group\(columns: \["_time"\]\)\n  \|> sum\(\)\n  \|> group\(columns: \[\]\)\n  \|> max\(\)/)
  }
})
