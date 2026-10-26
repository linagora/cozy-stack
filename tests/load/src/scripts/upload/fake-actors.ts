// A fake admin API and Stack for shared-drive actors (drive-actors.test.ts, shared-drive-campaign.test.ts):
// the organization's instances by Host, the owner's drive, its groups and contacts, and what each member
// may do there.
import { randomUUID } from 'node:crypto'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { createServer } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { TestContext } from 'node:test'

const domain = (hex: string) => `seed-${hex.repeat(32)}.localhost:8080`
export const [org, writer, reader] = [domain('a'), domain('b'), domain('c')]

export async function fakeActors(t: TestContext) {
  const uuids = new Map([org, writer, reader].map(d => [d, randomUUID()]))
  const state = {
    uuids, replaced: '', readOnlyWrites: false, keepRevoked: false, neverReady: false, removalStatus: 204,
    // A drive creation that commits but answers with an empty body.
    emptyDriveReply: false,
    // After the writer's group is removed: its reads answer 403 once, then 200; its writes answer this.
    flapAfterRevoke: false, revokedWriteStatus: 403, revokedReads: 0,
    groups: [] as Array<{ id: string, readOnly: boolean, members: string[], revoked: boolean, granted?: boolean }>,
    contacts: new Map<string, string>(), docs: new Set<string>(), root: 'live', requests: [] as string[],
  }
  const grant = (member: string) => state.groups.find(g => g.members.includes(member) && g.granted && !g.revoked)
  const server = createServer(async (req, res) => {
    let body = ''
    for await (const chunk of req) body += chunk
    const url = new URL(req.url!, 'http://x')
    const route = `${req.method} ${url.pathname}`
    const send = (status: number, value?: unknown) => res.writeHead(status).end(value === undefined ? '' : JSON.stringify(value))
    if (req.headers.authorization?.startsWith('Basic ')) {
      if (route === 'POST /instances/token') return res.end(`token-${url.searchParams.get('Domain')}`)
      const name = url.pathname.slice('/instances/'.length)
      const uuid = name === state.replaced ? 'replaced' : uuids.get(name)
      return send(uuid ? 200 : 404, { data: { attributes: { uuid, org_id: org.split('.')[0] } } })
    }
    const host = req.headers.host!
    if (req.headers.authorization !== `Bearer token-${host}`) return send(401)
    state.requests.push(`${host.slice(5, 6)} ${route}`)
    if (route === 'POST /sharings/drives') {
      if (state.emptyDriveReply) return send(201)
      return send(201, { data: { id: 'sid', attributes: { org_drive: true, rules: [{ values: ['root'] }],
        members: [{ instance: `http://${org}`, status: 'owner' }] } } })
    }
    if (route === 'POST /data/io.cozy.contacts.groups/') {
      const id = `group-${state.groups.length}`
      state.groups.push({ id, readOnly: false, members: [], revoked: false })
      state.docs.add(id)
      return send(201, { id })
    }
    if (route === 'POST /data/io.cozy.contacts/') {
      const contact = JSON.parse(body)
      const id = `contact-${state.contacts.size}`
      state.contacts.set(id, new URL(contact.cozy[0].url).host)
      state.docs.add(id)
      state.groups.find(g => g.id === contact.relationships.groups.data[0]._id)!.members.push(new URL(contact.cozy[0].url).host)
      return send(201, { id })
    }
    if (route === 'POST /sharings/sid/recipients') {
      const relationships = JSON.parse(body).data.relationships
      for (const relation of Object.keys(relationships)) {
        const group = state.groups.find(g => g.id === relationships[relation].data[0].id)!
        Object.assign(group, { readOnly: relation === 'read_only_recipients', granted: true })
      }
      return send(200, {})
    }
    if (route === 'GET /sharings/sid') {
      const members = state.groups.filter(g => g.members.length && g.granted)
        .map(g => ({ status: state.neverReady ? 'pending' : g.revoked ? 'revoked' : 'ready' }))
      return send(200, { data: { attributes: { groups: state.groups.map(g => ({ id: g.id, revoked: g.revoked })),
        members: [{ status: 'owner' }, ...members] } } })
    }
    const index = url.pathname.match(/^\/sharings\/sid\/groups\/(\d+)$/)?.[1]
    if (req.method === 'DELETE' && index) {
      if (state.removalStatus !== 204) return send(state.removalStatus)
      state.groups[Number(index)]!.revoked = !state.keepRevoked
      return send(204)
    }
    if (url.pathname === '/sharings/drives/sid/root') {
      const g = grant(host)
      const revoked = !g && state.groups.some(group => group.members.includes(host) && group.revoked)
      if (req.method === 'GET') {
        if (revoked && state.flapAfterRevoke) return send(state.revokedReads++ === 0 ? 403 : 200, {})
        return send(g ? 200 : 403, {})
      }
      if (revoked) return send(state.revokedWriteStatus, {})
      return send(g && (!g.readOnly || state.readOnlyWrites) ? 201 : 403, {})
    }
    if (route === 'GET /files/trash') return send(200, { data: [] })
    if (url.pathname === '/files/root') {
      if (req.method === 'DELETE') state.root = 'trashed'
      return send(req.method === 'DELETE' ? 200 : state.root === 'gone' ? 404 : 200, {})
    }
    if (route === 'DELETE /files/trash/root') {
      state.root = 'gone'
      return send(204)
    }
    const doc = url.pathname.match(/^\/data\/io\.cozy\.contacts(?:\.groups)?\/(.+)$/)?.[1]
    if (doc && req.method === 'GET') return send(state.docs.has(doc) ? 200 : 404, { _rev: '1-a' })
    if (doc && req.method === 'DELETE') return send(state.docs.delete(doc) ? 200 : 404, {})
    send(500)
  })
  await new Promise<void>(resolve => server.listen(0, '127.0.0.1', resolve))
  t.after(() => new Promise<void>(resolve => {
    server.closeAllConnections()
    server.close(() => resolve())
  }))
  const origin = `http://127.0.0.1:${(server.address() as { port: number }).port}`
  const dir = mkdtempSync(join(tmpdir(), 'drive-actors-'))
  t.after(() => rmSync(dir, { recursive: true, force: true }))
  const manifest = join(dir, 'manifest.json')
  writeFileSync(manifest, JSON.stringify({
    version: 1, profile: 'flat', status: 'complete', domain: org, organization: org.split('.')[0],
    parameters: { users: 2, files: 0, drives: 0, context: null }, content: { bytes: 1024, md5: 'x' },
    instances: [org, writer, reader].map((d, i) => ({
      domain: d, uuid: uuids.get(d), role: i ? 'member' : 'organization', created: true, files: [], drives: [] })),
  }))
  const env = { ACTORS_DOMAIN: org, SEED_MANIFEST: manifest, SEED_ADMIN_URL: origin, SEED_STACK_URL: origin,
    SEED_ADMIN_PASSWORD: 'cozy', ACTORS_SETTLE_MS: '50' }
  return { state, env, manifest, dir }
}
