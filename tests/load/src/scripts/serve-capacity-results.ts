import {
  closeSync, constants, fstatSync, openSync, opendirSync, readSync, realpathSync,
} from 'node:fs'
import { createServer } from 'node:http'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import { renderMetrics, type ReportFile } from './capacity-metrics.ts'
import { isJsonRecord } from './capacity/k6-summary.ts'

const finite = (value: unknown): value is number =>
  typeof value === 'number' && Number.isFinite(value) && value >= 0
const safeId = (value: unknown): value is string =>
  typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$/.test(value)

export type Snapshot = { reports: ReportFile[]; counts: { valid: number; invalid: number; unsupported: number } }
type Limits = { maxEntries: number; maxReports: number; maxFileBytes: number; maxTotalBytes: number }
const defaults: Limits = {
  maxEntries: 2048, maxReports: 128, maxFileBytes: 1024 * 1024, maxTotalBytes: 8 * 1024 * 1024,
}
const reportPattern = /-(upload-capacity|upload-regression|shared-drive-ramp|org-drive-fanout)\.json$/
// A producer's own recorded time, for runs outside a campaign.
const seconds = (value: unknown): number | undefined => {
  const time = typeof value === 'string' ? Date.parse(value) : NaN
  return Number.isNaN(time) ? undefined : time / 1000
}

function entries(path: string, budget: { entries: number }, limits: Limits): string[] {
  const dir = opendirSync(path)
  const names: string[] = []
  try {
    for (let entry = dir.readSync(); entry !== null; entry = dir.readSync()) {
      if (++budget.entries > limits.maxEntries) throw new Error('work_limit')
      names.push(entry.name)
    }
  } finally {
    dir.closeSync()
  }
  return names.sort()
}

function readBounded(path: string, budget: { bytes: number }, limits: Limits) {
  const fd = openSync(path, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK)
  try {
    const before = fstatSync(fd)
    if (!before.isFile() || before.nlink !== 1 || before.size > limits.maxFileBytes) {
      throw new Error('unsafe_file')
    }
    budget.bytes += before.size
    if (budget.bytes > limits.maxTotalBytes) throw new Error('work_limit')
    const buffer = Buffer.alloc(before.size + 1)
    let length = 0
    while (length < buffer.length) {
      const count = readSync(fd, buffer, length, buffer.length - length, null)
      if (count === 0) break
      length += count
    }
    const after = fstatSync(fd)
    if (length !== before.size || after.size !== before.size || after.mtimeMs !== before.mtimeMs) {
      throw new Error('changed_file')
    }
    return { text: buffer.subarray(0, length).toString('utf8'), timestamp: before.mtimeMs / 1000 }
  } finally {
    closeSync(fd)
  }
}

type Link = { campaign: string; kind: string; values: Record<string, number> }

function missing(error: unknown): boolean {
  return error !== null && typeof error === 'object' && 'code' in error && error.code === 'ENOENT'
}

function optionalText(path: string, budget: { bytes: number }, limits: Limits): string {
  try {
    return readBounded(path, budget, limits).text
  } catch (error) {
    if (missing(error)) return ''
    throw error
  }
}

function campaignLinks(root: string, budget: { bytes: number; entries: number }, limits: Limits) {
  const links = new Map<string, Link | null>()
  let fd: number
  try {
    fd = openSync(join(root, 'campaigns'), constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW)
  } catch (error) {
    if (missing(error)) return links
    throw new Error('unsafe_campaign')
  }
  try {
    for (const name of entries(`/proc/self/fd/${fd}`, budget, limits)) {
      if (!safeId(name)) throw new Error('unsafe_campaign')
      const child = openSync(join(`/proc/self/fd/${fd}`, name),
        constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW)
      try {
        const pinned = `/proc/self/fd/${child}`
        const env = optionalText(join(pinned, 'environment.txt'), budget, limits)
        const ids = env.split('\n').filter(line => line.startsWith('run_id=')).map(line => line.slice(7))
        const campaign = ids.length === 0 ? name : ids[0]
        if (ids.length > 1 || !safeId(campaign)) throw new Error('unsafe_campaign')
        const steps = optionalText(join(pinned, 'steps.tsv'), budget, limits)
        for (const line of steps.split('\n')) {
          if (line === '') continue
          if (++budget.entries > limits.maxEntries) throw new Error('work_limit')
          const [, kind, run, started, ended] = line.split('\t')
          if (!run) continue
          if (!safeId(run)) throw new Error('unsafe_campaign')
          const values: Record<string, number> = {}
          const start = Date.parse(started ?? '') / 1000
          const end = Date.parse(ended ?? '') / 1000
          if (finite(start) && finite(end) && end >= start) {
            values.started_at_seconds = start
            values.ended_at_seconds = end
          }
          links.set(run, links.has(run) ? null : { campaign, kind: kind ?? '', values })
        }
      } finally {
        closeSync(child)
      }
    }
  } catch (error) {
    if (error instanceof Error && error.message === 'work_limit') throw error
    throw new Error('unsafe_campaign')
  } finally {
    closeSync(fd)
  }
  return links
}

export function readResults(directory: string, options: Partial<Limits> = {}): Snapshot {
  const limits = { ...defaults, ...options }
  const snapshot: Snapshot = { reports: [], counts: { valid: 0, invalid: 0, unsupported: 0 } }
  const budget = { bytes: 0, entries: 0 }
  const root = resolve(directory)
  let fd: number
  try {
    if (realpathSync(root) !== root) throw new Error('unsafe_directory')
    fd = openSync(root, constants.O_RDONLY | constants.O_DIRECTORY | constants.O_NOFOLLOW)
  } catch {
    throw new Error('unsafe_directory')
  }
  try {
    // Pin reads to the opened directory, even if another process renames it.
    const pinned = `/proc/self/fd/${fd}`
    if (realpathSync(pinned) !== root) throw new Error('unsafe_directory')
    const links = campaignLinks(pinned, budget, limits)
    let reports = 0
    let pointCount = 0
    for (const name of entries(pinned, budget, limits)) {
      if (!name.endsWith('.json')) continue
      const format = reportPattern.exec(name)?.[1]
      if (!format) {
        snapshot.counts.unsupported++
        continue
      }
      if (++reports > limits.maxReports) throw new Error('work_limit')
      try {
        const file = readBounded(join(pinned, name), budget, limits)
        const source: unknown = JSON.parse(file.text)
        if (!isJsonRecord(source)) throw new Error('invalid_report')
        const content = source
        const kind = format.startsWith('upload-') ? 'upload'
          : format === 'shared-drive-ramp' ? 'shared_drive' : 'fanout'
        const run = kind === 'upload' ? content.campaign_id : name.replace(reportPattern, '')
        if (!safeId(run)) throw new Error('invalid_identity')
        const pointField = { upload: 'attempts', shared_drive: 'levels', fanout: 'points' }[kind]
        const points = content[pointField]
        if (Array.isArray(points)) {
          pointCount += points.length
          if (pointCount > 2048) throw new Error('work_limit')
          if (points.length > 256) throw new Error('unsafe_points')
        }
        const link = links.get(run)
        const ambiguous = link === null || (link !== undefined && link.kind !== format)
        if (ambiguous) throw new Error('ambiguous_campaign')
        snapshot.reports.push({
          campaign: link?.campaign ?? run, run, kind, data: content, timestamp: file.timestamp,
          startedAt: link?.values.started_at_seconds ?? seconds(content.started_at),
          endedAt: link?.values.ended_at_seconds ?? seconds(content.ended_at),
        })
      } catch (error) {
        if (error instanceof Error && error.message === 'work_limit') throw error
        snapshot.counts.invalid++
      }
    }
  } finally {
    closeSync(fd)
  }
  const ids = new Map<string, number>()
  for (const result of snapshot.reports) ids.set(result.run, (ids.get(result.run) ?? 0) + 1)
  snapshot.reports = snapshot.reports.filter(result => {
    if (ids.get(result.run) === 1) return true
    snapshot.counts.invalid++
    return false
  })
  snapshot.counts.valid = snapshot.reports.length
  return snapshot
}

export function createResultsServer(directory: string) {
  const server = createServer({ maxHeaderSize: 8192 }, (request, response) => {
    response.setHeader('Cache-Control', 'no-store')
    response.setHeader('X-Content-Type-Options', 'nosniff')
    response.setHeader('Content-Type', 'text/plain; charset=utf-8')
    if (request.method !== 'GET') {
      response.writeHead(405, { Allow: 'GET' }).end('method_not_allowed\n')
      return
    }
    if (request.url === '/health') {
      response.end('ok\n')
      return
    }
    if (request.url !== '/metrics') {
      response.writeHead(404).end('not_found\n')
      return
    }
    try {
      const snapshot = readResults(directory)
      const text = renderMetrics(snapshot.reports, snapshot.counts)
      response.setHeader('Content-Type', 'text/plain; version=0.0.4; charset=utf-8')
      response.end(text)
    } catch {
      response.writeHead(503).end('results_unavailable\n')
    }
  })
  server.maxConnections = 32
  server.maxRequestsPerSocket = 20
  server.requestTimeout = 5000
  server.headersTimeout = 5000
  server.keepAliveTimeout = 1000
  return server
}

function start(): void {
  const rawPort = process.env.CAPACITY_RESULTS_PORT ?? '9465'
  if (!/^(0|[1-9][0-9]{0,4})$/.test(rawPort) || Number(rawPort) > 65535) {
    throw new Error('invalid_port')
  }
  const harness = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..')
  const directory = resolve(process.env.CAPACITY_RESULTS_DIR ?? resolve(harness, 'data', 'results'))
  const server = createResultsServer(directory)
  server.on('error', () => {
    process.stderr.write('capacity_results_startup_failed\n')
    process.exitCode = 1
  })
  server.listen(Number(rawPort), '0.0.0.0', () => {
    const address = server.address()
    if (address && typeof address !== 'string') {
      process.stdout.write(`capacity_results_listening ${address.port}\n`)
    }
  })
  process.once('SIGTERM', () => server.close())
  process.once('SIGINT', () => server.close())
}

if (process.argv[1] !== undefined && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    start()
  } catch {
    process.stderr.write('capacity_results_startup_failed\n')
    process.exitCode = 1
  }
}
