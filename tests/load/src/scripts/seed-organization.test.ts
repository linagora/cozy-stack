import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createHash, randomUUID } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const domain = `seed-${'a'.repeat(32)}.localhost:8080`
const orgID = domain.split('.')[0]!
const orgDomain = `${orgID}.seed.invalid`
const password = 'private-organization-admin'
const script = fileURLToPath(new URL('./seed-scale-data.ts', import.meta.url))
interface Instance {
  id: string
  attributes: { domain: string, uuid: string, org_id?: string, org_domain?: string }
}
interface Request {
  api: string
  method: string
  url: URL
  headers: IncomingMessage['headers']
  body: Buffer
  outputBefore: string
}
type Override = (r: Request, res: ServerResponse) => boolean | void
const member = (hex: string) => `seed-${hex.repeat(32)}.localhost:8080`
function instance(name: string, scope = true): Instance {
  return { id: `id-${name}`, attributes: { domain: name, uuid: randomUUID(),
    ...(scope ? { org_id: orgID, org_domain: orgDomain } : {}) } }
}

// The manifest a seed of these instances would have written; absent names get a fresh UUID.
function record(f: { manifest: string, instances: Map<string, Instance> }, names: string[]) {
  writeFileSync(f.manifest, JSON.stringify({
    version: 1, profile: 'flat', status: 'complete', domain, organization: orgID,
    parameters: { users: names.length - 1, files: 0, drives: 0, context: null }, content: { bytes: 1024, md5: 'x' },
    instances: names.map((name, index) => ({ domain: name, uuid: f.instances.get(name)?.attributes.uuid ?? randomUUID(),
      role: index ? 'member' : 'organization', created: true, files: [], drives: [] })),
  }))
}

async function fixture(t: TestContext, override?: Override) {
  const instances = new Map<string, Instance>()
  const requests: Request[] = []
  const origins: Record<string, string> = {}
  let output = ''
  let drive = 0
  for (const api of ['admin', 'stack']) {
    const server = createServer(async (req, res) => {
      const chunks: Buffer[] = []
      for await (const chunk of req) chunks.push(Buffer.from(chunk))
      const r: Request = { api, method: req.method!, url: new URL(req.url!, 'http://localhost'),
        headers: req.headers, body: Buffer.concat(chunks), outputBefore: output }
      requests.push(r)
      if (override?.(r, res)) return
      const { pathname, searchParams: params } = r.url
      const send = (status: number, value?: unknown) => {
        res.writeHead(status, { 'Content-Type': 'application/vnd.api+json' })
        res.end(value === undefined ? '' : JSON.stringify(value))
      }
      if (api === 'admin' && pathname === '/instances') {
        if (r.method === 'POST') {
          const name = params.get('Domain')!
          const data = { id: `id-${instances.size}`, attributes: {
            domain: name, uuid: params.get('UUID')!, org_id: params.get('OrgID')!,
            org_domain: params.get('OrgDomain')!,
          } }
          instances.set(name, data)
          send(201, { data })
        } else {
          const cursor = Number(params.get('page[cursor]') ?? '0')
          const limit = Number(params.get('page[limit]') ?? '1000')
          const all = [...instances.values()]
          const next = cursor + limit < all.length ?
            `/instances?page[limit]=${limit}&page[cursor]=${cursor + limit}` : undefined
          send(200, { data: all.slice(cursor, cursor + limit), links: { next } })
        }
      } else if (api === 'admin' && pathname === '/instances/token') {
        res.end(`token-${params.get('Domain')}`)
      } else if (api === 'admin' && pathname.startsWith('/instances/')) {
        const name = pathname.slice('/instances/'.length)
        if (r.method === 'DELETE') send(instances.delete(name) ? 204 : 404)
        else send(instances.has(name) ? 200 : 404, { data: instances.get(name) })
      } else if (api === 'stack' && pathname === '/files/io.cozy.files.root-dir' && r.method === 'GET') {
        send(200, { data: { id: 'io.cozy.files.root-dir', relationships: { contents: { meta: { count: 3 } } } } })
      } else if (api === 'stack' && pathname === '/files/io.cozy.files.root-dir') {
        send(201, { data: { id: 'file' } })
      } else if (api === 'stack' && pathname === '/sharings/drives') {
        send(201, { data: { id: `sharing-${++drive}`, attributes: { rules: [{ values: [`root-${drive}`] }] } } })
      } else if (api === 'stack' && pathname.startsWith('/files/root-')) {
        const id = pathname.slice('/files/root-'.length)
        send(200, { data: { id: `root-${id}`, type: 'io.cozy.files', attributes: { type: 'directory' },
          relationships: { referenced_by: { data: [{ type: 'io.cozy.sharings', id: `sharing-${id}` }] } },
        } })
      } else send(500)
    })
    await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
    origins[api] = `http://127.0.0.1:${(server.address() as { port: number }).port}`
    t.after(async () => {
      server.closeAllConnections()
      await new Promise<void>((resolve) => server.close(() => resolve()))
    })
  }
  const dir = mkdtempSync(join(tmpdir(), 'seed-org-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const manifest = join(dir, 'manifest.json')
  async function run(args = ['seed-org', domain], env: Record<string, string | undefined> = {}) {
    output = ''
    const inherited = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('SEED_')))
    const child = spawn(process.execPath, ['--import', 'tsx', script, ...args], {
      env: { ...inherited, SEED_ADMIN_URL: origins.admin, SEED_ADMIN_PASSWORD: password, SEED_MANIFEST: manifest,
        ...env },
      timeout: 15_000,
    })
    child.stdout.on('data', (chunk) => { output += String(chunk) })
    child.stderr.on('data', (chunk) => { output += String(chunk) })
    const code = await new Promise<number | null>((resolve, reject) => {
      child.on('error', reject)
      child.on('close', resolve)
    })
    return { code, output }
  }
  return { instances, requests, origins, run, manifest }
}

const creations = (requests: Request[]) => requests.filter((r) => r.method === 'POST' &&
  r.api === 'admin' && r.url.pathname === '/instances')

test('organization membership follows every registry page for 10, 50 and 100 users', async (t) => {
  for (const users of [10, 50, 100]) await t.test(String(users), async (t) => {
    const f = await fixture(t)
    for (let index = 0; index < 13; index++) {
      const name = `unrelated-${index}.localhost:8080`
      f.instances.set(name, instance(name, false))
    }
    const result = await f.run(['seed-org', domain], { SEED_USERS: String(users) })
    equal(result.code, 0, result.output)
    equal(creations(f.requests).length, users + 1)
    equal(f.instances.size, users + 14)
    const pages = f.requests.filter((r) => r.method === 'GET' && r.url.pathname === '/instances')
    ok(pages.some((r) => r.url.searchParams.has('page[cursor]')))
    ok(pages.every((r) => r.url.searchParams.get('page[limit]') === '10'))
    ok(pages.every((r) => !r.url.searchParams.has('OrgID')))
    equal(f.requests.filter((r) => r.api === 'stack' || r.url.pathname === '/instances/token').length, 0)
  })
})

test('ordinary instances without a UUID do not block organization seeding or cleanup', async (t) => {
  const f = await fixture(t)
  const plain = 'load.localhost:8080'
  f.instances.set(plain, { id: 'id-plain', attributes: { domain: plain } } as unknown as Instance)
  const seeded = await f.run(['seed-org', domain], { SEED_USERS: '2' })
  equal(seeded.code, 0, seeded.output)
  equal(creations(f.requests).length, 3)
  const cleaned = await f.run(['cleanup-org', domain])
  equal(cleaned.code, 0, cleaned.output)
  deepEqual([...f.instances.keys()], [plain])
})

test('organization instances select and read back the requested server context', async (t) => {
  const context = 'load-org-sharing'
  const f = await fixture(t, (r, res) => {
    if (r.method === 'POST' && r.url.pathname === '/instances') {
      const params = r.url.searchParams
      const name = params.get('Domain')!
      const data = { id: `id-${f.instances.size}`, attributes: {
        domain: name, uuid: params.get('UUID')!, org_id: params.get('OrgID')!,
        org_domain: params.get('OrgDomain')!, context: params.get('ContextName'),
      } }
      f.instances.set(name, data)
      res.writeHead(201).end(JSON.stringify({ data }))
      return true
    }
  })
  const result = await f.run(['seed-org', domain], { SEED_USERS: '2', SEED_CONTEXT_NAME: context })
  equal(result.code, 0, result.output)
  const writes = creations(f.requests)
  equal(writes.length, 3)
  ok(writes.every(r => r.url.searchParams.get('ContextName') === context))
})

test('requested server context cannot be silently replaced during instance creation', async (t) => {
  const f = await fixture(t)
  const result = await f.run(['seed-org', domain], { SEED_CONTEXT_NAME: 'load-org-sharing' })
  equal(result.code, 1, result.output)
  match(result.output, /identity mismatch/)
  equal(creations(f.requests).length, 1)
  ok(f.requests.every(r => r.method !== 'DELETE' && r.url.pathname !== '/instances/token'))
})

test('invalid server context names fail before requests or fixture logging', async (t) => {
  const f = await fixture(t)
  for (const context of ['', ' ', 'load\nsharing', 'load\tsharing']) {
    const result = await f.run(['seed-org', domain], { SEED_CONTEXT_NAME: context })
    equal(result.code, 1, result.output)
    match(result.output, /SEED_CONTEXT_NAME/)
    equal(f.requests.length, 0)
    ok(!result.output.includes('Seed instance:'))
  }
})

test('seed-org defaults to a metadata-only organization and three distinct user instances', async (t) => {
  const f = await fixture(t)
  const result = await f.run()
  equal(result.code, 0, result.output)
  const writes = creations(f.requests)
  equal(writes.length, 4)
  equal(writes[0]!.url.searchParams.get('Domain'), domain)
  equal(f.instances.size, 4)
  const names = writes.map((r) => r.url.searchParams.get('Domain')!)
  equal(new Set(names).size, 4)
  for (const [index, r] of writes.entries()) {
    const params = r.url.searchParams
    const name = names[index]!
    match(name, /^seed-[a-f0-9]{32}\.localhost:8080$/)
    equal(params.get('OrgID'), orgID)
    equal(params.get('OrgDomain'), orgDomain)
    equal(params.get('CouchCluster'), '-1')
    equal(params.get('SwiftLayout'), '-1')
    ok(r.outputBefore.includes(name), 'planned domains must be logged before writes')
    const preflight = f.requests.findIndex((r) => r.method === 'GET' && r.url.pathname === `/instances/${name}`)
    ok(preflight >= 0 && preflight < f.requests.indexOf(writes[0]!))
    ok(f.requests.slice(f.requests.indexOf(r) + 1).some((r) => r.method === 'GET' &&
      r.url.pathname === `/instances/${name}`))
  }
  equal(f.requests.filter((r) => r.api === 'stack' || r.url.pathname === '/instances/token').length, 0)
  ok(f.requests.filter((r) => r.url.pathname === '/instances' && r.method === 'GET').length >= 2)
  ok(f.requests.every((r) => r.headers.authorization ===
    `Basic ${Buffer.from(`admin:${password}`).toString('base64')}`))
  ok(result.output.includes(`Seed complete: ${domain}; users=3; files=0; drives=0`))
})

test('registry pagination refuses unsafe links and cycles before credentials or writes escape', async (t) => {
  const modes = ['foreign', 'userinfo', 'hash', 'path', 'cycle', 'object', 'number']
  for (const [command, mode] of ['seed-org', 'cleanup-org'].flatMap((command) =>
    modes.map((mode) => [command, mode] as const))) {
    await t.test(`${command} ${mode}`, async (t) => {
      let pages = 0
      const f = await fixture(t, (r, res) => {
        if (r.api !== 'admin' || r.url.pathname !== '/instances' || r.method !== 'GET') return
        const links: Record<string, unknown> = {
          foreign: `${f.origins.stack}/instances`, userinfo: f.origins.admin!.replace('://', '://user@') +
            '/instances?page[limit]=10', hash: '/instances?page[limit]=10#private',
          path: '/must-not-follow', cycle: '/instances?page[limit]=10', object: { href: '/instances' }, number: 7,
        }
        res.end(JSON.stringify({ data: [], links: { next: ++pages < 3 ? links[mode] : null } }))
        return true
      })
      if (command === 'cleanup-org') record(f, [domain])
      const result = await f.run([command, domain])
      equal(result.code, 1, result.output)
      match(result.output, /pagination|instance list/i)
      equal(pages, 1)
      equal(creations(f.requests).length, 0)
      ok(f.requests.every((r) => r.api === 'admin' && r.url.pathname.startsWith('/instances')))
      ok(!result.output.includes(password) && !result.output.includes('user@'))
    })
  }
})

test('cleanup-org pages all members, deletes root last and preserves foreign instances', async (t) => {
  const f = await fixture(t)
  const foreignRoot = member('f')
  const foreign = instance(foreignRoot)
  foreign.attributes.org_id = foreignRoot.split('.')[0]!
  foreign.attributes.org_domain = `${foreign.attributes.org_id}.seed.invalid`
  f.instances.set(foreignRoot, foreign)
  f.instances.set(domain, instance(domain))
  const members = Array.from({ length: 13 }, (_, index) =>
    `seed-${index.toString(16).padStart(32, '0')}.localhost:8080`)
  for (const name of members) f.instances.set(name, instance(name))
  const ordinary = instance(member('e'), false)
  f.instances.set(ordinary.attributes.domain, ordinary)
  record(f, [domain, ...members])
  const result = await f.run(['cleanup-org', domain], {
    SEED_USERS: 'bad', SEED_FILES: '-1', SEED_DRIVES: 'bad', SEED_STACK_URL: undefined,
  })
  equal(result.code, 0, result.output)
  ok(result.output.includes(`Cleanup complete: ${domain}`))
  deepEqual([...f.instances.values()], [foreign, ordinary])
  const deletes = f.requests.filter((r) => r.method === 'DELETE')
  deepEqual(deletes.map((r) => r.url.pathname), [...members, domain].map((name) => `/instances/${name}`))
  for (const deletion of deletes) {
    const index = f.requests.indexOf(deletion)
    equal(f.requests[index - 1]!.method, 'GET')
    equal(f.requests[index - 1]!.url.pathname, deletion.url.pathname)
    equal(f.requests[index + 1]!.method, 'GET')
    equal(f.requests[index + 1]!.url.pathname, deletion.url.pathname)
  }
  const firstDelete = f.requests.indexOf(deletes[0]!)
  ok(f.requests.slice(0, firstDelete).some((r) => r.url.searchParams.has('page[cursor]')))
  const rootDelete = f.requests.indexOf(deletes.at(-1)!)
  ok(f.requests.slice(f.requests.indexOf(deletes.at(-2)!) + 2, rootDelete)
    .some((r) => r.url.pathname === '/instances'))
  equal(f.requests.at(-1)!.url.pathname, '/instances')
  ok(f.requests.every((r) => r.api === 'admin' && r.url.pathname !== '/instances/token'))
  match(String((await f.run(['cleanup-org', domain])).output), /No readable fixture manifest/)
})

test('an unrecorded member of the organization fails verify and keeps the root through cleanup', async (t) => {
  const f = await fixture(t)
  equal((await f.run(['seed-org', domain], { SEED_USERS: '2' })).code, 0)
  const verified = await f.run(['verify', domain])
  equal(verified.code, 0, verified.output)
  const stranger = instance(member('e'))
  f.instances.set(stranger.attributes.domain, stranger)
  match((await f.run(['verify', domain])).output, /membership differs from the manifest/)
  const result = await f.run(['cleanup-org', domain])
  equal(result.code, 1)
  match(result.output, /members remain; root retained/)
  deepEqual([...f.instances.keys()].sort(), [domain, stranger.attributes.domain].sort())
  ok(readFileSync(f.manifest, 'utf8'))
})

test('cleanup-org deletes nothing when the root is no longer the recorded instance', async (t) => {
  const f = await fixture(t)
  f.instances.set(domain, instance(domain))
  f.instances.set(member('b'), instance(member('b')))
  record(f, [domain, member('b')])
  f.instances.set(domain, instance(domain))
  const result = await f.run(['cleanup-org', domain])
  equal(result.code, 1)
  match(result.output, /root is not the recorded instance; kept/)
  equal(f.requests.filter((r) => r.method !== 'GET').length, 0)
  equal(f.instances.size, 2)
})

test('cleanup-org refuses the entire plan if any selected candidate is foreign or malformed', async (t) => {
  const variants = [
    { org_id: 'foreign' }, { org_domain: 'foreign.invalid' }, { domain: 'ordinary.localhost:8080' },
    { domain: member('c').replace('localhost', 'foreign.invalid') }, { domain: domain.replace(':8080', ':8081') },
    { uuid: '' }, { uuid: 'bad\nuuid' }, { uuid: 'not-a-uuid' },
  ]
  for (const changed of variants) await t.test(JSON.stringify(changed), async (t) => {
    const f = await fixture(t)
    f.instances.set(domain, instance(domain))
    f.instances.set(member('b'), instance(member('b')))
    const bad = instance(member('c'))
    Object.assign(bad.attributes, changed)
    f.instances.set(bad.attributes.domain, bad)
    record(f, [domain, member('b')])
    const result = await f.run(['cleanup-org', domain])
    equal(result.code, 1, result.output)
    match(result.output, /scope|identity|instance list|Domain must/i)
    equal(f.requests.filter((r) => r.method !== 'GET').length, 0)
    equal(f.instances.size, 3)
  })
})

test('cleanup-org refuses a non-organization root even if unrelated valid fixture members exist', async (t) => {
  const f = await fixture(t)
  f.instances.set(domain, instance(domain, false))
  f.instances.set(member('b'), instance(member('b')))
  record(f, [domain, member('b')])
  const result = await f.run(['cleanup-org', domain])
  equal(result.code, 1, result.output)
  match(result.output, /scope|identity/i)
  equal(f.requests.filter((r) => r.method !== 'GET').length, 0)
})

test('per-user data has explicit owners, isolated hosts and tokens, and root backlinks', async (t) => {
  const f = await fixture(t)
  const result = await f.run(['seed-org', domain], {
    SEED_USERS: '2', SEED_FILES: '2', SEED_DRIVES: '1', SEED_STACK_URL: f.origins.stack,
  })
  equal(result.code, 0, result.output)
  const names = creations(f.requests).slice(1).map((r) => r.url.searchParams.get('Domain')!)
  const tokens = f.requests.filter((r) => r.url.pathname === '/instances/token')
  deepEqual(tokens.map((r) => r.url.searchParams.get('Domain')), names)
  for (const name of names) {
    ok(result.output.includes(`Seed owner: ${name}; files=2; drives=1`))
    const owned = f.requests.filter((r) => r.api === 'stack' && r.headers.host === name)
    deepEqual(owned.map((r) => `${r.method} ${r.url.pathname}`), [
      'POST /files/io.cozy.files.root-dir', 'POST /files/io.cozy.files.root-dir',
      'POST /sharings/drives', `/files/root-${names.indexOf(name) + 1}`, '/files/io.cozy.files.root-dir',
    ].map((value) => value.startsWith('/') ? `GET ${value}` : value))
    for (const r of owned) equal(r.headers.authorization, `Bearer token-${name}`)
    for (const r of owned.filter((r) => r.method === 'POST' && r.url.pathname === '/files/io.cozy.files.root-dir')) {
      equal(r.body.length, 1024)
      equal(r.headers['content-md5'], createHash('md5').update(r.body).digest('base64'))
    }
  }
  equal(f.requests.filter((r) => r.api === 'stack' && r.headers.host === domain).length, 0)
  equal(f.requests.filter((r) => r.api === 'stack').length, 10)
  const manifest = JSON.parse(readFileSync(f.manifest, 'utf8'))
  deepEqual(manifest.instances.map((i: any) => [i.role, i.files.length, i.drives.length, i.root_entries]),
    [['organization', 0, 0, undefined], ['member', 2, 1, 3], ['member', 2, 1, 3]])
})

test('final organization membership must retain exact identities and both organization fields', async (t) => {
  for (const field of ['id', 'uuid', 'org_id', 'org_domain', 'extra']) await t.test(field, async (t) => {
    const f = await fixture(t, (r, res) => {
      if (r.method !== 'GET' || r.url.pathname !== '/instances' || !f.instances.size) return
      const data = JSON.parse(JSON.stringify([...f.instances.values()])) as Instance[]
      const item = data[1]!
      if (field === 'id') item.id = 'changed'
      else if (field === 'extra') data.push(instance(member('b')))
      else Object.assign(item.attributes, { [field]: 'changed' })
      res.end(JSON.stringify({ data }))
      return true
    })
    const result = await f.run()
    equal(result.code, 1, result.output)
    match(result.output, /membership|scope|identity/i)
    equal(f.instances.size, 4)
    ok(f.requests.every((r) => r.method !== 'DELETE'))
    ok(!result.output.includes('Seed complete:'))
  })
})

test('organization zero users is valid and generated roots retain fixture syntax', async (t) => {
  const f = await fixture(t)
  const result = await f.run(['seed-org'], { SEED_DOMAIN_SUFFIX: 'localhost:8080', SEED_USERS: '0' })
  equal(result.code, 0, result.output)
  equal(f.instances.size, 1)
  const root = [...f.instances.values()][0]!
  match(root.attributes.domain, /^seed-[a-f0-9]{32}\.localhost:8080$/)
  equal(root.attributes.org_id, root.attributes.domain.split('.')[0])
  equal(root.attributes.org_domain, `${root.attributes.org_id}.seed.invalid`)
  ok(result.output.includes('users=0; files=0; drives=0'))
  ok(f.requests.every((r) => r.api === 'admin' && r.url.pathname !== '/instances/token'))
})

test('organization count and command validation precede every request or domain log', async (t) => {
  const f = await fixture(t)
  const cases: [string[], Record<string, string | undefined>][] = [
    [['cleanup-org'], {}], [['seed-org', domain, 'extra'], {}],
    [['cleanup-org', 'ordinary.localhost'], {}],
    ...['-1', '1001', '1.5', '1e2', '', '3\n'].map((value): [string[], Record<string, string>] =>
      [['seed-org', domain], { SEED_USERS: value }]),
    [['seed-org', domain], { SEED_FILES: '-1' }], [['seed-org', domain], { SEED_DRIVES: '100001' }],
    [['seed-org', domain], { SEED_FILES: '1' }],
  ]
  for (const [args, env] of cases) {
    const result = await f.run(args, env)
    equal(result.code, 1, result.output)
    equal(f.requests.length, 0)
    ok(!result.output.includes('Seed instance:') && !result.output.includes(password))
  }
  const blocked = await fixture(t, (_r, res) => { res.writeHead(401).end(); return true })
  const upper = await blocked.run(['seed-org', domain], { SEED_USERS: '1000' })
  match(upper.output, /Instance lookup failed: HTTP 401/)
  equal(blocked.requests.length, 1)
  equal(creations(blocked.requests).length, 0)
})

test('seed-org refuses existing roots, planned member domains, and orphan scope on later pages', async (t) => {
  for (const mode of ['root', 'member', 'org_id', 'org_domain']) await t.test(mode, async (t) => {
    const f = await fixture(t, (r, res) => {
      if (mode !== 'member' || r.method !== 'GET' || !r.url.pathname.startsWith('/instances/')) return
      const name = r.url.pathname.slice('/instances/'.length)
      if (name === domain) return
      res.end(JSON.stringify({ data: instance(name, false) }))
      return true
    })
    if (mode === 'root') f.instances.set(domain, instance(domain))
    if (mode.startsWith('org_')) {
      for (let index = 0; index < 12; index++) {
        const name = `ordinary-${index}.localhost:8080`
        f.instances.set(name, instance(name, false))
      }
      const orphan = instance(member('b'), false)
      Object.assign(orphan.attributes, { [mode]: mode === 'org_id' ? orgID : orgDomain })
      f.instances.set(member('b'), orphan)
    }
    const result = await f.run()
    equal(result.code, 1, result.output)
    match(result.output, /already exists/)
    ok(f.requests.every((r) => r.method === 'GET'))
  })
})

test('organization creation and readback reject malformed identities before any population', async (t) => {
  for (const phase of ['created', 'readback']) {
    for (const field of ['id', 'domain', 'uuid', 'org_id', 'org_domain']) {
      await t.test(`${phase} ${field}`, async (t) => {
        const f = await fixture(t, (r, res) => {
          const name = r.url.searchParams.get('Domain') ?? r.url.pathname.slice('/instances/'.length)
          const hit = phase === 'created' ? r.method === 'POST' && r.url.pathname === '/instances' :
            r.method === 'GET' && r.url.pathname.startsWith('/instances/') && f.instances.has(name)
          if (!hit) return
          const data: Instance = phase === 'created' ? { id: 'bad identity', attributes: {
            domain: name, uuid: r.url.searchParams.get('UUID')!, org_id: orgID, org_domain: orgDomain,
          } } : structuredClone(f.instances.get(name)!)
          if (field === 'id') data.id = 'bad identity'
          else {
            if (phase === 'created') data.id = 'created-id'
            Object.assign(data.attributes, { [field]: 'wrong' })
          }
          res.writeHead(phase === 'created' ? 201 : 200).end(JSON.stringify({ data }))
          return true
        })
        const result = await f.run(['seed-org', domain], { SEED_FILES: '1', SEED_STACK_URL: f.origins.stack })
        equal(result.code, 1, result.output)
        match(result.output, /identity|resource/)
        ok(f.requests.every((r) => r.api === 'admin' && r.method !== 'DELETE' &&
          r.url.pathname !== '/instances/token'))
        equal(creations(f.requests).length, 1)
      })
    }
  }
})

test('accepted organization writes retain partial instances without retries or automatic cleanup', async (t) => {
  for (const mode of ['create-disconnect', 'create-timeout', 'file-disconnect', 'file-timeout', 'backlink']) {
    await t.test(mode, async (t) => {
      let failures = 0
      const f = await fixture(t, (r, res) => {
        const name = r.url.searchParams.get('Domain')!
        const createHit = mode.startsWith('create-') && r.method === 'POST' && r.url.pathname === '/instances' &&
          name !== domain
        const fileHit = mode.startsWith('file-') && r.api === 'stack'
        const backlink = mode === 'backlink' && r.api === 'stack' && r.method === 'GET'
        if (!createHit && !fileHit && !backlink) return
        failures++
        if (createHit) f.instances.set(name, { id: 'accepted-member', attributes: {
          domain: name, uuid: r.url.searchParams.get('UUID')!, org_id: orgID, org_domain: orgDomain,
        } })
        if (backlink) res.end(JSON.stringify({ data: {
          id: 'root-1', type: 'io.cozy.files', attributes: { type: 'directory' },
        } }))
        else if (mode.endsWith('disconnect')) res.destroy()
        else {
          res.writeHead(201).write(' ')
          const finish = setTimeout(() => res.end(password), 1000)
          res.on('close', () => clearTimeout(finish))
        }
        return true
      })
      const result = await f.run(['seed-org', domain], {
        SEED_FILES: mode === 'backlink' ? '0' : '1', SEED_DRIVES: mode === 'backlink' ? '1' : '0',
        SEED_STACK_URL: f.origins.stack, SEED_REQUEST_TIMEOUT_MS: '100',
      })
      equal(result.code, 1, result.output)
      match(result.output, /may be partial.*explicit cleanup.*writes have stopped/)
      if (mode === 'backlink') match(result.output, /root or backlink mismatch/)
      equal(failures, 1)
      equal(f.instances.size, 2)
      ok(f.instances.has(domain))
      equal(creations(f.requests).length, 2)
      ok(f.requests.every((r) => r.method !== 'DELETE'))
      ok(!result.output.includes('Seed complete:') && !result.output.includes(password))
    })
  }
})

test('cleanup-org handles partial fixtures, missing roots, zero members and absent scope', async (t) => {
  for (const mode of ['missing-root', 'root-only', 'absent']) await t.test(mode, async (t) => {
    const f = await fixture(t)
    if (mode === 'missing-root') f.instances.set(member('b'), instance(member('b')))
    if (mode === 'root-only') f.instances.set(domain, instance(domain))
    const unrelated = instance(member('f'), false)
    f.instances.set(member('f'), unrelated)
    record(f, [domain, member('b')])
    const result = await f.run(['cleanup-org', domain])
    equal(result.code, 0, result.output)
    deepEqual([...f.instances.values()], [unrelated])
    deepEqual(f.requests.filter((r) => r.method === 'DELETE').map((r) => r.url.pathname),
      mode === 'absent' ? [] : [`/instances/${mode === 'missing-root' ? member('b') : domain}`])
  })
})

test('cleanup-org rechecks domain, ID, UUID and organization fields before deletion', async (t) => {
  for (const field of ['id', 'domain', 'uuid', 'org_id', 'org_domain']) await t.test(field, async (t) => {
    const f = await fixture(t, (r, res) => {
      if (r.method !== 'GET' || r.url.pathname !== `/instances/${member('b')}`) return
      const changed = instance(member('b'))
      if (field === 'id') changed.id = 'replacement'
      else Object.assign(changed.attributes, { [field]: 'replacement' })
      res.end(JSON.stringify({ data: changed }))
      return true
    })
    f.instances.set(domain, instance(domain))
    f.instances.set(member('b'), instance(member('b')))
    record(f, [domain, member('b')])
    const result = await f.run(['cleanup-org', domain])
    equal(result.code, 1, result.output)
    match(result.output, /identity|scope|Domain must/)
    equal(f.requests.filter((r) => r.method === 'DELETE').length, 0)
    equal(f.instances.size, 2)
  })
})

test('cleanup-org lost DELETE replies warn about partial state and retain the root without retries', async (t) => {
  const f = await fixture(t, (r, res) => {
    if (r.method !== 'DELETE' || r.url.pathname !== `/instances/${member('b')}`) return
    f.instances.delete(member('b'))
    res.destroy()
    return true
  })
  f.instances.set(domain, instance(domain))
  f.instances.set(member('b'), instance(member('b')))
  record(f, [domain, member('b')])
  const result = await f.run(['cleanup-org', domain])
  equal(result.code, 1, result.output)
  match(result.output, /may be partial.*explicit cleanup.*writes have stopped/)
  equal(f.requests.filter((r) => r.method === 'DELETE').length, 1)
  ok(f.instances.has(domain))
  equal(f.instances.size, 1)
  ok(!result.output.includes('Cleanup complete:'))
})

test('cleanup-org refuses failed deletions, missing absence proofs and leftover members', async (t) => {
  for (const mode of ['delete', 'member-readback', 'members-remain', 'root-readback', 'scope-remains']) {
    await t.test(mode, async (t) => {
      const f = await fixture(t, (r, res) => {
        if (mode === 'delete' && r.method === 'DELETE') {
          res.writeHead(500).end(password)
          return true
        }
        if (r.method !== 'GET') return
        if ((mode === 'member-readback' && r.url.pathname === `/instances/${member('b')}` &&
          !f.instances.has(member('b'))) || (mode === 'root-readback' &&
          r.url.pathname === `/instances/${domain}` && !f.instances.has(domain))) {
          res.end(JSON.stringify({ data: instance(r.url.pathname.slice('/instances/'.length)) }))
          return true
        }
        if (r.url.pathname !== '/instances') return
        const leftover = mode === 'members-remain' && !f.instances.has(member('b')) && f.instances.has(domain)
        const final = mode === 'scope-remains' && !f.instances.has(domain)
        if (!leftover && !final) return
        res.end(JSON.stringify({ data: [...f.instances.values(), instance(member('c'))] }))
        return true
      })
      f.instances.set(domain, instance(domain))
      f.instances.set(member('b'), instance(member('b')))
      record(f, [domain, member('b')])
      const result = await f.run(['cleanup-org', domain])
      equal(result.code, 1, result.output)
      match(result.output, /may be partial/)
      ok(!result.output.includes('Cleanup complete:') && !result.output.includes(password))
      const deletes = f.requests.filter((r) => r.method === 'DELETE')
      equal(deletes.length, ['root-readback', 'scope-remains'].includes(mode) ? 2 : 1)
      equal(f.instances.has(domain), !['root-readback', 'scope-remains'].includes(mode))
    })
  }
})

test('registry enumeration refuses malformed pages and repeated identities before writes', async (t) => {
  for (const [index, value] of ['invalid', { data: null }, { data: [null] }, { data: [{ id: 'x' }] },
    { data: [instance(member('b')), instance(member('b'))] },
    { data: [], links: 'not-links' }, { data: [], links: [] }, { data: [], links: { next: '' } }].entries()) {
    await t.test(`malformed page ${index}`, async (t) => {
      const f = await fixture(t, (r, res) => {
        if (r.url.pathname !== '/instances' || r.method !== 'GET') return
        res.end(typeof value === 'string' ? value : JSON.stringify(value))
        return true
      })
      const result = await f.run()
      equal(result.code, 1, result.output)
      match(result.output, /Invalid instance list/)
      equal(creations(f.requests).length, 0)
    })
  }
})
