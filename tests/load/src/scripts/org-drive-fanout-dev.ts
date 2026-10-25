// Shared DEV for the group fan-out (org-drive-fanout.ts, LOAD_TARGET=dev): the organization and
// members of the Cloudery fixture (SEEDING.md), tokens minted through the Cloudery, and the cleanup
// of a run's own drives that a long-lived organization needs.
import { readFileSync } from 'node:fs'
import { performance } from 'node:perf_hooks'

import { request, type Reply } from './fixture-http.ts'

// Every drive a fan-out run creates starts with this, then the run's execution ID.
export const OWNED = 'twake-load-fanout-'
// Instance tokens last 30 minutes: no level starts with a token older than this.
export const TOKEN_REFRESH_MS = 10 * 60_000
const OWNER_SCOPE = 'io.cozy.files io.cozy.sharings io.cozy.contacts io.cozy.contacts.groups io.cozy.settings'
const MEMBER_SCOPE = 'io.cozy.files io.cozy.sharings io.cozy.settings'

export class FanoutSetupError extends Error {}
function check(value: unknown, message: string): asserts value {
  if (!value) throw new FanoutSetupError(message)
}
function parse(text: string): any {
  try { return JSON.parse(text) } catch { return null }
}
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))

export interface DevFixture {
  root: string
  // The organization first, then the members the levels need, by name.
  actors: Array<{ domain: string, uuid: string, origin: URL }>
  warmup: string
  mint(domain: string, owner: boolean): Promise<string>
}
// Requests as the organization instance, bounded by the caller's deadline.
export type Api = (method: string, path: string) => Promise<Reply>

export function readFixture(env: Record<string, string | undefined>, recipients: number): DevFixture {
  const read = (name: string) => {
    const path = env[name]
    check(path, `${name} is required`)
    const value = (() => { try { return parse(readFileSync(path, 'utf8')) } catch { return null } })()
    check(value && typeof value === 'object', `${name} is not readable JSON`)
    return value
  }
  const manifest = read('SEED_CLOUDERY_MANIFEST')
  const secrets = read('SEED_CLOUDERY_TOKEN_FILE')
  check(secrets.public && secrets.manager_url, 'The token file needs public and manager_url')
  const instances: Array<{ fqdn?: string, uuid?: string, role?: string }> =
    Array.isArray(manifest.instances) ? manifest.instances : []
  const org = instances.filter(i => i.role === 'organization')
  const members = instances.filter(i => i.role === 'member').sort((a, b) => String(a.fqdn).localeCompare(String(b.fqdn)))
  check(org.length === 1 && [...org, ...members].every(i => i.fqdn && i.uuid),
    'The manifest needs one organization and identified members')
  check(members.length >= recipients, `FANOUT_LEVELS needs ${recipients} members; the fixture has ${members.length}`)
  check(typeof manifest.warmup_sharing_id === 'string' && manifest.warmup_sharing_id, 'The manifest has no warm-up drive')
  return {
    root: org[0]!.fqdn!,
    // Only the members the levels need. DEV_STACK_URL is a connection origin for tests; each
    // instance stays the Host.
    actors: [org[0]!, ...members.slice(0, recipients)].map(i => ({
      domain: i.fqdn!, uuid: i.uuid!, origin: new URL(env.DEV_STACK_URL || `https://${i.fqdn}`),
    })),
    warmup: manifest.warmup_sharing_id,
    async mint(domain, owner) {
      const reply = await request(new URL(`/api/public/instances/${domain}/token`, secrets.manager_url), 'POST',
        { Authorization: `Bearer ${secrets.public}`, 'Content-Type': 'application/json', Accept: 'application/json' },
        60_000, JSON.stringify({ audience: 'cli', scope: owner ? OWNER_SCOPE : MEMBER_SCOPE }))
      check(reply.status === 200, `Cloudery token for ${domain}: HTTP ${reply.status}`)
      return parse(reply.body)?.token
    },
  }
}

// Waits until the drive's roster stops changing, so no auto-accept is cut short, then trashes its
// root, which revokes the drive everywhere, and destroys it. False when that cannot be shown.
export async function removeDrive(api: Api, sid: string, root: string, stableMs: number, pollMs: number,
  deadline: number): Promise<boolean> {
  let roster: string | null = null
  let since = performance.now()
  for (;;) {
    const reply = await api('GET', `/sharings/${sid}`)
    if (reply.status === 404) break
    if (reply.status !== 200) return false
    const statuses = JSON.stringify((parse(reply.body)?.data?.attributes?.members ?? []).map((m: any) => m?.status))
    if (statuses !== roster) {
      roster = statuses
      since = performance.now()
    } else if (performance.now() - since >= stableMs) break
    if (performance.now() > deadline) return false
    await sleep(pollMs)
  }
  const trashed = await api('DELETE', `/files/${root}`)
  if (![200, 404].includes(trashed.status)) return false
  if (![204, 404].includes((await api('DELETE', `/files/trash/${root}`)).status)) return false
  return (await api('GET', `/files/${root}`)).status === 404
}

export async function removeDoc(api: Api, doctype: string, id: string): Promise<boolean> {
  const reply = await api('GET', `/data/${doctype}/${encodeURIComponent(id)}`)
  if (reply.status === 404) return true
  const rev = parse(reply.body)?._rev
  return reply.status === 200 && typeof rev === 'string' &&
    (await api('DELETE', `/data/${doctype}/${encodeURIComponent(id)}?rev=${encodeURIComponent(rev)}`)).status === 200
}

// Fan-out drives that an earlier, killed run left on the organization instance.
export async function leftoverDrives(api: Api): Promise<Array<{ sid: string, root: string, name: string }>> {
  const reply = await api('GET', '/sharings/drives')
  check(reply.status === 200, `Listing the organization's drives: HTTP ${reply.status}`)
  const drives: any[] = parse(reply.body)?.data ?? []
  return drives.filter(d => d?.attributes?.owner === true && String(d.attributes.description ?? '').startsWith(OWNED))
    .map(d => ({ sid: String(d.id), root: String(d.attributes.rules?.[0]?.values?.[0]), name: d.attributes.description }))
}
