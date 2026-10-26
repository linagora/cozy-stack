import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { createHash } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer, type IncomingHttpHeaders, type RequestListener, type ServerResponse } from 'node:http'
import { createServer as createSecureServer } from 'node:https'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { type TLSSocket } from 'node:tls'
import { fileURLToPath } from 'node:url'

const domain = `seed-${'a'.repeat(32)}.localhost:8080`
const password = 'private-admin-password'
const token = 'private-cli-token'
const script = fileURLToPath(new URL('./seed-scale-data.ts', import.meta.url))
const certificate = fileURLToPath(new URL('./fixtures/seed-loopback-cert.pem', import.meta.url))
const md5 = createHash('md5').update(Buffer.alloc(1024, 's')).digest('base64')
const testTLS = { cert: readFileSync(certificate),
  key: readFileSync(new URL('./fixtures/seed-loopback-key.pem', import.meta.url)) }
interface Request {
  api: string
  method: string
  url: URL
  rawPath: string
  headers: IncomingHttpHeaders
  body: Buffer
  outputBefore: string
  servername: TLSSocket['servername'] | undefined
}
type Override = (request: Request, response: ServerResponse) => boolean | void

async function fixture(t: TestContext, override?: Override, tlsHost?: string) {
  const requests: Request[] = []
  const unrelated = `seed-${'b'.repeat(32)}.localhost:8080`
  const instances = new Map([[unrelated, { id: 'unrelated', attributes: { domain: unrelated, uuid: 'other' } }]])
  let file = 0
  let drive = 0
  let output = ''
  const origins: Record<string, string> = {}
  for (const api of ['admin', 'stack']) {
    const handler: RequestListener = async (req, res) => {
      const chunks: Buffer[] = []
      for await (const chunk of req) chunks.push(Buffer.from(chunk))
      const request: Request = {
        api, method: req.method!, url: new URL(req.url!, 'http://localhost'), rawPath: req.url!,
        headers: req.headers, body: Buffer.concat(chunks),
        outputBefore: output,
        servername: (req.socket as TLSSocket).servername,
      }
      requests.push(request)
      if (override?.(request, res)) return
      const { pathname, searchParams } = request.url
      const send = (status: number, value?: unknown) => {
        res.writeHead(status, { 'Content-Type': 'application/vnd.api+json' })
        res.end(value === undefined ? '' : JSON.stringify(value))
      }
      if (api === 'admin' && pathname === '/instances' && req.method === 'POST') {
        const name = searchParams.get('Domain')!
        const instance = { id: `instance-${instances.size}`, attributes: {
          domain: name, uuid: searchParams.get('UUID')!,
        } }
        instances.set(name, instance)
        send(201, { data: instance })
      } else if (api === 'admin' && pathname === '/instances/token') {
        res.end(token)
      } else if (api === 'admin' && pathname.startsWith('/instances/')) {
        const name = pathname.slice('/instances/'.length)
        if (req.method === 'DELETE') {
          const existed = instances.delete(name)
          send(existed ? 204 : 404)
        } else send(instances.has(name) ? 200 : 404, { data: instances.get(name) })
      } else if (api === 'stack' && pathname === '/files/io.cozy.files.root-dir' && req.method === 'GET') {
        send(200, { data: { id: 'io.cozy.files.root-dir', relationships: { contents: { meta: { count: file + drive } } } } })
      } else if (api === 'stack' && pathname === '/files/io.cozy.files.root-dir') {
        send(201, { data: { id: `file-${++file}`, type: 'io.cozy.files' } })
      } else if (api === 'stack' && pathname.startsWith('/files/file-')) {
        send(200, { data: { id: pathname.slice('/files/'.length), type: 'io.cozy.files',
          attributes: { md5sum: md5, size: '1024', dir_id: 'io.cozy.files.root-dir' } } })
      } else if (api === 'stack' && pathname === '/sharings/drives') {
        drive++
        send(201, { data: { id: `sharing-${drive}`, attributes: {
          rules: [{ values: [`root-${drive}`] }],
        } } })
      } else if (api === 'stack' && pathname.startsWith('/files/root-')) {
        const id = pathname.slice('/files/root-'.length)
        send(200, { data: { id: `root-${id}`, type: 'io.cozy.files', attributes: { type: 'directory' },
          relationships: { referenced_by: { data: [{ type: 'io.cozy.sharings', id: `sharing-${id}` }] } },
        } })
      } else send(500, { error: 'unexpected route' })
    }
    const secure = api === 'stack' && tlsHost
    const server = secure ? createSecureServer(testTLS, handler) : createServer(handler)
    const host = secure ? tlsHost : '127.0.0.1'
    const address = host === 'localhost' ? '127.0.0.1' : host.replace(/^\[|\]$/g, '')
    await new Promise<void>((resolve) => server.listen(0, address, resolve))
    origins[api] = `${secure ? 'https' : 'http'}://${host}:${(server.address() as { port: number }).port}`
    t.after(async () => {
      server.closeAllConnections()
      await new Promise<void>((resolve) => server.close(() => resolve()))
    })
  }
  const dir = mkdtempSync(join(tmpdir(), 'seed-scale-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const manifest = join(dir, 'manifest.json')
  async function run(args = ['seed', domain], env: Record<string, string | undefined> = {}) {
    output = ''
    const inherited = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('SEED_')))
    const child = spawn(process.execPath, ['--import', 'tsx', script, ...args], {
      env: { ...inherited, SEED_ADMIN_URL: origins.admin, SEED_STACK_URL: origins.stack,
        SEED_ADMIN_PASSWORD: password, SEED_FILES: '2', SEED_DRIVES: '0', SEED_MANIFEST: manifest, ...env },
      timeout: 6000,
    })
    child.stdout.on('data', (chunk) => { output += String(chunk) })
    child.stderr.on('data', (chunk) => { output += String(chunk) })
    const code = await new Promise<number | null>((resolve, reject) => {
      child.on('error', reject)
      child.on('close', resolve)
    })
    return { code, output }
  }
  return { requests, instances, unrelated, run, manifest }
}

test('file-only seed uses dedicated APIs, Basic auth and exact public Host including port', async (t) => {
  const f = await fixture(t)
  const result = await f.run()
  equal(result.code, 0, result.output)
  match(result.output, new RegExp(`Seed instance: ${domain}`))
  ok(result.output.includes(`Seed complete: ${domain}; files=2; drives=0`))
  equal(f.requests[0]!.outputBefore, `Seed instance: ${domain}\n`)
  deepEqual(f.requests.map((r) => `${r.api} ${r.method} ${r.url.pathname}`), [
    `admin GET /instances/${domain}`, 'admin POST /instances', `admin GET /instances/${domain}`,
    'admin POST /instances/token', 'stack POST /files/io.cozy.files.root-dir',
    'stack POST /files/io.cozy.files.root-dir', 'stack GET /files/io.cozy.files.root-dir',
  ])
  for (const r of f.requests.filter((r) => r.api === 'admin')) {
    equal(r.headers.authorization, `Basic ${Buffer.from(`admin:${password}`).toString('base64')}`)
    ok(!r.rawPath.split('?')[0]!.includes('%3A'))
  }
  const creation = f.requests[1]!.url.searchParams
  equal(creation.get('Domain'), domain)
  match(creation.get('UUID')!, /^[a-f0-9-]{36}$/)
  equal(creation.get('Locale'), 'en')
  equal(creation.get('CouchCluster'), '-1')
  equal(creation.get('SwiftLayout'), '-1')
  equal(creation.get('AutoUpdate'), 'false')
  match(creation.get('Email')!, /^[^@]+@spam\.cozycloud\.cc$/)
  ok(creation.get('PublicName'))
  const credentials = f.requests[3]!.url.searchParams
  equal(credentials.get('Domain'), domain)
  equal(credentials.get('Audience'), 'cli')
  equal(credentials.get('Scope'), 'io.cozy.files io.cozy.sharings')
  const files = f.requests.filter((r) => r.api === 'stack' && r.method === 'POST')
  for (const [index, r] of files.entries()) {
    equal(r.headers.host, domain)
    equal(r.headers.authorization, `Bearer ${token}`)
    equal(r.url.searchParams.get('Type'), 'file')
    equal(r.url.searchParams.get('Name'), `seed-file-${index + 1}.txt`)
    equal(r.body.length, 1024)
    equal(r.headers['content-length'], '1024')
    equal(r.headers['content-type'], 'text/plain')
    equal(r.headers['content-md5'], createHash('md5').update(r.body).digest('base64'))
  }
  deepEqual(files[0]!.body, files[1]!.body)
  ok(!result.output.includes(password) && !result.output.includes(token))
  ok(f.instances.has(f.unrelated))
  const { preparation, ...manifest } = JSON.parse(readFileSync(f.manifest, 'utf8'))
  // Seeding is preparation, timed and sized apart from any measured work.
  deepEqual(preparation.bytes, 2048)
  ok(preparation.duration_ms >= 0 && Date.parse(preparation.ended_at) >= Date.parse(preparation.started_at))
  deepEqual(manifest, {
    version: 1, profile: 'flat', status: 'complete', domain, organization: null,
    parameters: { users: 0, files: 2, drives: 0, context: null }, content: { bytes: 1024, md5 },
    instances: [{ domain, uuid: creation.get('UUID'), role: 'owner', created: true,
      files: ['file-1', 'file-2'], drives: [], root_entries: 2 }],
  })
  ok(!readFileSync(f.manifest, 'utf8').includes(token))
})

test('HTTPS verifies the connection hostname while preserving the exact tenant Host', async (t) => {
  for (const host of ['localhost', '127.0.0.1', '[::1]']) await t.test(host, async (t) => {
    const f = await fixture(t, undefined, host)
    const result = await f.run(['seed', domain], {
      NODE_EXTRA_CA_CERTS: certificate, NODE_TLS_REJECT_UNAUTHORIZED: '1',
      SEED_FILES: '1', SEED_DRIVES: '1',
    })
    equal(result.code, 0, result.output)
    ok(result.output.includes(`Seed complete: ${domain}; files=1; drives=1`))
    const publicRequests = f.requests.filter((r) => r.api === 'stack')
    deepEqual(publicRequests.map((r) => `${r.method} ${r.url.pathname}`), [
      'POST /files/io.cozy.files.root-dir', 'POST /sharings/drives', 'GET /files/root-1',
      'GET /files/io.cozy.files.root-dir',
    ])
    for (const r of publicRequests) {
      equal(r.headers.host, domain)
      equal(r.headers.authorization, `Bearer ${token}`)
      equal(r.servername, host === 'localhost' ? host : false)
    }
    ok(!result.output.includes('DeprecationWarning'))
  })
})

test('HTTPS rejects untrusted certificates and trusted certificates for a different connection IP', async (t) => {
  for (const trusted of [false, true]) await t.test(trusted ? 'mismatched IP' : 'untrusted', async (t) => {
    const f = await fixture(t, undefined, trusted ? '127.0.0.2' : 'localhost')
    const result = await f.run(['seed', domain], {
      NODE_EXTRA_CA_CERTS: trusted ? certificate : undefined, NODE_TLS_REJECT_UNAUTHORIZED: '1',
    })
    equal(result.code, 1, result.output)
    match(result.output, /API request failed.*may be partial/)
    ok(!result.output.includes('Seed complete:'))
    equal(f.requests.length, 4)
    ok(f.requests.every((r) => r.api === 'admin' && r.method !== 'DELETE'))
    ok(f.instances.has(domain))
  })
})

test('cleanup deletes only the recorded instance, reads back absence and removes the manifest', async (t) => {
  const f = await fixture(t)
  equal((await f.run()).code, 0)
  const uuid = JSON.parse(readFileSync(f.manifest, 'utf8')).instances[0].uuid
  f.requests.splice(0)
  const env = { SEED_STACK_URL: undefined, SEED_FILES: 'invalid', SEED_DRIVES: '-1',
    SEED_ADMIN_USERNAME: 'fixture-admin' }
  const result = await f.run(['cleanup', domain], env)
  equal(result.code, 0, result.output)
  ok(result.output.includes(`Cleanup complete: ${domain}`))
  equal(f.instances.has(domain), false)
  ok(f.instances.has(f.unrelated))
  deepEqual(f.requests.map((r) => `${r.method} ${r.rawPath}`),
    ['GET', 'DELETE', 'GET'].map((method) => `${method} /instances/${domain}`))
  for (const r of f.requests) {
    equal(r.headers.authorization, `Basic ${Buffer.from(`fixture-admin:${password}`).toString('base64')}`)
  }
  ok(uuid)
  // The manifest is gone with the fixture: cleaning up again fails closed.
  const again = await f.run(['cleanup', domain], env)
  equal(again.code, 1)
  match(again.output, /No readable fixture manifest/)
  equal(f.requests.length, 3)
})

test('cleanup fails closed without a manifest of this fixture and never deletes another instance', async (t) => {
  const f = await fixture(t)
  const other = { id: 'other', attributes: { domain, uuid: 'another-run' } }
  f.instances.set(domain, other)
  for (const content of [undefined, 'not json', JSON.stringify({ version: 2 })]) {
    if (content !== undefined) writeFileSync(f.manifest, content)
    const result = await f.run(['cleanup', domain])
    equal(result.code, 1)
    match(result.output, /fixture manifest/)
  }
  equal(f.requests.length, 0)
  // A recorded domain now held by another UUID is someone else's instance.
  writeFileSync(f.manifest, JSON.stringify({
    version: 1, profile: 'flat', status: 'failed', domain, organization: null,
    parameters: { users: 0, files: 2, drives: 0, context: null }, content: { bytes: 1024, md5 },
    instances: [{ domain, uuid: 'this-run', role: 'owner', created: false, files: [], drives: [] }],
  }))
  const result = await f.run(['cleanup', domain])
  equal(result.code, 1)
  match(result.output, /not the recorded instance; kept/)
  equal(f.instances.get(domain), other)
  ok(f.instances.has(f.unrelated))
  ok(f.requests.every((r) => r.method === 'GET'))
})

test('verify checks recorded identities, objects, content and root entries before use and after a run', async (t) => {
  let extra = 0
  let changed = ''
  const f = await fixture(t, (r, res) => {
    if (r.api !== 'stack' || r.method !== 'GET') return
    if (extra && r.url.pathname === '/files/io.cozy.files.root-dir') {
      res.end(JSON.stringify({ data: { id: r.url.pathname.slice(7),
        relationships: { contents: { meta: { count: 3 + extra } } } } }))
      return true
    }
    if (!changed || r.url.pathname !== '/files/file-2') return
    res.end(JSON.stringify({ data: { id: 'file-2',
      attributes: { md5sum: changed, size: '1024', dir_id: 'io.cozy.files.root-dir' } } }))
    return true
  })
  equal((await f.run(['seed', domain], { SEED_DRIVES: '1' })).code, 0)
  f.requests.splice(0)
  const verified = await f.run(['verify', domain], { SEED_FILES: 'ignored' })
  equal(verified.code, 0, verified.output)
  ok(verified.output.includes(`Fixture verified: ${domain}`))
  deepEqual(f.requests.map((r) => `${r.api} ${r.method} ${r.url.pathname}`), [
    `admin GET /instances/${domain}`, 'admin POST /instances/token', 'stack GET /files/file-1',
    'stack GET /files/file-2', 'stack GET /files/root-1', 'stack GET /files/io.cozy.files.root-dir',
  ])
  // A run that leaves an entry at the root or changes a file no longer matches the fixture.
  extra = 1
  let result = await f.run(['verify', domain])
  equal(result.code, 1)
  match(result.output, /the root holds 4 entries; the manifest records 3/)
  extra = 0
  changed = 'other'
  result = await f.run(['verify', domain])
  equal(result.code, 1)
  match(result.output, /file file-2 changed/)
  f.instances.set(domain, { id: 'replaced', attributes: { domain, uuid: 'another' } })
  result = await f.run(['verify', domain])
  equal(result.code, 1)
  match(result.output, /is not the recorded instance/)
  ok(f.requests.every((r) => r.method === 'GET' || r.url.pathname === '/instances/token'))
})

test('a partial generation records its confirmed objects; verify refuses it and cleanup removes only it',
  async (t) => {
    const f = await fixture(t, (r, res) => {
      if (r.method !== 'POST' || r.url.searchParams.get('Name') !== 'seed-file-2.txt') return
      res.writeHead(500).end()
      return true
    })
    const seeded = await f.run(['seed', domain], { SEED_FILES: '3' })
    equal(seeded.code, 1)
    match(seeded.output, /may be partial/)
    const manifest = JSON.parse(readFileSync(f.manifest, 'utf8'))
    equal(manifest.status, 'failed')
    deepEqual(manifest.instances.map((i: any) => [i.created, i.files]), [[true, ['file-1']]])
    const verified = await f.run(['verify', domain])
    equal(verified.code, 1)
    match(verified.output, /manifest is failed; only a complete fixture is usable/)
    const cleaned = await f.run(['cleanup', domain])
    equal(cleaned.code, 0, cleaned.output)
    deepEqual([...f.instances.keys()], [f.unrelated])
  })

test('a lost creation reply leaves the planned identity, which cleanup can still prove and delete', async (t) => {
  const f = await fixture(t, (r, res) => {
    if (r.method !== 'POST' || r.url.pathname !== '/instances') return
    f.instances.set(domain, { id: 'accepted', attributes: { domain, uuid: r.url.searchParams.get('UUID')! } })
    res.destroy()
    return true
  })
  equal((await f.run()).code, 1)
  const manifest = JSON.parse(readFileSync(f.manifest, 'utf8'))
  deepEqual([manifest.status, manifest.instances[0].created], ['failed', false])
  equal(manifest.instances[0].uuid, f.instances.get(domain)!.attributes.uuid)
  const cleaned = await f.run(['cleanup', domain])
  equal(cleaned.code, 0, cleaned.output)
  deepEqual([...f.instances.keys()], [f.unrelated])
})

test('seed refuses to replace an existing manifest before any write', async (t) => {
  const f = await fixture(t)
  writeFileSync(f.manifest, 'another fixture')
  const result = await f.run()
  equal(result.code, 1)
  match(result.output, /an existing manifest may record another fixture/)
  ok(f.requests.every((r) => r.method === 'GET'))
  equal(readFileSync(f.manifest, 'utf8'), 'another fixture')
})

test('the tree profile seeds the same structure and bytes for a seed, and verify proves every parent', async (t) => {
  const seeded = async (seed: string) => {
    // Directories and files anywhere, as the Stack keeps them; drive roots and the root count stay default.
    const items = new Map<string, Record<string, unknown>>()
    const f = await fixture(t, (r, res) => {
      const id = decodeURIComponent(r.url.pathname.slice('/files/'.length))
      if (r.api !== 'stack' || !r.url.pathname.startsWith('/files/')) return
      if (r.method === 'POST') {
        const created = `item-${items.size + 1}`
        const type = r.url.searchParams.get('Type')!
        items.set(created, { type, name: r.url.searchParams.get('Name'), dir_id: id,
          ...(type === 'file' ? { md5sum: r.headers['content-md5'], size: String(r.body.length) } : {}) })
        res.writeHead(201).end(JSON.stringify({ data: { id: created, type: 'io.cozy.files' } }))
        return true
      }
      if (!items.has(id)) return
      res.end(JSON.stringify({ data: { id, type: 'io.cozy.files', attributes: items.get(id) } }))
      return true
    })
    const result = await f.run(['seed', domain], { SEED_PROFILE: 'tree', SEED_GENERATION_SEED: seed, SEED_FILES: '9' })
    equal(result.code, 0, result.output)
    ok(result.output.includes('profile=tree; files=7; drives=1'))
    return { f, items, manifest: JSON.parse(readFileSync(f.manifest, 'utf8')) }
  }
  const first = await seeded('alpha')
  const again = await seeded('alpha')
  const other = await seeded('beta')
  const shape = (items: Map<string, Record<string, unknown>>) => [...items.values()]
  // The same seed gives the same structure and bytes; another seed changes the bytes, not the structure.
  deepEqual(shape(again.items), shape(first.items))
  const structure = (items: Map<string, Record<string, unknown>>) => shape(items).map(({ md5sum, ...rest }) => rest)
  deepEqual(structure(other.items), structure(first.items))
  ok(first.manifest.content.md5 !== other.manifest.content.md5)
  const m = first.manifest
  deepEqual([m.profile, m.profile_version, m.generation_seed, m.parameters.files, m.parameters.drives],
    ['tree', 1, 'alpha', 7, 1])
  deepEqual([m.instances[0].dirs.length, m.instances[0].files.length, m.instances[0].drives.length], [5, 7, 1])
  equal(m.preparation.bytes, m.content.bytes)
  ok(m.content.bytes < 2 << 20, 'the tree stays within shared DEV byte budgets')
  // Directories nest, and drive content sits under the drive root.
  const byName = (name: string) => [...first.items.entries()].find(([, item]) => item.name === name)!
  equal(byName('specs')[1].dir_id, byName('alpha')[0])
  equal(byName('design.bin')[1].dir_id, byName('specs')[0])
  equal(byName('handbook')[1].dir_id, m.instances[0].drives[0].root)
  equal(byName('readme.txt')[1].dir_id, 'io.cozy.files.root-dir')

  const verified = await first.f.run(['verify', domain])
  equal(verified.code, 0, verified.output)
  byName('plan.txt')[1].dir_id = byName('archive')[0]
  match((await first.f.run(['verify', domain])).output, /file projects\/plan.txt changed/)
  byName('plan.txt')[1].dir_id = byName('projects')[0]
  const alpha = byName('alpha')[1]
  alpha.name = 'beta'
  match((await first.f.run(['verify', domain])).output, /directory projects\/alpha changed/)
  alpha.name = 'alpha'
  byName('welcome.txt')[1].md5sum = shape(other.items).find(item => item.name === 'welcome.txt')!.md5sum
  match((await first.f.run(['verify', domain])).output, /file @drive\/handbook\/welcome.txt changed/)
  writeFileSync(first.f.manifest, JSON.stringify({ ...first.manifest, profile_version: 2 }))
  const requests = first.f.requests.length
  match((await first.f.run(['verify', domain])).output, /tree profile version 2; this seeder knows version 1/)
  equal(first.f.requests.length, requests)
})

test('drive-only seed reuses shared-drive creation and reads each root backlink with the same Host', async (t) => {
  const f = await fixture(t)
  const result = await f.run(['seed', domain], { SEED_FILES: '0', SEED_DRIVES: '2' })
  equal(result.code, 0, result.output)
  ok(result.output.includes('files=0; drives=2'))
  const writes = f.requests.filter((r) => r.api === 'stack')
  deepEqual(writes.map((r) => `${r.method} ${r.url.pathname}`), [
    'POST /sharings/drives', 'GET /files/root-1', 'POST /sharings/drives', 'GET /files/root-2',
    'GET /files/io.cozy.files.root-dir',
  ])
  for (const r of writes) {
    equal(r.headers.host, domain)
    equal(r.headers.authorization, `Bearer ${token}`)
  }
  for (const [index, r] of writes.filter((r) => r.method === 'POST').entries()) {
    const data = JSON.parse(r.body.toString()).data
    equal(data.type, 'io.cozy.sharings')
    equal(data.attributes.name, `Seed shared drive ${index + 1}`)
    equal(data.attributes.description, data.attributes.name)
    equal(r.headers['content-type'], 'application/vnd.api+json')
    equal(r.headers['content-length'], String(r.body.length))
  }
})

test('seed refuses an existing disposable domain without adopting or deleting it', async (t) => {
  const f = await fixture(t)
  const existing = { id: 'existing', attributes: { domain, uuid: 'not-this-run' } }
  f.instances.set(domain, existing)
  const result = await f.run()
  equal(result.code, 1)
  match(result.output, /already exists/)
  deepEqual(f.requests.map((r) => r.method), ['GET'])
  equal(f.instances.get(domain), existing)
  ok(f.instances.has(f.unrelated))
})

test('autogenerated domains are fresh and use the configured suffix and default file count', async (t) => {
  const f = await fixture(t)
  const domains: string[] = []
  for (let attempt = 0; attempt < 2; attempt++) {
    const result = await f.run(['seed'], {
      SEED_DOMAIN_SUFFIX: 'localhost:8080', SEED_FILES: undefined, SEED_DRIVES: undefined,
    })
    equal(result.code, 0, result.output)
    const creation = f.requests.find((r) => r.url.pathname === '/instances')!
    const generated = creation.url.searchParams.get('Domain')!
    match(generated, /^seed-[a-f0-9]{32}\.localhost:8080$/)
    domains.push(generated)
    equal(f.requests.filter((r) => r.api === 'stack').length, 101)
    ok(f.requests.filter((r) => r.api === 'stack').every((r) => r.headers.host === generated))
    ok(result.output.includes('files=100; drives=0'))
    f.requests.splice(0)
    rmSync(f.manifest)
  }
  equal(new Set(domains).size, 2)
})

test('strict CLI validation happens before any network or secret-bearing output', async (t) => {
  const f = await fixture(t)
  const cases: [string[], Record<string, string | undefined>][] = [
    [[], {}], [['status', domain], {}], [['seed', domain, password], {}], [['cleanup'], {}],
    [['seed', 'load.localhost:8080'], {}], [['cleanup', `https://${domain}`], {}],
    [['seed', `${domain}/path`], {}], [['seed', `${domain}?x=y`], {}], [['cleanup', `${domain}\n`], {}],
    [['seed', domain.replace(':8080', ':0')], {}], [['seed', domain.replace(':8080', ':65536')], {}],
    [['seed', `seed-${'A'.repeat(32)}.localhost`], {}],
    [['seed', `seed-${'a'.repeat(32)}.-bad.localhost`], {}],
    [['seed'], {}], [['seed'], { SEED_DOMAIN_SUFFIX: '' }],
    [['seed'], { SEED_DOMAIN_SUFFIX: 'https://localhost:8080' }],
    [['seed'], { SEED_DOMAIN_SUFFIX: 'localhost:8080/path' }],
    [['seed', domain], { SEED_ADMIN_PASSWORD: '' }],
    [['seed', domain], { SEED_ADMIN_USERNAME: 'user:bad' }],
    [['seed', domain], { SEED_ADMIN_USERNAME: '' }],
    ...['SEED_ADMIN_URL', 'SEED_STACK_URL'].flatMap((name): [string[], Record<string, string | undefined>][] =>
      [undefined, 'ftp://127.0.0.1', 'http://127.0.0.1/path', 'http://127.0.0.1?query=1',
        'http://127.0.0.1#fragment', 'http://127.0.0.1\n', 'http://user:private-url-secret@127.0.0.1']
        .map((value) => [['seed', domain], { [name]: value }])),
    ...['-1', '1000001', '1.2', '1e2', '', '1\n'].map((value): [string[], Record<string, string>] =>
      [['seed', domain], { SEED_FILES: value }]),
    [['seed', domain], { SEED_DRIVES: '100001' }],
    [['seed', domain], { SEED_FILES: '0', SEED_DRIVES: '0' }],
    ...['0', '1.5', '2147483648'].map((value): [string[], Record<string, string>] =>
      [['seed', domain], { SEED_REQUEST_TIMEOUT_MS: value }]),
  ]
  for (const [args, env] of cases) {
    const result = await f.run(args, env)
    equal(result.code, 1, JSON.stringify([args, env]))
    ok(!result.output.includes('Seed instance:'))
    ok(!result.output.includes(password) && !result.output.includes('private-url-secret'))
    equal(f.requests.length, 0)
  }
})

test('count upper bounds are valid without scheduling a million-file test', async (t) => {
  const f = await fixture(t, (_r, res) => { res.writeHead(401).end(); return true })
  const result = await f.run(['seed', domain], { SEED_FILES: '1000000', SEED_DRIVES: '100000' })
  equal(result.code, 1)
  match(result.output, /Instance lookup failed: HTTP 401/)
  equal(f.requests.length, 1)
})

test('untrusted instance, token, file and shared-drive replies never disclose payloads or clean up', async (t) => {
  const secret = `${password} ${token}`
  const badIdentity = JSON.stringify({ data: { id: 'wrong', attributes: { domain, uuid: 'wrong' } } })
  const cases: [string, number, string][] = [
    ['POST /instances', 202, secret], ['POST /instances', 201, secret],
    ['POST /instances', 201, 'null'], ['POST /instances', 201, badIdentity],
    ['readback', 200, badIdentity], ['readback', 404, secret],
    ['POST /instances/token', 401, secret], ['POST /instances/token', 200, ''],
    ['POST /instances/token', 200, `${token}\n`],
    ['POST /files/io.cozy.files.root-dir', 500, secret],
    ['POST /files/io.cozy.files.root-dir', 201, secret],
    ['POST /files/io.cozy.files.root-dir', 201, '{"data":{"id":""}}'],
    ['POST /sharings/drives', 400, secret], ['POST /sharings/drives', 201, secret],
    ['POST /sharings/drives', 201, JSON.stringify({ data: { id: secret, attributes: { rules: [] } } })],
    ['POST /sharings/drives', 201, JSON.stringify({ data: { attributes: { rules: [{ values: ['root'] }] } } })],
  ]
  for (const [target, status, body] of cases) await t.test(`${target} ${status} ${body.length}`, async (t) => {
    let lookups = 0
    const f = await fixture(t, (r, res) => {
      if (r.method === 'GET' && r.url.pathname === `/instances/${domain}`) lookups++
      const hit = target === 'readback' ? lookups === 2 : `${r.method} ${r.url.pathname}` === target
      if (!hit) return
      res.writeHead(status).end(body)
      return true
    })
    const result = await f.run(['seed', domain], { SEED_DRIVES: '1' })
    equal(result.code, 1, result.output)
    match(result.output, /may be partial.*explicit cleanup.*writes have stopped/)
    ok(!result.output.includes('Seed complete:'))
    ok(!result.output.includes(password) && !result.output.includes(token))
    ok(!result.output.includes(' at ') && !result.output.includes('node:'))
    if (target === 'readback') equal(lookups, 2)
    else equal(f.requests.filter((r) => `${r.method} ${r.url.pathname}` === target).length, 1)
    ok(f.requests.every((r) => r.method !== 'DELETE'))
    ok(f.instances.has(f.unrelated))
  })
})

test('drive root verification refuses missing or foreign backlinks and non-directory roots', async (t) => {
  const variants = [
    { id: 'root-1', type: 'io.cozy.files', attributes: { type: 'directory' } },
    { id: 'root-1', type: 'io.cozy.files', attributes: { type: 'directory' },
      relationships: { referenced_by: { data: [{ type: 'io.cozy.sharings', id: 'foreign' }] } } },
    { id: 'root-1', type: 'io.cozy.files', attributes: { type: 'file' },
      relationships: { referenced_by: { data: [{ type: 'io.cozy.sharings', id: 'sharing-1' }] } } },
    { id: 'foreign-root', type: 'io.cozy.files', attributes: { type: 'directory' },
      relationships: { referenced_by: { data: [{ type: 'io.cozy.sharings', id: 'sharing-1' }] } } },
  ]
  for (const [index, data] of variants.entries()) await t.test(String(index), async (t) => {
    const f = await fixture(t, (r, res) => {
      if (r.url.pathname !== '/files/root-1') return
      res.end(JSON.stringify({ data }))
      return true
    })
    const result = await f.run(['seed', domain], { SEED_FILES: '0', SEED_DRIVES: '2' })
    equal(result.code, 1, result.output)
    match(result.output, /root or backlink mismatch/)
    equal(f.requests.filter((r) => r.url.pathname === '/sharings/drives').length, 1)
    ok(f.instances.has(domain))
    ok(f.requests.every((r) => r.method !== 'DELETE'))
  })
})

test('redirects are not followed, retried or used to forward credentials', async (t) => {
  for (const path of [`/instances/${domain}`, '/files/io.cozy.files.root-dir', '/sharings/drives']) {
    await t.test(path, async (t) => {
      const f = await fixture(t, (r, res) => {
        if (r.url.pathname !== path) return
        res.writeHead(302, { Location: '/must-not-follow' }).end(password)
        return true
      })
      const result = await f.run(['seed', domain], { SEED_FILES: path === '/sharings/drives' ? '0' : '1',
        SEED_DRIVES: '1' })
      equal(result.code, 1)
      match(result.output, /HTTP 302/)
      ok(!result.output.includes(password))
      equal(f.requests.filter((r) => r.url.pathname === path).length, 1)
      ok(f.requests.every((r) => r.url.pathname !== '/must-not-follow' && r.method !== 'DELETE'))
    })
  }
})

test('complete-response deadline bounds stalled headers, stalled bodies and trickling bytes', async (t) => {
  for (const mode of ['headers', 'body', 'trickle']) await t.test(mode, async (t) => {
    let closed = false
    const f = await fixture(t, (r, res) => {
      if (r.api !== 'stack') return
      if (mode !== 'headers') res.writeHead(201).write(' ')
      const pulse = mode === 'trickle' ? setInterval(() => res.write(' '), 20) : undefined
      const finish = setTimeout(() => res.end('{"data":{"id":"late-file"}}'), 1200)
      res.on('close', () => { closed = true; clearTimeout(finish); clearInterval(pulse) })
      return true
    })
    const result = await f.run(['seed', domain], { SEED_REQUEST_TIMEOUT_MS: '100' })
    equal(result.code, 1, result.output)
    match(result.output, /deadline exceeded.*timeout\/disconnect is not proof/)
    equal(closed, true)
    equal(f.requests.filter((r) => r.api === 'stack').length, 1)
    ok(f.instances.has(domain))
    ok(f.requests.every((r) => r.method !== 'DELETE'))
  })
})

test('oversized, disconnected and truncated replies retain partial data without retries or leaks', async (t) => {
  for (const mode of ['oversized', 'disconnect', 'truncated']) await t.test(mode, async (t) => {
    const f = await fixture(t, (r, res) => {
      if (r.api !== 'stack') return
      if (mode === 'oversized') res.writeHead(201).end(password + 'x'.repeat(65_537))
      else if (mode === 'disconnect') res.destroy()
      else {
        res.writeHead(201, { 'Content-Length': 1000 }).write('{')
        setImmediate(() => res.destroy())
      }
      return true
    })
    const result = await f.run(['seed', domain], { SEED_REQUEST_TIMEOUT_MS: '1000' })
    equal(result.code, 1, result.output)
    match(result.output, /may be partial/)
    if (mode === 'oversized') match(result.output, /exceeds 64 KiB/)
    ok(!result.output.includes(password) && !result.output.includes(token))
    ok(!result.output.includes(' at ') && !result.output.includes('node:'))
    equal(f.requests.filter((r) => r.api === 'stack').length, 1)
    ok(f.instances.has(domain))
    ok(f.requests.every((r) => r.method !== 'DELETE'))
  })
})

test('cleanup rejects unsuccessful DELETE and any readback other than 404', async (t) => {
  for (const [method, status] of [['DELETE', 200], ['DELETE', 302], ['DELETE', 500],
    ['GET', 200], ['GET', 401], ['GET', 500]] as const) await t.test(`${method} ${status}`, async (t) => {
    let deleted = false
    const f = await fixture(t, (r, res) => {
      if (r.method === 'DELETE') deleted = true
      if (r.method !== method || r.api !== 'admin' || (method === 'GET' && !deleted)) return
      res.writeHead(status, { Location: '/must-not-follow' }).end(password)
      return true
    })
    equal((await f.run()).code, 0)
    f.requests.splice(0)
    const result = await f.run(['cleanup', domain], { SEED_STACK_URL: undefined })
    equal(result.code, 1, result.output)
    match(result.output, new RegExp(`HTTP ${status}`))
    ok(!result.output.includes('Cleanup complete:') && !result.output.includes(password))
    deepEqual(f.requests.map((r) => r.method), method === 'DELETE' ? ['GET', 'DELETE'] : ['GET', 'DELETE', 'GET'])
    ok(f.instances.has(f.unrelated))
    // Nothing is confirmed absent, so the manifest stays for the next attempt.
    ok(readFileSync(f.manifest, 'utf8'))
  })
})
