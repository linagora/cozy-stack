import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { mkdtemp, readFile, rm, stat } from 'node:fs/promises'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const script = fileURLToPath(new URL('./org-drive-fanout.ts', import.meta.url))
const root = `seed-${'a'.repeat(32)}.localhost:8080`
const orgID = root.split('.')[0]!
const domain = (i: number) => `seed-${i.toString(16).padStart(32, '0')}.localhost:8080`
const secret = 'private-fanout-admin-secret'
interface Instance {
  id: string,
  attributes: { domain: string, uuid: string, org_id: string, org_domain: string, context: string }
}
interface Call {
  api: string, method: string, url: URL, headers: IncomingMessage['headers'], body: string,
  transform?: (value: any) => any, delayMs?: number
}
type Override = (r: Call, res: ServerResponse) => boolean | void
async function fixture(t: TestContext, override?: Override) {
  const instances: Instance[] = [root, ...Array.from({ length: 12 }, (_, i) => domain(i + 1))].map((name, i) => ({
    id: `instance-${i}`, attributes: { domain: name, uuid: randomUUID(), org_id: orgID,
      org_domain: `${orgID}.seed.invalid`, context: 'fanout-test' },
  }))
  const calls: Call[] = []
  const origins: Record<string, string> = {}
  const drives = new Map<string, { recipients: string[], group: string, removed: boolean, owner: string }>()
  const contacts = new Map<string, string[]>()
  let current = ''
  let stdout = ''
  for (const api of ['admin', 'stack']) {
    const server = createServer(async (req, res) => {
      let body = ''
      for await (const chunk of req) body += String(chunk)
      const r: Call = {
        api, method: req.method!, url: new URL(req.url!, 'http://localhost'), headers: req.headers, body,
      }
      calls.push(r)
      if (override?.(r, res)) return
      const { pathname: path, searchParams: p } = r.url
      const send = (status: number, value?: unknown) => {
        const end = () => res.writeHead(status).end(
          value === undefined ? '' : JSON.stringify(r.transform ? r.transform(value) : value))
        return r.delayMs ? setTimeout(end, r.delayMs).unref() : end()
      }
      if (api === 'admin') {
        if (path === '/instances/token') return res.end(`token-${p.get('Domain')}`)
        if (path === '/instances' && r.method === 'GET') {
          const start = Number(p.get('page[cursor]') ?? 0)
          return send(200, { data: instances.slice(start, start + 10), links: {
            next: start + 10 < instances.length ? `/instances?page[limit]=10&page[cursor]=${start + 10}` : null,
          } })
        }
        if (path === '/instances' && r.method === 'POST') {
          const name = p.get('Domain')!
          const item = { id: `instance-${instances.length}`, attributes: { domain: name, uuid: p.get('UUID')!,
            org_id: p.get('OrgID')!, org_domain: p.get('OrgDomain')!, context: p.get('ContextName')! } }
          instances.push(item)
          return send(201, { data: item })
        }
        const item = instances.find(i => path === `/instances/${i.attributes.domain}`)
        return send(item ? 200 : 404, { data: item })
      }
      if (path === '/settings/context') return send(200, { data: { id: 'context', attributes: {
        common_contacts: true, max_members_per_sharing: 1001, sharing: { auto_accept_trusted: true },
      } } })
      if (path === '/sharings/drives' && r.method === 'POST') {
        current = `drive-${drives.size + 1}`
        drives.set(current, { recipients: [], group: '', removed: false, owner: r.headers.host! })
      }
      const sid = path.match(/^\/sharings\/(?:drives\/)?(drive-\d+)/)?.[1] ?? current
      const drive = drives.get(sid)!
      if (path === '/data/io.cozy.contacts.groups') return send(404)
      if (path === '/data/io.cozy.contacts.groups/') {
        const id = `group-${drives.size}`
        contacts.set(id, [])
        return send(201, { id })
      }
      if (path === '/data/io.cozy.contacts/') {
        const value = JSON.parse(body)
        contacts.get(value.relationships.groups.data[0]._id)!.push(new URL(value.cozy[0].url).host)
        return send(201, { id: `contact-${calls.length}` })
      }
      if (path.startsWith('/files/root-')) return send(201, { data: { id: `file-${sid}`, attributes: {} } })
      if (path.endsWith('/recipients') && r.method === 'POST') {
        drive.group = JSON.parse(body).data.relationships.recipients.data[0].id
        drive.recipients = contacts.get(drive.group)!
        return send(200, { data: { id: sid } })
      }
      if (path.includes('/groups/') && r.method === 'DELETE') {
        drive.removed = true
        return send(204)
      }
      const owner = r.headers.host === drive.owner
      if (path.includes('/download/')) {
        if (drive.removed && !owner) return send(403)
        return res.end('cozy-org-fanout-canary\n')
      }
      const isDriveRoot = path.startsWith('/sharings/drives/') && path.includes('/root-')
      if (isDriveRoot) return send(drive.removed && !owner ? 403 : 200, { data: { id: `root-${sid}` } })
      const members = [{ instance: `https://${drive.owner}/`, status: 'owner' }, ...drive.recipients.map(name => ({
        instance: `https://${name}/`, status: drive.removed ? 'revoked' : 'ready',
        only_in_groups: true, groups: [1],
      }))]
      return send(path === '/sharings/drives' ? 201 : 200, { data: { id: sid, attributes: {
        org_drive: true, owner, active: owner || !drive.removed, members,
        rules: [{ values: [`root-${sid}`] }], groups: [
          { id: 'historic-group', revoked: true, read_only: false, addedBy: 0 },
          ...(drive.group ? [{ id: drive.group, revoked: drive.removed, read_only: false, addedBy: 0 }] : []),
        ],
      } } })
    })
    await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
    origins[api] = `http://127.0.0.1:${(server.address() as { port: number }).port}`
    t.after(async () => {
      server.closeAllConnections()
      await new Promise<void>(resolve => server.close(() => resolve()))
    })
  }
  async function run(
    env: Record<string, string | undefined> = {},
    options: { preload?: string, onStderr?: (text: string) => void } = {},
  ) {
    stdout = ''
    let stderr = ''
    const inherited = Object.fromEntries(Object.entries(process.env).filter(([k]) => !/^(SEED_|FANOUT_)/.test(k)))
    const args = ['--import', 'tsx', ...(options.preload ? ['--import', options.preload] : []), script]
    const child = spawn(process.execPath, args, { timeout: 15000, env: {
      ...inherited, SEED_ADMIN_URL: origins.admin, SEED_STACK_URL: origins.stack, SEED_ADMIN_PASSWORD: secret,
      SEED_DOMAIN_SUFFIX: 'localhost:8080', SEED_CONTEXT_NAME: 'fanout-test', FANOUT_ORG_DOMAIN: root,
      FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '800', FANOUT_POLL_MS: '10', FANOUT_STABLE_MS: '20',
      FANOUT_POLL_CONCURRENCY: '2', ...env,
    } })
    child.stdout.on('data', chunk => { stdout += String(chunk) })
    child.stderr.on('data', chunk => {
      stderr += String(chunk)
      options.onStderr?.(stderr)
    })
    const code = await new Promise<number | null>((resolve, reject) => {
      child.on('error', reject)
      child.on('close', resolve)
    })
    let report: any
    try {
      report = JSON.parse(stdout)
    } catch { /* Preserve child diagnostics for assertions. */ }
    return { code, stdout, stderr, report }
  }
  return { calls, instances, drives, origins, run, get stdout() { return stdout } }
}
const mutations = (calls: Call[]) => calls.filter(r => r.method !== 'GET')

test('real child ramps group fanout, proves every recipient access and one-operation removal', async t => {
  const f = await fixture(t)
  const result = await f.run()
  equal(result.code, 0, result.stdout + result.stderr)
  equal(result.report.status, 'passed')
  deepEqual(result.report.capacity, { kind: 'lower_bound', lower: 2, upper_exclusive: null, max: null })
  deepEqual(result.report.points.map((p: any) => p.n), [1, 2])
  for (const p of result.report.points) {
    equal(p.status, 'passed')
    equal(p.mutation_concurrency, 1)
    equal(p.observer_concurrency, 2)
    equal(p.recipients.length, p.n)
    ok(p.recipients.every((r: any) => r.add.converged && r.remove.converged))
    ok(p.recipients.every((r: any) => r.add.observed_elapsed_ms > 0 && r.remove.observed_elapsed_ms > 0))
    ok(p.add.elapsed_ms >= 20 && p.remove.elapsed_ms >= 20)
    for (const phase of [p.add, p.remove]) {
      ok(phase.converged_ms >= 0 && phase.elapsed_ms - phase.converged_ms >= 20, JSON.stringify(phase))
    }
    equal(p.fixtures.contact_ids.length, p.n)
    equal(p.owner.add.ready_recipients, p.n)
    equal(p.owner.remove.revoked_recipients, p.n)
    equal(p.owner.remove.active, true)
  }
  const add = f.calls.filter(r => r.url.pathname.endsWith('/recipients'))
  equal(add.length, 2)
  for (const r of add) {
    const value = JSON.parse(r.body)
    equal(value.data.type, 'io.cozy.sharings')
    deepEqual(value.data.relationships.recipients.data, [{
      id: `group-${value.data.id.split('-')[1]}`, type: 'io.cozy.contacts.groups',
    }])
    ok(!r.url.searchParams.has('interactive'))
  }
  equal(f.calls.filter(r => r.method === 'DELETE').length, 2)
  ok(f.calls.filter(r => r.method === 'DELETE').every(r => r.url.pathname.endsWith('/groups/1')))
  const contacts = f.calls.filter(r => r.url.pathname === '/data/io.cozy.contacts/')
  equal(contacts.length, 3)
  for (const r of contacts) {
    equal(r.headers['content-type'], 'application/json')
    const c = JSON.parse(r.body)
    const actor = f.instances.find(i => i.attributes.domain === new URL(c.cozy[0].url).host)!
    equal(c.cozy[0].url, `https://${actor.attributes.domain}/`)
    deepEqual(c.email, [{ address: `${actor.attributes.uuid}@seed.invalid`, primary: true }])
    deepEqual(Object.keys(c.relationships), ['groups'])
    equal(c.relationships.groups.data[0]._type, 'io.cozy.contacts.groups')
  }
  const groups = f.calls.filter(r => r.url.pathname === '/data/io.cozy.contacts.groups/')
  ok(groups.every(r => !('members' in JSON.parse(r.body))))
  ok(f.calls.filter(r => r.api === 'stack').every(r => r.headers.host?.startsWith('seed-')))
  equal(f.calls.filter(r => r.url.pathname === '/settings/context').length, 13)
  ok(f.calls.some(r => r.url.searchParams.has('page[cursor]')))
  ok(!result.stdout.includes(secret) && !result.stdout.includes('token-'))
  equal(f.calls.filter(r => /accept|answer|cleanup/.test(r.url.pathname)).length, 0)
})

test('pending peer snapshots may omit instance URLs without invalidating the ready recipient', async t => {
  const f = await fixture(t, r => {
    const isRecipientSnapshot = r.method === 'GET' &&
      /^\/sharings\/drive-/.test(r.url.pathname) && r.headers.host !== root
    if (!isRecipientSnapshot) return
    r.transform = value => {
      for (const m of value.data.attributes.members) {
        if (m.status !== 'owner' && new URL(m.instance).host !== r.headers.host) {
          delete m.instance
          m.status = 'pending'
        }
      }
      return value
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '2' })
  equal(result.code, 0, result.stdout + result.stderr)
  equal(result.report.points[0].status, 'passed')
  ok(result.report.points[0].recipients.every((r: any) => r.add.own_status === 'ready'))
})

test('configuration member cap is reported as infrastructure, before drive creation', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.url.pathname !== '/settings/context') return
    res.end(JSON.stringify({ data: { id: 'context', attributes: {
      common_contacts: true, max_members_per_sharing: 2, sharing: { auto_accept_trusted: true },
    } } }))
    return true
  })
  const result = await f.run()
  equal(result.code, 2)
  equal(result.report.status, 'inconclusive')
  equal(result.report.settings.effective_member_cap, 2)
  equal(result.report.capacity.max, null)
  equal(result.report.points.length, 0)
  equal(f.calls.filter(r => r.api === 'stack' && r.method !== 'GET').length, 0)
})

test('recipient canonical scheme mismatch is inconclusive, not a ready cohort', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host !== domain(1) || !/^\/sharings\/drive-/.test(r.url.pathname)) return
    const removed = f.drives.get('drive-1')!.removed
    res.end(JSON.stringify({ data: { id: 'drive-1', attributes: { active: !removed, members: [
      { instance: `https://${root}/`, status: 'owner' },
      { instance: `http://${domain(1)}/`, status: removed ? 'revoked' : 'ready' },
    ] } } }))
    return true
  })
  const result = await f.run({ FANOUT_LEVELS: '1' })
  equal(result.code, 2, result.stdout)
  equal(result.report.status, 'inconclusive')
  equal(result.report.points[0].add.status, 'inconclusive')
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
})

test('unrecognized roster fields cannot leak an upstream secret into the report', async t => {
  const f = await fixture(t, r => {
    if (r.headers.host === domain(1) && r.url.pathname === '/sharings/drive-1') r.transform = value => {
      value.data.attributes.members[1].status = secret
      return value
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1', FANOUT_TIMEOUT_MS: '120' })
  equal(result.code, 2)
  ok(!result.stdout.includes(secret), result.stdout)
  equal(result.report.status, 'inconclusive')
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
})

test('healthy all-N nonconvergence yields a sparse bracket and retains running add work', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host === domain(2) && r.url.pathname === '/sharings/drive-2') {
      res.writeHead(404).end()
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1,2,4', FANOUT_TIMEOUT_MS: '300', FANOUT_POLL_MS: '160' })
  equal(result.code, 0, result.stdout)
  equal(result.report.status, 'failed', result.stdout)
  deepEqual(result.report.capacity, { kind: 'bracket', lower: 1, upper_exclusive: 2, max: null })
  equal(f.drives.size, 2)
  equal(result.report.points[1].retained_autoaccept_work_may_continue, true)
  equal(result.report.points[1].recipients[1].add.sharing_status, 404)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 1)
})

test('an observation still in flight at the phase deadline misses the SLA like a late reply', async t => {
  for (const mode of ['headers', 'body']) await t.test(mode, async t => {
    const f = await fixture(t, (r, res) => {
      if (r.headers.host !== domain(2) || r.url.pathname !== '/sharings/drive-2') return
      // Neither stall ends, so the runner's own request deadline cuts the read.
      if (mode === 'body') res.writeHead(200).write('{')
      return true
    })
    const result = await f.run({ FANOUT_LEVELS: '1,2,4', FANOUT_TIMEOUT_MS: '300', FANOUT_POLL_MS: '160' })
    equal(result.code, 0, result.stdout)
    equal(result.report.status, 'failed', result.stdout)
    deepEqual(result.report.capacity, { kind: 'bracket', lower: 1, upper_exclusive: 2, max: null })
    equal(result.report.error, 'Observation SLA deadline exceeded')
    const failed = result.report.points[1]
    equal(failed.add.mutation, 'accepted')
    equal(failed.add.status, 'failed')
    equal(failed.retained_autoaccept_work_may_continue, true)
    equal(f.calls.filter(r => r.method === 'DELETE').length, 1)
  })
})

// Advance only the post-response deadline check, without racing real request timers.
// The next-turn acknowledgement runs after the observer's rejection has settled.
const observerClock = 'data:text/javascript,' + encodeURIComponent(`
  import http from 'node:http'
  import { performance } from 'node:perf_hooks'
  const realNow = performance.now.bind(performance)
  let crossDeadline = false
  Object.defineProperty(performance, 'now', { value: () => {
    const offset = crossDeadline ? 1000000 : 0
    crossDeadline = false
    return realNow() + offset
  } })
  const emit = http.IncomingMessage.prototype.emit
  const settled = new WeakSet()
  http.IncomingMessage.prototype.emit = function (event, ...args) {
    const label = this.headers['x-fanout-test']
    if (label && !settled.has(this) && (event === 'end' || event === 'aborted')) {
      settled.add(this)
      crossDeadline = this.headers['x-fanout-deadline'] === 'true'
      setImmediate(() => console.error('settled:' + label))
    }
    return Reflect.apply(emit, this, [event, ...args])
  }
`)

test('mixed observer failures preserve uncertainty after a passed level in either completion order', async t => {
  const cases = [
    { mode: 'json', order: 'uncertain-first' }, { mode: 'json', order: 'deadline-first' },
    { mode: 'overflow', order: 'uncertain-first' }, { mode: 'overflow', order: 'deadline-first' },
    { mode: 'deadlines', order: 'deadline-first' },
  ]
  for (const { mode, order } of cases) await t.test(`${mode} ${order}`, async t => {
    const held = new Map<string, ServerResponse>()
    let inflight = 0
    let peak = 0
    let drainedFirst: { stdout: string, inflight: number } | undefined
    let writesAtFailure: Call[] = []
    const firstHost = order === 'uncertain-first' ? domain(1) : root
    const secondHost = firstHost === root ? domain(1) : root
    const respond = (host: string, label: string) => {
      const deadline = host === root || mode === 'deadlines'
      held.get(host)!.writeHead(200, {
        'x-fanout-test': label, 'x-fanout-deadline': String(deadline),
      }).end(!deadline && mode === 'overflow' ? 'x'.repeat(1_048_577) : '{')
    }
    const f = await fixture(t, (r, res) => {
      if (r.method !== 'GET' || r.url.pathname !== '/sharings/drive-2') return
      peak = Math.max(peak, ++inflight)
      res.once('finish', () => { inflight-- })
      held.set(r.headers.host!, res)
      if (held.size === 2) respond(firstHost, 'first')
      return true
    })
    const result = await f.run({ FANOUT_LEVELS: '1,2,4', FANOUT_TIMEOUT_MS: '5000' }, {
      preload: observerClock,
      onStderr: text => {
        if (drainedFirst || !text.includes('settled:first')) return
        drainedFirst = { stdout: f.stdout, inflight }
        writesAtFailure = mutations(f.calls)
        respond(secondHost, 'second')
      },
    })
    equal(result.code, mode === 'deadlines' ? 0 : 2, result.stdout + result.stderr)
    deepEqual(drainedFirst, { stdout: '', inflight: 1 }, 'report must wait for the other observer')
    equal(inflight, 0, 'every in-flight observer drains')
    equal(peak, 2, 'both failing observers were in flight')
    deepEqual([...result.stderr.matchAll(/settled:(first|second)/g)].map(m => m[1]), ['first', 'second'])
    deepEqual(mutations(f.calls), writesAtFailure, 'no mutations after observer failure')
    equal(held.size, 2, 'queued recipient observer must not start')
    deepEqual([...held.keys()].sort(), [root, domain(1)].sort())
    deepEqual(result.report.points.map((p: any) => p.n), [1, 2], 'no further levels')
    equal(f.drives.size, 2)
    equal(f.calls.filter(r => r.url.pathname.endsWith('/recipients')).length, 2, 'no add retry')
    equal(f.calls.filter(r => r.method === 'DELETE').length, 1, 'only N=1 removal')
    const [passed, failed] = result.report.points
    equal(passed.status, 'passed')
    equal(failed.add.mutation, 'accepted')
    equal(failed.remove.status, 'not_started')
    equal(failed.retained_autoaccept_work_may_continue, true)
    const status = mode === 'deadlines' ? 'failed' : 'inconclusive'
    equal(result.report.status, status, result.stdout)
    equal(failed.status, status)
    equal(failed.add.status, status)
    if (mode === 'deadlines') {
      deepEqual(result.report.capacity, { kind: 'bracket', lower: 1, upper_exclusive: 2, max: null })
      equal(result.report.error, 'Observation SLA deadline exceeded')
    } else {
      deepEqual(result.report.capacity, { kind: 'inconclusive', lower: null, upper_exclusive: null, max: null })
      equal(result.report.error, mode === 'json' ? 'Invalid API JSON' :
        'Transport failure; server write completion unknown')
    }
  })
})

test('transient recipient absence is pending, then all-N access must stabilize', async t => {
  let misses = 0
  const f = await fixture(t, (r, res) => {
    if (r.headers.host === domain(2) && r.url.pathname === '/sharings/drive-1' && misses++ < 3) {
      res.writeHead(404).end()
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '2', FANOUT_STABLE_MS: '35' })
  equal(result.code, 0, result.stdout)
  ok(misses >= 4)
  ok(result.report.points[0].add.elapsed_ms >= 60)
  equal(result.report.points[0].recipients.length, 2)
})

test('wrong canary bytes cannot pass even with an entirely ready owner roster', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host === domain(1) && r.url.pathname.includes('/download/')) {
      res.end('wrong bytes')
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '300', FANOUT_POLL_MS: '160' })
  equal(result.code, 1)
  equal(result.report.status, 'failed', result.stdout)
  equal(result.report.capacity.kind, 'inconclusive')
  equal(result.report.points[0].recipients[0].add.marker_matches, false)
  equal(result.report.points[0].owner.add.converged, true)
  equal(f.drives.size, 1)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
})

test('wrong bytes after a passed level fail the run instead of measuring a bracket', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host === domain(2) && r.url.pathname.includes('/download/')) {
      res.end('wrong bytes')
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '300', FANOUT_POLL_MS: '160' })
  equal(result.code, 1, result.stdout)
  equal(result.report.status, 'failed')
  equal(result.report.content_mismatch, true)
  equal(result.report.error, 'Recipient read different marker bytes')
  equal(result.report.capacity.kind, 'inconclusive')
  deepEqual(result.report.points.map((p: { status: string }) => p.status), ['passed', 'failed'])
})

test('removal without denial invalidates every usable capacity claim, retaining add diagnostics', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host !== root && r.url.pathname.includes('/download/') && f.drives.get('drive-2')?.removed) {
      res.end('cozy-org-fanout-canary\n')
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1,2,4', FANOUT_TIMEOUT_MS: '300', FANOUT_POLL_MS: '160' })
  equal(result.code, 2)
  equal(result.report.status, 'inconclusive', result.stdout)
  deepEqual(result.report.capacity, { kind: 'inconclusive', lower: null, upper_exclusive: null, max: null })
  equal(result.report.points[0].status, 'passed')
  equal(result.report.points[1].add.status, 'passed')
  equal(result.report.points[1].remove.status, 'failed', result.stdout)
  equal(f.drives.size, 2)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 2)
})

test('single mutation transport failures are unknown and never retried or removed', async t => {
  const modes = ['disconnect', 'timeout', 'late-body', 'redirect', 'http-error']
  for (const mode of modes) await t.test(mode, async t => {
    const f = await fixture(t, (r, res) => {
      if (!r.url.pathname.endsWith('/recipients')) return
      if (mode === 'disconnect') res.destroy()
      else if (mode === 'redirect') res.writeHead(307, { Location: `${f.origins.admin}/leak` }).end(secret)
      else if (mode === 'http-error') res.writeHead(500).end(secret)
      else if (mode === 'late-body') {
        res.writeHead(200)
        res.write('{')
        setTimeout(() => res.end(secret), 250).unref()
      }
      // timeout deliberately leaves headers and body unwritten.
      return true
    })
    const result = await f.run({ FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '120' })
    equal(result.code, 2)
    equal(result.report.status, 'inconclusive', result.stdout)
    equal(result.report.capacity.max, null)
    const isHTTPError = ['redirect', 'http-error'].includes(mode)
    equal(result.report.points[0].add.mutation, isHTTPError ? 'http_error' : 'unknown')
    equal(result.report.points[0].add.http_status, mode === 'redirect' ? 307 : mode === 'http-error' ? 500 : null)
    ok(result.report.points[0].recipients.every((actor: any) => actor.add === null && actor.remove === null))
    equal(f.drives.size, 1)
    equal(f.calls.filter(r => r.url.pathname.endsWith('/recipients')).length, 1)
    equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
    ok(!result.stdout.includes(secret) && !result.stderr.includes(secret))
    ok(f.calls.every(r => r.url.pathname !== '/leak'))
  })
})

test('malformed observations are inconclusive, never capacity saturation', async t => {
  for (const mode of ['json', 'identity', 'overflow']) await t.test(mode, async t => {
    const f = await fixture(t, (r, res) => {
      if (r.headers.host !== domain(1) || r.url.pathname !== '/sharings/drive-1') return
      if (mode === 'json') res.end(`{${secret}`)
      else if (mode === 'overflow') res.end('x'.repeat(1_048_577))
      else {
        r.transform = value => {
          value.data.attributes.members[0].instance = `https://${domain(3)}/`
          return value
        }
        return
      }
      return true
    })
    const result = await f.run({ FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '120' })
    equal(result.code, 2)
    equal(result.report.status, 'inconclusive')
    equal(result.report.capacity.kind, 'inconclusive')
    equal(result.report.points[0].add.mutation, 'accepted')
    equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
    ok(!result.stdout.includes(secret))
  })
})

test('all configuration is rejected before network or mutations', async t => {
  const f = await fixture(t)
  for (const env of [
    { FANOUT_LEVELS: '' }, { FANOUT_LEVELS: '2,1' }, { FANOUT_LEVELS: '1,1' }, { FANOUT_LEVELS: '1,1001' },
    { FANOUT_LEVELS: '1, 2' }, { FANOUT_LEVELS: '0' }, { FANOUT_TIMEOUT_MS: '0' }, { FANOUT_POLL_MS: '-1' },
    { FANOUT_POLL_CONCURRENCY: '0' }, { FANOUT_STABLE_MS: '800' }, { FANOUT_ORG_DOMAIN: 'real.example' },
    { SEED_STACK_URL: 'http://user:pass@localhost' }, { SEED_ADMIN_URL: 'http://localhost/private' },
    { SEED_CONTEXT_NAME: 'bad context' }, { SEED_DOMAIN_SUFFIX: 'different.invalid' },
  ]) {
    const result = await f.run(env)
    equal(result.code, 2, JSON.stringify(env))
    equal(result.report.status, 'inconclusive')
    equal(f.calls.length, 0)
  }
})

test('every registry page validates ownership, UUID, contexts and uniqueness before setup', async t => {
  const modes = ['uuid', 'org-id', 'org-domain', 'suffix', 'context', 'duplicate', 'missing-root']
  for (const mode of modes) await t.test(mode, async t => {
    const f = await fixture(t)
    const last = f.instances.at(-1)!
    if (mode === 'uuid') last.attributes.uuid = 'not-a-v4-uuid'
    if (mode === 'org-id') last.attributes.org_id = 'foreign'
    if (mode === 'org-domain') last.attributes.org_domain = 'foreign.seed.invalid'
    if (mode === 'suffix') last.attributes.domain = `seed-${'b'.repeat(32)}.elsewhere.invalid`
    if (mode === 'context') last.attributes.context = 'foreign'
    if (mode === 'duplicate') last.attributes.uuid = f.instances[1]!.attributes.uuid
    if (mode === 'missing-root') f.instances.shift()
    const result = await f.run()
    equal(result.code, 2, result.stdout)
    equal(result.report.status, 'inconclusive')
    equal(mutations(f.calls).length, 0)
  })
})

test('registry links never forward admin auth to foreign origins, paths, userinfo or loops', async t => {
  for (const mode of ['origin', 'path', 'userinfo', 'hash', 'loop', 'object']) await t.test(mode, async t => {
    const f = await fixture(t, (r, res) => {
      if (r.api !== 'admin' || r.url.pathname !== '/instances') return
      const links: Record<string, unknown> = { origin: `${f.origins.stack}/instances`, path: '/leak',
        userinfo: f.origins.admin!.replace('://', '://user@') + '/instances', hash: '/instances#secret',
        loop: '/instances?page[limit]=10', object: { href: '/instances' } }
      res.end(JSON.stringify({ data: f.instances.slice(0, 10), links: { next: links[mode] } }))
      return true
    })
    const result = await f.run()
    equal(result.code, 2)
    equal(f.calls.length, 1)
    equal(mutations(f.calls).length, 0)
  })
})

test('new reports are private, byte-identical JSON and existing paths are refused before network', async t => {
  const base = await mkdtemp(join(tmpdir(), 'fanout-report-'))
  t.after(() => rm(base, { recursive: true, force: true }))
  const path = join(base, 'new', 'report.json')
  const f = await fixture(t)
  const result = await f.run({ FANOUT_LEVELS: '1', FANOUT_REPORT: path })
  equal(result.code, 0, result.stdout)
  equal(await readFile(path, 'utf8'), result.stdout)
  equal((await stat(path)).mode & 0o777, 0o600)
  equal((await stat(join(base, 'new'))).mode & 0o777, 0o700)
  const before = f.calls.length
  const rejected = await f.run({ FANOUT_REPORT: path })
  equal(rejected.code, 2)
  equal(f.calls.length, before)
  equal(await readFile(path, 'utf8'), result.stdout)
})

test('ordinary instances without a UUID stay outside the organization cohort', async t => {
  const f = await fixture(t)
  f.instances.push({ id: 'instance-plain', attributes: { domain: 'load.localhost:8080' } } as unknown as Instance)
  const result = await f.run({ FANOUT_LEVELS: '1' })
  equal(result.code, 0, result.stdout + result.stderr)
  equal(result.report.status, 'passed')
  ok(f.calls.every(r => r.headers.host !== 'load.localhost:8080'))
})

test('fresh mode invokes real metadata-only seeder and never silently adopts an existing root', async t => {
  const f = await fixture(t)
  f.instances.splice(0)
  const result = await f.run({
    FANOUT_ORG_DOMAIN: undefined, FANOUT_LEVELS: '1,2', SEED_FILES: '99', SEED_DRIVES: '99', SEED_USERS: '99',
  })
  equal(result.code, 0, result.stdout + result.stderr)
  equal(result.report.mode, 'fresh')
  equal(f.instances.length, 3)
  const writes = f.calls.filter(r => r.api === 'admin' && r.method === 'POST' && r.url.pathname === '/instances')
  equal(writes.length, 3)
  ok(writes.every(r => r.url.searchParams.get('OrgID') === result.report.organization_domain.split('.')[0]))
  ok(writes.every(r => r.url.searchParams.get('ContextName') === 'fanout-test'))
  equal(f.calls.filter(r => r.url.pathname === '/sharings/drives' && r.method === 'POST').length, 2)
  equal(f.calls.filter(r => r.url.pathname.startsWith('/files/') && r.method === 'POST').length, 2)
  ok(result.stderr.includes(result.report.organization_domain))
})

test('RPC time is part of the observation SLA and cannot manufacture a late stable pass', async t => {
  const f = await fixture(t, (r, res) => {
    if (!r.url.pathname.endsWith('/recipients')) return
    const drive = f.drives.get('drive-1')!
    drive.group = JSON.parse(r.body).data.relationships.recipients.data[0].id
    drive.recipients = [domain(1)]
    setTimeout(() => res.writeHead(200).end('{}'), 140).unref()
    return true
  })
  const result = await f.run({
    FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '250', FANOUT_STABLE_MS: '150', FANOUT_POLL_MS: '80',
  })
  equal(result.code, 1)
  equal(result.report.status, 'failed', result.stdout)
  equal(result.report.points[0].add.mutation, 'accepted')
  ok(result.report.points[0].add.elapsed_ms >= 250)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
  equal(f.drives.size, 1)
})

test('readiness that flickers between recipients never satisfies a stable all-N window', async t => {
  let reads = 0
  const f = await fixture(t, r => {
    if (r.headers.host === domain(1) && r.url.pathname === '/sharings/drive-1') r.transform = value => {
      value.data.attributes.members[1].status = ++reads % 2 ? 'ready' : 'pending'
      return value
    }
  })
  const result = await f.run({
    FANOUT_LEVELS: '2,4', FANOUT_TIMEOUT_MS: '900', FANOUT_STABLE_MS: '350', FANOUT_POLL_MS: '300',
  })
  equal(result.code, 1)
  equal(result.report.status, 'failed', result.stdout)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
  ok(reads >= 3)
})

test('removal rediscovers the group index immediately before its single DELETE', async t => {
  let latestIndex = 1
  let deletedIndex = -1
  const f = await fixture(t, r => {
    if (r.headers.host === root && r.url.pathname === '/sharings/drive-1') r.transform = value => {
      if (value.data.attributes.groups.length > 1) {
        latestIndex = latestIndex === 1 ? 2 : 1
        if (latestIndex === 2) {
          value.data.attributes.groups.splice(1, 0, { id: 'another-historic-group', revoked: true })
        }
        for (const m of value.data.attributes.members.slice(1)) m.groups = [latestIndex]
      }
      return value
    }
    if (r.method === 'DELETE') deletedIndex = Number(r.url.pathname.split('/').at(-1))
  })
  const result = await f.run({ FANOUT_LEVELS: '1' })
  equal(result.code, 0, result.stdout)
  const reads = f.calls.filter(r => r.headers.host === root && r.url.pathname === '/sharings/drive-1')
  ok(reads.length > 3)
  const deletion = f.calls.findIndex(r => r.method === 'DELETE')
  const previous = f.calls[deletion - 1]!
  equal(previous.url.pathname, '/sharings/drive-1')
  equal(previous.headers.host, root)
  // Each owner read flips the index. The latest pre-removal read, not its predecessor, controls DELETE.
  const isOwnerSnapshot = (r: Call) => r.headers.host === root && r.url.pathname === '/sharings/drive-1'
  const preRemovalOwnerReads = f.calls.slice(0, deletion).filter(isOwnerSnapshot).length
  equal(deletedIndex, preRemovalOwnerReads % 2 ? 2 : 1)
})

test('recipient sharing 404 after removal still requires both access-denial probes', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host !== root && r.url.pathname === '/sharings/drive-1' && f.drives.get('drive-1')!.removed) {
      res.writeHead(404).end()
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1' })
  equal(result.code, 0, result.stdout)
  const proof = result.report.points[0].recipients[0].remove
  equal(proof.sharing_status, 404)
  equal(proof.root_status, 403)
  equal(proof.marker_status, 403)
})

test('every actor must expose the same effective trust configuration without manual acceptance', async t => {
  const f = await fixture(t, r => {
    if (r.headers.host === domain(12) && r.url.pathname === '/settings/context') r.transform = value => {
      value.data.attributes.sharing.auto_accept_trusted = false
      return value
    }
  })
  const result = await f.run()
  equal(result.code, 2)
  match(result.report.error, /auto_accept_trusted/)
  equal(f.calls.filter(r => r.api === 'stack' && r.method !== 'GET').length, 0)
  const tokens = f.calls.filter(r => r.url.pathname === '/instances/token')
  ok(tokens.every(r => r.url.searchParams.get('Audience') === 'cli'))
  equal(tokens[0]!.url.searchParams.get('Scope'),
    'io.cozy.files io.cozy.sharings io.cozy.contacts io.cozy.contacts.groups')
  ok(tokens.slice(1).every(r => r.url.searchParams.get('Scope') === 'io.cozy.files io.cozy.sharings'))
})

test('observer request concurrency is independently bounded while mutation concurrency stays one', async t => {
  let inflight = 0
  let peak = 0
  const f = await fixture(t, (r, res) => {
    if (r.method === 'GET' && r.url.pathname.startsWith('/sharings/')) {
      peak = Math.max(peak, ++inflight)
      res.once('finish', () => { inflight-- })
      if (/^\/sharings\/drive-/.test(r.url.pathname)) r.delayMs = 12
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '8', FANOUT_POLL_CONCURRENCY: '3', FANOUT_TIMEOUT_MS: '1500' })
  equal(result.code, 0, result.stdout)
  equal(peak, 3)
  equal(inflight, 0)
  equal(f.calls.filter(r => r.url.pathname.endsWith('/recipients')).length, 1)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 1)
  equal(result.report.points[0].observer_concurrency, 3)
  equal(result.report.points[0].mutation_concurrency, 1)
})

test('malformed effective cap is never echoed as arbitrary upstream text', async t => {
  const f = await fixture(t, r => {
    if (r.url.pathname === '/settings/context') r.transform = value => {
      value.data.attributes.max_members_per_sharing = secret
      return value
    }
  })
  const result = await f.run()
  equal(result.code, 2)
  ok(!result.stdout.includes(secret), result.stdout)
  equal(result.report.capacity.kind, 'inconclusive')
  equal(f.drives.size, 0)
})

test('server errors on removal access probes are observer failures, not healthy SLA misses', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.headers.host !== root && /\/root-drive-1$/.test(r.url.pathname) && f.drives.get('drive-1')!.removed) {
      res.writeHead(500).end(secret)
      return true
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1,2', FANOUT_TIMEOUT_MS: '250', FANOUT_POLL_MS: '50' })
  equal(result.code, 2)
  equal(result.report.status, 'inconclusive')
  equal(result.report.points[0].remove.status, 'inconclusive', result.stdout)
  equal(f.drives.size, 1)
  ok(!result.stdout.includes(secret))
})

test('owner roster must include the exact cohort with group-only writable provenance', async t => {
  const modes = ['subset', 'direct-grant', 'read-only', 'wrong-group-index', 'inactive']
  for (const mode of modes) await t.test(mode, async t => {
    const f = await fixture(t, r => {
      if (r.headers.host !== root || r.url.pathname !== '/sharings/drive-1') return
      r.transform = value => {
        const a = value.data.attributes
        if (mode === 'subset') a.members.pop()
        if (mode === 'direct-grant') a.members[1].only_in_groups = false
        if (mode === 'read-only') a.members[1].read_only = true
        if (mode === 'wrong-group-index') a.members[1].groups = [0]
        if (mode === 'inactive') a.active = false
        return value
      }
    })
    const result = await f.run({ FANOUT_LEVELS: '2,4', FANOUT_TIMEOUT_MS: '300', FANOUT_POLL_MS: '160' })
    equal(result.code, 1)
    equal(result.report.points[0].add.status, 'failed', result.stdout)
    equal(f.drives.size, 1)
    equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
  })
})

test('fresh mode refuses a collision before invoking the seeder', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.api === 'admin' && r.method === 'GET' && r.url.pathname.startsWith('/instances/')) {
      res.writeHead(200).end('{}')
      return true
    }
  })
  const result = await f.run({ FANOUT_ORG_DOMAIN: undefined })
  equal(result.code, 2)
  equal(result.report.mode, 'fresh')
  equal(result.report.capacity.kind, 'inconclusive')
  equal(mutations(f.calls).length, 0)
  equal(f.calls.length, 1)
})

test('a failed real seed child leaves its disclosed partial organization without cleanup', async t => {
  const f = await fixture(t, (r, res) => {
    if (r.api === 'admin' && r.method === 'POST' && r.url.pathname === '/instances') {
      res.destroy()
      return true
    }
  })
  f.instances.splice(0)
  const result = await f.run({ FANOUT_ORG_DOMAIN: undefined, FANOUT_LEVELS: '1' })
  equal(result.code, 2)
  match(result.report.error, /seeder failed/)
  match(result.stderr, /API request failed/)
  ok(result.stderr.includes(result.report.organization_domain))
  equal(result.report.points.length, 0)
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
  equal(f.calls.filter(r => r.api === 'stack').length, 0)
})

test('report records the verified effective context when no override was requested', async t => {
  const f = await fixture(t)
  const result = await f.run({ SEED_CONTEXT_NAME: undefined, FANOUT_LEVELS: '1' })
  equal(result.code, 0, result.stdout)
  equal(result.report.settings.context, 'fanout-test')
  equal(result.report.settings.common_contacts, true)
  equal(result.report.settings.auto_accept_trusted, true)
  equal(result.report.points[0].fixtures.marker_bytes, Buffer.byteLength('cozy-org-fanout-canary\n'))
})

test('group-index rediscovery must still identify this owner sharing before DELETE', async t => {
  let ownerReads = 0
  const f = await fixture(t, r => {
    const isThirdOwnerSnapshot = r.headers.host === root &&
      r.url.pathname === '/sharings/drive-1' && ++ownerReads === 3
    if (isThirdOwnerSnapshot) r.transform = value => {
      value.data.id = 'foreign-sharing'
      return value
    }
  })
  const result = await f.run({ FANOUT_LEVELS: '1', FANOUT_STABLE_MS: '1' })
  equal(result.code, 2, result.stdout)
  equal(result.report.status, 'inconclusive')
  equal(result.report.points[0].add.status, 'passed')
  equal(f.calls.filter(r => r.method === 'DELETE').length, 0)
})

test(
  'known credentials injected into reportable identifiers are redacted in stdout and private reports',
  async t => {
    const base = await mkdtemp(join(tmpdir(), 'fanout-secret-report-'))
    t.after(() => rm(base, { recursive: true, force: true }))
    for (const value of [secret, `token-${root}`]) {
      const name = value === secret ? 'password' : 'token'
      await t.test(name, async t => {
        const f = await fixture(t, r => {
          if (r.url.pathname === '/sharings/drives' && r.method === 'POST') r.transform = data => {
            data.data.id = value
            return data
          }
        })
        const path = join(base, randomUUID() + '.json')
        const result = await f.run({ FANOUT_LEVELS: '1', FANOUT_REPORT: path })
        equal(result.code, 2)
        ok(!result.stdout.includes(value), result.stdout)
        ok(!result.stderr.includes(value))
        ok(!(await readFile(path, 'utf8')).includes(value))
      })
    }
  },
)
