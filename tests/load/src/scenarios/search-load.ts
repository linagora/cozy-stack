import { check, sleep } from 'k6'
import http from 'k6/http'
import { Trend } from 'k6/metrics'
import type { Options } from 'k6/options'

import { parseDurationToMs } from '../lib/duration.ts'
import { parseHosts, parsePositiveIntegerEnv } from '../lib/env.ts'

// Searches as members of 1, 10 or 200 drives, or of the big drive of many folders (search-load.ts
// prepares them and passes their tokens in SEARCH_USERS), at a fixed arrival rate per group; arrivals
// do not wait for slow responses. OpenSearch's CPU is sampled meanwhile.

// group is the drive count, or big.
interface User { url: string, token: string, group: string }

const executionId = __ENV.EXECUTION_ID || 'search'
const users: User[] = JSON.parse(__ENV.SEARCH_USERS || '[]')
if (!users.length) throw new Error('SEARCH_USERS is empty: run make search-load, which passes the members')
const groups = [...new Set(users.map((u) => u.group))]
const rate = parsePositiveIntegerEnv(__ENV, 'SEARCH_RATE', 5)
const durationMs = parseDurationToMs(__ENV.SEARCH_DURATION || '1m')
const maxVus = parsePositiveIntegerEnv(__ENV, 'SEARCH_MAX_VUS', 50)
// The words of the fixture's file names, and starts of words being typed.
const QUERIES = ['report', 'budget', 'invoice', 'meeting', 'notes', 'plan', 'photo', 'contract', 'draft', 'summary',
  'rep', 'budg', 'inv', 'meet', 'contr']

export const opensearchCPU = new Trend('search_opensearch_cpu_percent')

export const options: Options = {
  hosts: parseHosts(__ENV.ACTOR_HOSTS),
  scenarios: {
    ...Object.fromEntries(groups.map((group) => [`drives_${group}`, {
      executor: 'constant-arrival-rate',
      rate,
      timeUnit: '1s',
      duration: `${durationMs}ms`,
      preAllocatedVUs: Math.min(maxVus, rate * 2),
      maxVUs: maxVus,
      exec: 'search',
      env: { GROUP: group },
      tags: { drives: group },
    }])),
    opensearch: { executor: 'constant-vus', vus: 1, duration: `${durationMs}ms`, exec: 'sampleCPU' },
  },
  // Always passing: they only keep a submetric per group in the summary.
  thresholds: Object.fromEntries(groups.flatMap((group) => [
    [`http_req_duration{drives:${group}}`, ['p(95)>=0']],
    [`http_req_failed{drives:${group}}`, ['rate>=0']],
  ])),
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)', 'count'],
}

const pick = <T>(list: T[]): T => list[Math.floor(Math.random() * list.length)]!

export function search(): void {
  const user = pick(users.filter((u) => u.group === __ENV.GROUP))
  const res = http.post(`${user.url}/files/search?page[limit]=20`, JSON.stringify({ q: pick(QUERIES) }), {
    headers: { Authorization: `Bearer ${user.token}`, 'Content-Type': 'application/json' },
    tags: { name: 'POST /files/search' },
  })
  check(res, { 'search answered': (r) => r.status === 200 && Array.isArray(r.json('data')) })
}

export function sampleCPU(): void {
  const res = http.get('http://opensearch:9200/_nodes/stats/process', { tags: { name: 'opensearch stats' } })
  const nodes = res.status === 200 ? Object.values(res.json('nodes') as Record<string, any>) : []
  for (const node of nodes) {
    if (typeof node?.process?.cpu?.percent === 'number') opensearchCPU.add(node.process.cpu.percent)
  }
  sleep(2)
}

export function handleSummary(data: { metrics: Record<string, { values?: Record<string, number> }> }):
  Record<string, string> {
  const value = (metric: string, stat: string) => data.metrics[metric]?.values?.[stat]
  const report = {
    rate_per_s: rate,
    duration_s: durationMs / 1000,
    levels: groups.map((drives) => ({
      drives,
      users: users.filter((u) => u.group === drives).length,
      searches: value(`http_req_duration{drives:${drives}}`, 'count') ?? 0,
      p50_ms: value(`http_req_duration{drives:${drives}}`, 'med'),
      p95_ms: value(`http_req_duration{drives:${drives}}`, 'p(95)'),
      error_rate: value(`http_req_failed{drives:${drives}}`, 'rate'),
    })),
    opensearch_cpu_percent: { avg: value('search_opensearch_cpu_percent', 'avg'),
      max: value('search_opensearch_cpu_percent', 'max') },
    dropped_iterations: value('dropped_iterations', 'count') ?? 0,
  }
  const ms = (v?: number) => (v === undefined ? 'n/a' : `${Math.round(v)}`)
  const lines = report.levels.map((l) => `  ${l.drives === 'big' ? 'the big drive' : `${l.drives} drive(s)`}, ` +
    `${l.users} user(s): ${l.searches} searches, ` +
    `p50 ${ms(l.p50_ms)} ms, p95 ${ms(l.p95_ms)} ms, errors ${l.error_rate ?? 'n/a'}`)
  return {
    stdout: ['', `Search load at ${rate} searches/s per group for ${report.duration_s} s`, ...lines,
      `  OpenSearch CPU: avg ${ms(report.opensearch_cpu_percent.avg)} %, max ${ms(report.opensearch_cpu_percent.max)} %`,
      `  dropped iterations: ${report.dropped_iterations}`, ''].join('\n'),
    [`/results/${executionId}-search-load.json`]: `${JSON.stringify(report, null, 2)}\n`,
  }
}
