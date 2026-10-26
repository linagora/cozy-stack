import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawn } from 'node:child_process'
import { randomUUID } from 'node:crypto'
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const script = fileURLToPath(new URL('./seed-cloudery.ts', import.meta.url))
const domain = 'dev.example.test'
const org = `seedtest.${domain}`
const member = (i: number) => `seedtest${String(i).padStart(2, '0')}.${domain}`
const unrelated = [`other01.${domain}`, `seedtestx01.${domain}`, `seedtestorg.${domain}`, 'seedtest01.other.test']

// One server plays the Cloudery (/api/...) and every Stack instance (by Host).
async function fixture(t: TestContext) {
  const instances = new Map<string, { _id: string, fqdn: string, deleting_at?: string }>()
  for (const fqdn of unrelated) instances.set(fqdn, { _id: randomUUID(), fqdn })
  const state = {
    instances, used: new Set<string>(), orgs: new Set<string>(), mints: [] as string[], created: [] as string[], emails: [] as string[],
    released: [] as string[],
    adminCalls: 0, failWorkflow: '', noAutoAccept: '', foreign: '', corrupt: false,
  }
  const workflows = new Map<string, { polls: number, fqdn: string, done: () => void }>()
  const tokens = new Map<string, string>()
  const files = new Map<string, string>()

  const server = createServer(async (req, res) => {
    let body = ''
    for await (const chunk of req) body += chunk
    const { pathname } = new URL(req.url!, 'http://x')
    const bearer = req.headers.authorization?.replace('Bearer ', '')
    const send = (status: number, value?: unknown) => {
      res.writeHead(status, { 'Content-Type': 'application/json' })
      res.end(value === undefined ? '' : typeof value === 'string' ? value : JSON.stringify(value))
    }
    const parts = pathname.split('/')
    if (pathname.startsWith('/api/')) {
      if (bearer !== (pathname.startsWith('/api/public/') ? 'public-secret' : 'partner-id')) return send(401)
      const route = `${req.method} ${parts.slice(1, 4).join('/')}`
      const id = decodeURIComponent(parts[4] ?? '')
      if (route === 'GET api/v2/organizations') return send(state.orgs.has(id) ? 200 : 404, state.orgs.has(id) ? { ldap_branch: id } : undefined)
      if (route === 'POST api/v2/organizations') {
        const { ldap_branch: branch, name, custom_domain: customDomain } = JSON.parse(body)
        if (!name || !customDomain) return send(400, { errors: ["Custom domain can't be blank"] })
        state.orgs.add(branch)
        return send(201, {})
      }
      if (route === 'GET api/v2/instances') return send(200, { count: instances.size, items: [...instances.values()] })
      if (route === 'POST api/v1/instances') {
        const params = JSON.parse(body)
        const fqdn = `${params.slug}.${params.domain}`
        if (instances.has(fqdn) || state.used.has(fqdn)) return send(409, `${fqdn} is already taken`)
        if (params.org_id !== 'seedtest' || !params.skip_email_validation) return send(400)
        state.created.push(fqdn)
        state.emails.push(params.email)
        const _id = randomUUID()
        workflows.set(`wf-${_id}`, { polls: 0, fqdn, done: () => instances.set(fqdn, { _id, fqdn }) })
        return send(201, { _id, fqdn, workflow: `wf-${_id}` })
      }
      if (route === 'GET api/v1/workflows') {
        const workflow = workflows.get(id)!
        if (workflow.polls++ === 0) return send(200, { status: 'running' })
        if (workflow.fqdn === state.failWorkflow) return send(200, { status: 'failed' })
        workflow.done()
        return send(200, { status: 'succeeded' })
      }
      const instance = [...instances.values()].find(i => i._id === id)
      if (route === 'GET api/v1/instances') return send(instance ? 200 : 404, instance)
      if (route === 'DELETE api/v1/instances' && instance) {
        instance.deleting_at = new Date().toISOString()
        workflows.set(`wf-del-${id}`, { polls: 0, fqdn: instance.fqdn, done: () => {
          instances.delete(instance.fqdn)
          state.used.add(instance.fqdn)
        } })
        return send(200, { workflow: `wf-del-${id}` })
      }
      if (route === 'DELETE api/v2/fqdns') return state.used.delete(id), state.released.push(id), send(200)
      if (route === 'POST api/public/instances' && parts[5] === 'token') {
        if (!instances.has(id)) return send(404)
        const token = `token-${tokens.size}`
        tokens.set(token, id)
        state.mints.push(id)
        return send(200, { token })
      }
      return send(500)
    }

    const host = req.headers.host!
    if (!bearer || tokens.get(bearer) !== host) return send(401)
    const route = `${req.method} ${pathname}`
    if (route === 'GET /settings/instance') {
      const orgID = host === state.foreign ? 'another' : 'seedtest'
      return send(200, { data: { attributes: { org_id: orgID, uuid: instances.get(host)!._id } } })
    }
    if (route === 'GET /settings/context') {
      const autoAccept = host !== state.noAutoAccept
      return send(200, { data: { attributes: { common_contacts: autoAccept, sharing: { auto_accept_trusted: true } } } })
    }
    if (route === 'POST /files/io.cozy.files.root-dir') {
      const id = `file-${files.size}`
      files.set(id, body)
      return send(201, { data: { id } })
    }
    if (route.startsWith('GET /files/download/')) return send(200, state.corrupt ? 'other bytes' : files.get(parts[3]!))
    if (route.startsWith('DELETE /files/')) return send(200, {})
    send(500)
  })
  const admin = createServer((_req: IncomingMessage, res: ServerResponse) => {
    state.adminCalls++
    res.end()
  })
  const origins: string[] = []
  for (const s of [server, admin]) {
    await new Promise<void>(resolve => s.listen(0, '127.0.0.1', resolve))
    origins.push(`http://127.0.0.1:${(s.address() as { port: number }).port}`)
    t.after(() => new Promise<void>(resolve => {
      s.closeAllConnections()
      s.close(() => resolve())
    }))
  }
  const dir = mkdtempSync(join(tmpdir(), 'seed-cloudery-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const tokenFile = join(dir, 'token.json')
  writeFileSync(tokenFile, JSON.stringify({ id: 'partner-id', public: 'public-secret', manager_url: origins[0] }))
  const manifest = join(dir, 'manifest.json')

  async function run(command: string, env: Record<string, string> = {}) {
    const inherited = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('SEED_')))
    const child = spawn(process.execPath, ['--import', 'tsx', script, command], {
      env: {
        ...inherited, SEED_CLOUDERY_TOKEN_FILE: tokenFile, SEED_CLOUDERY_DOMAIN: domain,
        SEED_CLOUDERY_PREFIX: 'seedtest', SEED_CLOUDERY_POLL_MS: '1', SEED_CLOUDERY_MANIFEST: manifest,
        SEED_STACK_URL: origins[0], SEED_ADMIN_URL: origins[1], SEED_USERS: '3', ...env,
      },
      timeout: 15_000,
    })
    let output = ''
    child.stdout.on('data', chunk => { output += chunk })
    child.stderr.on('data', chunk => { output += chunk })
    const code = await new Promise<number | null>(resolve => child.on('close', resolve))
    return { code, output }
  }
  return { state, run, manifest: () => JSON.parse(readFileSync(manifest, 'utf8')) }
}

test('seeds and reuses the organization with per-instance tokens only', async t => {
  const f = await fixture(t)
  const first = await f.run('seed')
  equal(first.code, 0, first.output)
  deepEqual(f.state.created, [org, member(1), member(2), member(3)])
  deepEqual(f.state.emails, ['seedtest', 'seedtest01', 'seedtest02', 'seedtest03'].map(slug => `${slug}@spam.cozycloud.cc`))
  deepEqual([...f.state.orgs], ['seedtest'])
  deepEqual(f.manifest().instances.map((i: any) => [i.fqdn, i.role]),
    [[org, 'organization'], [member(1), 'member'], [member(2), 'member'], [member(3), 'member']])
  match(first.output, /Smoke passed on seedtest01/)
  // The fake rejects any Stack request whose token was minted for another instance.
  deepEqual([...new Set(f.state.mints)].sort(), [member(1), member(2), member(3), org].sort())
  equal(f.state.mints.length, 4)

  const second = await f.run('seed', { SEED_USERS: '4' })
  equal(second.code, 0, second.output)
  deepEqual(f.state.created.slice(4), [member(4)])
  equal(f.state.adminCalls, 0)
})

test('tokens are minted again once they reach the maximum age', async t => {
  const f = await fixture(t)
  const result = await f.run('seed', { SEED_USERS: '1', SEED_CLOUDERY_TOKEN_MAX_AGE_MS: '0' })
  equal(result.code, 0, result.output)
  ok(f.state.mints.filter(host => host === member(1)).length > 3)
})

test('a failed creation workflow fails the seed', async t => {
  const f = await fixture(t)
  f.state.failWorkflow = member(2)
  const result = await f.run('seed')
  equal(result.code, 2)
  match(result.output, /seedtest02\.dev\.example\.test: workflow wf-\S+ failed/)
})

test('a fixture name the Cloudery already reserved fails instead of being adopted', async t => {
  const f = await fixture(t)
  f.state.used.add(member(1))
  const result = await f.run('seed')
  equal(result.code, 2)
  match(result.output, /POST \/api\/v1\/instances: HTTP 409 seedtest01\.dev\.example\.test is already taken/)
})

test('an instance with a fixture name but another organization is not adopted', async t => {
  const f = await fixture(t)
  f.state.instances.set(member(1), { _id: randomUUID(), fqdn: member(1) })
  f.state.foreign = member(1)
  const result = await f.run('seed')
  equal(result.code, 2)
  match(result.output, /seedtest01\.dev\.example\.test is not the Cloudery instance of organization seedtest/)
  ok(!f.state.created.includes(member(1)))
})

test('a member whose context cannot auto-accept the organization\'s drives fails the seed', async t => {
  const f = await fixture(t)
  f.state.noAutoAccept = member(2)
  const result = await f.run('seed')
  equal(result.code, 2)
  match(result.output, /seedtest02\.dev\.example\.test cannot auto-accept the organization's drives/)
})

test('only a smoke that reads back different content is a correctness failure', async t => {
  const f = await fixture(t)
  f.state.corrupt = true
  const result = await f.run('seed')
  equal(result.code, 1)
  match(result.output, /seedtest01\.dev\.example\.test: the 1 KiB smoke read back different content/)
})

test('teardown deletes only fixture instances and releases their names', async t => {
  const f = await fixture(t)
  equal((await f.run('seed')).code, 0)
  const result = await f.run('teardown')
  equal(result.code, 0, result.output)
  deepEqual([...f.state.instances.keys()], unrelated)
  deepEqual([...f.state.used], [])
  match(result.output, /Deleted seedtest03[^]*Deleted seedtest\.dev[^]*Organization seedtest kept/)
  const again = await f.run('seed')
  equal(again.code, 0, again.output)
  equal(f.state.adminCalls, 0)
})

test('another organization\'s instance with a fixture name is never recorded, deleted or released', async t => {
  const f = await fixture(t)
  f.state.instances.set(member(9), { _id: randomUUID(), fqdn: member(9) })
  f.state.foreign = member(9)
  equal((await f.run('seed')).code, 0)
  ok(!f.manifest().instances.some((i: any) => i.fqdn === member(9)))
  // Any member count: names are released from verified identities, never from the naming scheme.
  const result = await f.run('teardown', { SEED_USERS: '9' })
  equal(result.code, 2, result.output)
  match(result.output, /Kept instances not verified as organization seedtest: seedtest09\.dev\.example\.test/)
  deepEqual([...f.state.instances.keys()], [...unrelated, member(9)])
  deepEqual(f.state.released.sort(), [org, member(1), member(2), member(3)].sort())
})
