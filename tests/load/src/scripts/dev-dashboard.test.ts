import assert from 'node:assert/strict'
import { existsSync, readFileSync } from 'node:fs'
import test from 'node:test'

const view = new URL('../../config/grafana/dashboards-dev/dev-infrastructure.json', import.meta.url)
const dashboard = JSON.parse(readFileSync(view, 'utf8'))
const panels = dashboard.panels.filter((p: { type: string }) => p.type !== 'row')
const queries: string[] = [
  ...panels.map((p: { targets: Array<{ query: string }> }) => p.targets[0]!.query),
  ...dashboard.templating.list.map((v: { query: string }) => v.query),
]

test('the DEV view stays out of the dashboards every installation provisions', () => {
  assert.equal(existsSync(new URL('../../config/grafana/dashboards/dev-infrastructure.json', import.meta.url)), false)
  assert.equal(dashboard.uid, 'twake-drive-dev-infra')
})

test('the DEV view names no host or bucket and reads only DEV data', () => {
  for (const item of [...panels, ...dashboard.templating.list]) {
    assert.deepEqual(item.datasource, { type: 'influxdb', uid: 'dev-influxdb' }, item.title ?? item.name)
  }
  for (const query of queries) {
    assert.match(query, /bucket: v\.defaultBucket/)
    assert.match(query, /r\.env == "dev"/)
    // Hosts come from the variables, never from a literal in a public file.
    assert.doesNotMatch(query, /r\.host ==|r\.host =~ \/\^[a-z]/)
  }
})

test('DEV counters are differentiated per series before they are summed', () => {
  for (const query of queries.filter(q => /derivative\(/.test(q))) {
    assert.match(query, /derivative\(unit: 1s, nonNegative: true\)[\s\S]*aggregateWindow[\s\S]*sum\(\)/)
    assert.ok(query.indexOf('derivative(') < query.indexOf('sum()'))
  }
  const durations = queries.find(q => q.includes('workers_exec_durations'))!
  assert.match(durations, /r\.sum \/ r\.count/)
})
