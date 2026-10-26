import { spawn } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { closeSync, mkdirSync, openSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'
import { performance } from 'node:perf_hooks'
import { fileURLToPath } from 'node:url'
import { parseDurationToMs } from '../lib/duration.ts'
import { FixtureDeadline, FixtureError, request, type Reply } from './fixture-http.ts'
import {
  FanoutSetupError, leftoverDrives, OWNED, readFixture, removeDoc, removeDrive, TOKEN_REFRESH_MS,
} from './org-drive-fanout-dev.ts'

class FanoutError extends Error {}
class Deadline extends Error {}
class RuntimeBudget extends Error {}
// Served bytes that differ from the marker: corruption, never a slow convergence.
class ContentMismatch extends Error {}
function check(value: unknown, message: string): asserts value {
  if (!value) throw new FanoutError(message)
}
// An interruption ends the measurement at its next request; shared DEV cleanup and the report follow.
let interrupted: NodeJS.Signals | null = null
for (const signal of ['SIGINT', 'SIGTERM'] as const) process.on(signal, () => { interrupted ??= signal })
const canary = 'cozy-org-fanout-canary\n'
const jsonType = 'application/vnd.api+json'
const now = () => performance.now()
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))
function integer(name: string, fallback: string, max = 2_147_483_647): number {
  const raw = process.env[name] ?? fallback
  const n = Number(raw)
  check(/^[1-9]\d*$/.test(raw) && Number.isSafeInteger(n) && n <= max, `Invalid ${name}`)
  return n
}
function origin(raw: string | undefined, name: string): URL {
  try {
    check(raw && /^https?:\/\/[^/?#@\\\s]+\/?$/.test(raw), '')
    const url = new URL(raw)
    check(!url.username && !url.password && !url.hash && !url.search && url.pathname === '/', '')
    return url
  } catch {
    throw new FanoutError(`Invalid ${name} origin`)
  }
}
function domain(value: string): void {
  const [host = '', port, ...rest] = value.split(':')
  const validHost = /^seed-[a-f0-9]{32}\./.test(host) && host.length <= 253 &&
    host.split('.').every(label => /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(label))
  const validPort = port === undefined || (/^[1-9]\d{0,4}$/.test(port) && Number(port) <= 65535)
  check(validHost && !rest.length && validPort, 'Invalid seed organization domain')
}
function id(value: unknown): string {
  const validID = typeof value === 'string' && /^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,255}$/.test(value)
  check(validID, 'Invalid fixture identifier')
  return value
}
function decode<T>(reply: Reply): T {
  try {
    return JSON.parse(reply.body) as T
  } catch {
    throw new FanoutError('Invalid API JSON')
  }
}
interface Member {
  instance?: string, status?: string, read_only?: boolean, only_in_groups?: boolean, groups?: number[]
}
interface Group { id?: string, revoked?: boolean, read_only?: boolean, addedBy?: number }
interface Resource {
  id: string
  attributes: {
    domain?: string, uuid?: string, org_id?: string, org_domain?: string, context?: string,
    org_drive?: boolean, owner?: boolean, active?: boolean, members?: Member[], groups?: Group[],
    rules?: Array<{ values?: string[] }>, common_contacts?: boolean, max_members_per_sharing?: number,
    sharing?: { auto_accept_trusted?: boolean, auto_accept_trusted_contacts?: boolean }
  }
}
// origin is where to connect; the domain stays the Host.
interface Actor { domain: string, uuid: string, token: string, origin: URL, mintedAt: number }
interface Proof {
  converged: boolean, observed_elapsed_ms: number, sharing_status: number,
  own_status: string | null, active: boolean | null,
  root_status: number | null, marker_status: number | null, marker_matches: boolean | null
}
interface Phase {
  status: string, elapsed_ms: number | null, converged_ms: number | null, mutation: string, http_status: number | null
}
interface OwnerProof {
  converged: boolean, active: boolean, org_drive: boolean, ready_recipients: number,
  revoked_recipients: number, observed_elapsed_ms: number
}
interface Point {
  n: number, status: string, phase: string, mutation_concurrency: number, observer_concurrency: number,
  fixtures: {
    sharing_id?: string, root_id?: string, group_id?: string,
    file_id?: string, marker_bytes?: number, contact_ids: string[]
  },
  add: Phase, remove: Phase, recipients: Array<{ domain: string, add: Proof | null, remove: Proof | null }>,
  owner?: { domain: string, add?: OwnerProof, remove?: OwnerProof },
  retained_autoaccept_work_may_continue: boolean, error?: string
}
function resource(reply: Reply): Resource {
  const data = decode<{ data?: Resource }>(reply)?.data
  const validResource = data && typeof data === 'object' &&
    data.attributes && typeof data.attributes === 'object'
  check(validResource, 'Invalid API resource')
  id(data.id)
  return data
}
async function bounded(tasks: Array<() => Promise<void>>, concurrency: number): Promise<void> {
  let next = 0
  let failure: unknown
  await Promise.all(Array.from({ length: Math.min(tasks.length, concurrency) }, async () => {
    while (!failure && next < tasks.length) {
      const task = tasks[next++]!
      try {
        await task()
      } catch (error) {
        // Observer uncertainty takes precedence over a healthy SLA miss, regardless of completion order.
        if (!failure || failure instanceof Deadline || error instanceof ContentMismatch) failure = error
      }
    }
  }))
  if (failure) throw failure
}

async function main(): Promise<void> {
  const report: { status: string, organization_domain?: string, mode?: string, settings?: object,
    points: Point[], capacity: { kind: string, lower: number | null, upper_exclusive: number | null, max: null },
    retained: boolean, warning: string, error?: string, content_mismatch: boolean, budget_reached?: string,
    load_target?: 'local' | 'dev', outcome?: number,
    recovery?: { cleaned: string[] }, cleanup?: { cleaned: string[], retained: string[] } } = {
    status: 'inconclusive', points: [], content_mismatch: false,
    capacity: { kind: 'inconclusive', lower: null, upper_exclusive: null, max: null },
    retained: true, warning: 'No automatic cleanup. Client completion/timeout does not prove server writes ended.',
  }
  let reportFD: number | undefined
  // Shared DEV removes the run's own fixtures before the report is written.
  let cleanupRun: (() => Promise<void>) | undefined
  let workDeadline = Infinity
  const remainingWork = (): number => {
    const remaining = workDeadline - now()
    if (remaining <= 0) throw new RuntimeBudget('MAX_RUNTIME exhausted')
    return remaining
  }
  let measuring = true
  const secrets = new Set<string>()
  const redact = (text: string) => {
    for (const secret of secrets) text = text.replaceAll(secret, '[redacted]')
    return text
  }
  const render = () => JSON.stringify(report, (_key, value: unknown) =>
    typeof value === 'string' ? redact(value) : value, 2) + '\n'
  try {
    check(process.argv.length === 2, 'Configuration belongs in environment variables, not arguments')
    const levelsRaw = process.env.FANOUT_LEVELS ?? '1,2,4,8,16'
    const levels = levelsRaw.split(',').map(Number)
    const validLevels = /^[1-9]\d*(,[1-9]\d*)*$/.test(levelsRaw) && levels.every((n, i) =>
      Number.isSafeInteger(n) && n <= 1000 && (i === 0 || n > levels[i - 1]!))
    check(validLevels, 'Invalid FANOUT_LEVELS')
    const maxN = levels.at(-1)!
    const timeout = integer('FANOUT_TIMEOUT_MS', '60000')
    const poll = integer('FANOUT_POLL_MS', '500')
    const concurrency = integer('FANOUT_POLL_CONCURRENCY', '8', 1000)
    const stable = integer('FANOUT_STABLE_MS', '3000')
    check(stable < timeout, 'FANOUT_STABLE_MS must be less than FANOUT_TIMEOUT_MS')
    const dev = process.env.LOAD_TARGET === 'dev'
    report.load_target = dev ? 'dev' : 'local'
    // A level starts only when both of its phases can still end within MAX_RUNTIME.
    const levelBudget = 2 * timeout + stable + 60_000
    const runtime = process.env.MAX_RUNTIME
    let runtimeMs = Infinity
    if (runtime) {
      try { runtimeMs = parseDurationToMs(runtime) } catch { throw new FanoutError('Invalid MAX_RUNTIME') }
    }
    check(!dev || runtime, 'Shared DEV needs MAX_RUNTIME agreed with the DEV owner')
    check(runtimeMs >= levelBudget, 'MAX_RUNTIME leaves no time for one level')
    workDeadline = now() + runtimeMs
    const runID = id(process.env.EXECUTION_ID ?? 'fanout')
    report.settings = {
      levels, timeout_ms: timeout, poll_ms: poll, stable_ms: stable,
      mutation_concurrency: 1, observer_concurrency: concurrency,
      group_contact_cap: 1000, max_runtime: runtime ?? null,
    }
    // Opened once the configuration is valid, before any request.
    const openReport = () => {
      const path = process.env.FANOUT_REPORT
      if (path === undefined) return
      check(path.length > 0 && !/[\x00-\x1f\x7f]/.test(path), 'Invalid FANOUT_REPORT')
      try {
        mkdirSync(dirname(path), { recursive: true, mode: 0o700 })
        reportFD = openSync(path, 'wx', 0o600)
      } catch {
        throw new FanoutError('FANOUT_REPORT must be a new private writable file')
      }
    }
    const fixture = dev ? readFixture(process.env, maxN) : null
    let root: string
    let actors: Actor[]
    let contextName: string | null = null
    let mint: (actor: Actor) => Promise<string>
    const expect = (reply: Reply, status = 200) => {
      check(reply.status === status, `Unexpected API status: HTTP ${reply.status}`)
      return reply
    }
    if (fixture) {
      root = fixture.root
      report.organization_domain = root
      report.mode = 'cloudery'
      openReport()
      console.error(`Organization fixture: ${root}`)
      actors = fixture.actors.map(a => ({ ...a, token: '', mintedAt: 0 }))
      mint = actor => fixture.mint(actor.domain, actor === actors[0], Math.min(60_000, remainingWork()))
    } else {
      const adminURL = origin(process.env.SEED_ADMIN_URL, 'SEED_ADMIN_URL')
      const stackURL = origin(process.env.SEED_STACK_URL, 'SEED_STACK_URL')
      const username = process.env.SEED_ADMIN_USERNAME ?? 'admin'
      const password = process.env.SEED_ADMIN_PASSWORD
      check(password && username && !/[:\x00-\x1f\x7f]/.test(username), 'Invalid admin credentials configuration')
      secrets.add(password)
      const context = process.env.SEED_CONTEXT_NAME
      const validContext = context === undefined || (context.length > 0 && !/[\s\x00-\x1f\x7f]/.test(context))
      check(validContext, 'Invalid SEED_CONTEXT_NAME')
      const supplied = process.env.FANOUT_ORG_DOMAIN
      root = supplied ?? `seed-${randomBytes(16).toString('hex')}.${process.env.SEED_DOMAIN_SUFFIX ?? ''}`
      domain(root)
      const orgID = root.split('.')[0]!
      const suffix = root.slice(orgID.length)
      if (process.env.SEED_DOMAIN_SUFFIX !== undefined) {
        check(suffix === `.${process.env.SEED_DOMAIN_SUFFIX}`, 'Organization suffix mismatch')
      }
      report.organization_domain = root
      report.mode = supplied === undefined ? 'fresh' : 'precreated'
      report.settings = { ...report.settings, context: context ?? null }
      openReport()
      console.error(`Organization fixture (retained): ${redact(root)}`)
      const auth = `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}`
      secrets.add(auth)
      secrets.add(auth.slice(6))
      const admin = async (method: string, path: string, params?: Record<string, string>) => {
        const url = new URL(path, adminURL)
        if (params) url.search = new URLSearchParams(params).toString()
        return request(url, method, { Authorization: auth }, Math.min(timeout, remainingWork()), '', 1_048_576)
      }
      if (supplied === undefined) {
        expect(await admin('GET', `/instances/${root}`), 404)
        await new Promise<void>((resolve, reject) => {
          const seeder = fileURLToPath(new URL('./seed-scale-data.ts', import.meta.url))
          // The seeder's domains and failure reason go to stderr; stdout is reserved for the report.
          const child = spawn(process.execPath, ['--import', 'tsx', seeder, 'seed-org', root], {
            env: { ...process.env, SEED_USERS: String(maxN), SEED_FILES: '0', SEED_DRIVES: '0' },
            stdio: ['ignore', 2, 2],
          })
          child.once('error', () => {
            reject(new FanoutError('Organization seeder failed; partial organization retained'))
          })
          child.once('close', code => {
            if (code === 0) resolve()
            else reject(new FanoutError('Organization seeder failed; partial organization retained'))
          })
        })
      }
      // /instances is global: consume every page before selecting the exact organization.
      const registry: Resource[] = []
      const seen = new Set<string>()
      const ids = new Set<string>()
      const domains = new Set<string>()
      const uuids = new Set<string>()
      let next = '/instances?page[limit]=10'
      while (next) {
        let url: URL
        try {
          check(typeof next === 'string' && !/[\s\\]/.test(next), '')
          url = new URL(next, adminURL)
          const validRegistryURL = url.origin === adminURL.origin && url.pathname === '/instances' &&
            !url.username && !url.password && !url.hash
          check(validRegistryURL, '')
          url.searchParams.sort()
          check(!seen.has(url.href), '')
          seen.add(url.href)
        } catch {
          throw new FanoutError('Invalid registry pagination link or loop')
        }
        const page = decode<{ data?: Resource[], links?: { next?: string | null } }>(
          expect(await admin('GET', url.href)))
        check(Array.isArray(page?.data), 'Invalid instance registry')
        for (const item of page.data) {
          const a = item?.attributes
          // Instances created without a UUID are ordinary; organization members are checked below.
          check(typeof item?.id === 'string' && item.id && a && typeof a.domain === 'string' && a.domain &&
            (a.uuid == null || a.uuid === '' || typeof a.uuid === 'string'),
          'Invalid or duplicate registry identity')
          const uniqueIdentity = !ids.has(item.id) && !domains.has(a.domain) && !(a.uuid && uuids.has(a.uuid))
          check(uniqueIdentity, 'Invalid or duplicate registry identity')
          ids.add(item.id)
          domains.add(a.domain)
          if (a.uuid) uuids.add(a.uuid)
          registry.push(item)
        }
        const validLinks = page.links == null || (typeof page.links === 'object' && !Array.isArray(page.links))
        check(validLinks, 'Invalid registry pagination')
        const validNext = page.links?.next == null ||
          (typeof page.links.next === 'string' && page.links.next.length > 0)
        check(validNext, 'Invalid registry pagination')
        next = page.links?.next ?? ''
      }
      const isOrganizationMember = (r: Resource) => r.attributes.org_id === orgID ||
        r.attributes.org_domain === `${orgID}.seed.invalid` || r.attributes.domain === root
      const cohort = registry.filter(isOrganizationMember)
      const completeMembership = cohort.filter(r => r.attributes.domain === root).length === 1 &&
        cohort.length > maxN
      check(completeMembership, 'Incomplete organization registry membership')
      for (const item of cohort) {
        const a = item.attributes
        domain(a.domain!)
        const validOrganizationIdentity = a.org_id === orgID && a.org_domain === `${orgID}.seed.invalid` &&
          a.domain!.slice(a.domain!.indexOf('.')) === suffix &&
          /^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/.test(a.uuid!) &&
          (a.context === undefined || (typeof a.context === 'string' && !/[\s\x00-\x1f\x7f]/.test(a.context))) &&
          (context === undefined || a.context === context)
        check(validOrganizationIdentity, 'Organization scope, context or UUID identity mismatch')
      }
      check(new Set(cohort.map(r => r.attributes.context ?? '')).size === 1, 'Organization context mismatch')
      actors = cohort.sort((a, b) => {
        if (a.attributes.domain === root) return -1
        if (b.attributes.domain === root) return 1
        return a.attributes.domain!.localeCompare(b.attributes.domain!)
      }).map(r => ({ domain: r.attributes.domain!, uuid: r.attributes.uuid!, token: '', origin: stackURL, mintedAt: 0 }))
      contextName = cohort[0]!.attributes.context ?? null
      mint = async actor => expect(await admin('POST', '/instances/token', {
        Domain: actor.domain, Audience: 'cli',
        Scope: actor === actors[0] ? 'io.cozy.files io.cozy.sharings io.cozy.contacts io.cozy.contacts.groups'
          : 'io.cozy.files io.cozy.sharings',
      })).body
    }
    const owner = actors[0]!
    const api = async (
      actor: Actor, method: string, path: string, body = '', deadline = now() + timeout, contentType = jsonType,
    ) => {
      remainingWork()
      const end = Math.min(deadline, workDeadline)
      const remaining = end - now()
      if (interrupted && measuring) throw new FanoutError(`interrupted by ${interrupted}`)
      if (remaining <= 0) throw new Deadline()
      let reply: Reply
      try {
        reply = await request(new URL(path, actor.origin), method, {
          Host: actor.domain, Authorization: `Bearer ${actor.token}`,
          Accept: jsonType, 'Content-Type': contentType }, Math.max(1, Math.floor(remaining)), body, 1_048_576)
      } catch (error) {
        if (error instanceof FixtureDeadline && end === workDeadline) throw new RuntimeBudget('MAX_RUNTIME exhausted')
        throw error
      }
      const observed = now()
      if (observed >= workDeadline) throw new RuntimeBudget('MAX_RUNTIME exhausted')
      if (observed > deadline) throw new Deadline()
      return reply
    }
    const refresh = async (actor: Actor) => {
      remainingWork()
      actor.token = await mint(actor)
      remainingWork()
      check(actor.token && !/[\s\x00-\x1f\x7f]/.test(actor.token), 'Invalid CLI token response')
      secrets.add(actor.token)
      actor.mintedAt = Date.now()
    }
    let memberCap = 90
    for (const actor of actors) {
      await refresh(actor)
      const settings = resource(expect(await api(actor, 'GET', '/settings/context'))).attributes
      if (fixture) {
        const identity = resource(expect(await api(actor, 'GET', '/settings/instance'))).attributes
        check(identity.uuid === actor.uuid, `${actor.domain} is no longer the fixture instance in the manifest`)
      }
      // Recipients auto-accept because they share the owner's organization ID; on shared DEV the
      // organization instance's own context does not enable it.
      if (!fixture || actor !== owner) {
        const trustedCommonContacts = settings.common_contacts === true &&
          settings.sharing?.auto_accept_trusted === true
        check(trustedCommonContacts, `${actor.domain} context must enable common_contacts and sharing.auto_accept_trusted`)
      }
      if (actor === owner) {
        memberCap = settings.max_members_per_sharing ?? 90
        const validMemberCap = Number.isSafeInteger(memberCap) && memberCap > 0
        check(validMemberCap, 'Malformed max_members_per_sharing context setting')
        report.settings = { ...report.settings, effective_member_cap: memberCap }
        check(memberCap >= maxN + 1, 'Configured max_members_per_sharing is below requested recipients plus owner')
      }
    }
    report.settings = {
      ...report.settings, effective_member_cap: memberCap, context: contextName,
      common_contacts: true, auto_accept_trusted: true,
    }
    const asOwner = (deadline: number) => (method: string, path: string) => api(owner, method, path, '', deadline)
    if (fixture) {
      // Under the shared DEV lock, drives an earlier run left are this run's to remove first.
      const deadline = now() + 300_000
      const cleaned: string[] = []
      for (const drive of await leftoverDrives(asOwner(deadline))) {
        check(await removeDrive(asOwner(deadline), drive.sid, drive.root, stable, poll, deadline),
          `Earlier fan-out drive ${drive.name} remains`)
        cleaned.push(drive.name)
      }
      report.recovery = { cleaned }
      cleanupRun = async () => {
        const deadline = now() + 300_000
        const cleaned: string[] = []
        const retained: string[] = []
        for (const { fixtures: f, n, phase } of report.points) {
          const label = f.sharing_id ?? `level ${n}`
          try {
            let removed = !!f.sharing_id && !!f.root_id &&
              await removeDrive(asOwner(deadline), f.sharing_id, f.root_id, stable, poll, deadline)
            for (const [doctype, docID] of [...(f.group_id ? [['io.cozy.contacts.groups', f.group_id]] : []),
              ...f.contact_ids.map(c => ['io.cozy.contacts', c])]) {
              removed = removed && await removeDoc(asOwner(deadline), doctype!, docID!)
            }
            // A setup write may have committed without returning its new resource ID.
            if (phase === 'setup') removed = false
            ;(removed ? cleaned : retained).push(label)
          } catch {
            retained.push(label)
          }
        }
        report.cleanup = { cleaned, retained }
        report.retained = retained.length > 0
        report.warning = 'This run removes its own drives, groups and contacts once their rosters settle.'
      }
    }
    const canonical = (value: unknown, expected: string): URL => {
      check(typeof value === 'string', 'Invalid sharing member identity')
      const url = origin(value, 'canonical sharing member')
      check(url.host === expected, 'Sharing member identity mismatch')
      return url
    }
    const roster = (data: Resource, sid: string): Member[] => {
      const members = data.attributes.members
      const isKnownMember = (m: Member) => m &&
        ['owner', 'mail-not-sent', 'pending', 'seen', 'ready', 'revoked'].includes(m.status ?? '')
      const validRoster = data.id === sid && Array.isArray(members) &&
        members.length > 0 && members.every(isKnownMember)
      check(validRoster, 'Malformed sharing roster')
      return members
    }
    for (const n of levels) {
      if (fixture) for (const actor of actors) if (Date.now() - actor.mintedAt > TOKEN_REFRESH_MS) await refresh(actor)
      if (now() + levelBudget > workDeadline) {
        report.budget_reached = 'runtime'
        break
      }
      const selected = actors.slice(1, n + 1)
      const p: Point = {
        n, status: 'inconclusive', phase: 'setup', mutation_concurrency: 1, observer_concurrency: concurrency,
        fixtures: { contact_ids: [] },
        add: { status: 'not_started', elapsed_ms: null, converged_ms: null, mutation: 'not_sent', http_status: null },
        remove: {
          status: 'not_started', elapsed_ms: null, converged_ms: null, mutation: 'not_sent', http_status: null,
        },
        recipients: selected.map(a => ({ domain: a.domain, add: null, remove: null })),
        retained_autoaccept_work_may_continue: false,
      }
      report.points.push(p)
      const drive = `${OWNED}${runID}-${n}-${randomBytes(8).toString('hex')}`
      try {
        const created = resource(expect(await api(owner, 'POST', '/sharings/drives', JSON.stringify({ data: {
          type: 'io.cozy.sharings', attributes: { name: drive, description: drive },
        } })), 201))
        const sid = id(created.id)
        const rootID = id(created.attributes.rules?.[0]?.values?.[0])
        p.fixtures.sharing_id = sid
        p.fixtures.root_id = rootID
        const activeOwnedDrive = created.attributes.org_drive === true &&
          created.attributes.owner === true && created.attributes.active === true
        check(activeOwnedDrive, 'Expected active organization-owned drive')
        const scheme = canonical(roster(created, sid)[0]?.instance, root).protocol
        check(created.attributes.members![0]?.status === 'owner', 'Invalid drive owner status')
        const group = decode<{ id?: string }>(expect(await api(
          owner, 'POST', '/data/io.cozy.contacts.groups/', JSON.stringify({ name: `${OWNED}${sid}` }),
          undefined, 'application/json',
        ), 201))
        const groupID = id(group.id)
        p.fixtures.group_id = groupID
        for (const actor of selected) {
          const contact = decode<{ id?: string }>(expect(await api(
            owner, 'POST', '/data/io.cozy.contacts/', JSON.stringify({
              fullname: 'Disposable fanout fixture',
              email: [{ address: `${actor.uuid}@spam.cozycloud.cc`, primary: true }],
              cozy: [{ url: `${scheme}//${actor.domain}/`, primary: true }],
              relationships: { groups: { data: [{ _id: groupID, _type: 'io.cozy.contacts.groups' }] } },
            }), undefined, 'application/json',
          ), 201))
          p.fixtures.contact_ids.push(id(contact.id))
        }
        const fileID = id(resource(expect(await api(
          owner, 'POST', `/files/${rootID}?Type=file&Name=fanout-canary.txt`, canary, undefined, 'text/plain',
        ), 201)).id)
        p.fixtures.file_id = fileID
        p.fixtures.marker_bytes = Buffer.byteLength(canary)
        const sharingPath = `/sharings/${sid}`
        const proxyRoot = `/sharings/drives/${sid}/${rootID}`
        const marker = `/sharings/drives/${sid}/download/${fileID}`
        const ownerCheck = async (removed: boolean, deadline: number): Promise<boolean> => {
          const data = resource(expect(await api(owner, 'GET', sharingPath, '', deadline)))
          const members = roster(data, sid)
          const a = data.attributes
          check(canonical(members[0]?.instance, root).protocol === scheme, 'Sharing owner scheme mismatch')
          const groups = a.groups ?? []
          check(Array.isArray(groups), 'Malformed sharing groups')
          const indices = groups.flatMap((g, i) => g.id === groupID ? [i] : [])
          const index = indices[0]
          const hasExpectedGrant = (m: Member) => m.status === (removed ? 'revoked' : 'ready') &&
            m.only_in_groups === true && !m.read_only &&
            (removed || (Array.isArray(m.groups) && m.groups.length === 1 && m.groups[0] === index))
          const expectedOwner = a.org_drive === true && a.owner === true && a.active === true &&
            members[0]?.status === 'owner'
          // JSON false and omitted false are equivalent in Stack's omitempty fields.
          const hasExpectedGroup = () => indices.length === 1 &&
            Boolean(groups[index!]?.revoked) === removed && groups[index!]?.read_only === false &&
            groups[index!]?.addedBy === 0
          let good = expectedOwner && hasExpectedGroup() && members.length === n + 1
          const found = new Set<string>()
          for (const m of members.slice(1)) {
            const url = origin(m.instance, 'sharing recipient')
            const actor = selected.find(a => a.domain === url.host)
            const expectedRecipient = actor && url.protocol === scheme && !found.has(url.host)
            check(expectedRecipient, 'Unexpected or duplicate sharing recipient identity')
            found.add(url.host)
            good = good && hasExpectedGrant(m)
          }
          if (removed) {
            expect(await api(owner, 'GET', proxyRoot, '', deadline))
            const bytes = expect(await api(owner, 'GET', marker, '', deadline))
            if (bytes.body !== canary) throw new ContentMismatch('Owner read different marker bytes')
          }
          p.owner ??= { domain: root }
          p.owner[removed ? 'remove' : 'add'] = {
            converged: good, active: a.active === true, org_drive: a.org_drive === true,
            ready_recipients: members.slice(1).filter(m => m.status === 'ready').length,
            revoked_recipients: members.slice(1).filter(m => m.status === 'revoked').length,
            observed_elapsed_ms: now() - (deadline - timeout),
          }
          return good
        }
        const recipientCheck = async (
          actor: Actor, removed: boolean, deadline: number, i: number,
        ): Promise<boolean> => {
          const reply = await api(actor, 'GET', sharingPath, '', deadline)
          const proof: Proof = {
            converged: false, observed_elapsed_ms: now() - (deadline - timeout), sharing_status: reply.status,
            own_status: null, active: null, root_status: null, marker_status: null, marker_matches: null,
          }
          p.recipients[i]![removed ? 'remove' : 'add'] = proof
          let good = removed && reply.status === 404
          if (reply.status !== 404) {
            const data = resource(expect(reply))
            const members = roster(data, sid)
            check(canonical(members[0]?.instance, root).protocol === scheme, 'Sharing owner scheme mismatch')
            const own = members.filter(m => {
              if (!m.instance) return false // Peer snapshots can stay unresolved after our own acceptance.
              const url = origin(m.instance, 'sharing recipient')
              check(url.protocol === scheme, 'Sharing recipient scheme mismatch')
              return url.host === actor.domain
            })
            const awaitingIdentity = own.length === 0 && members.some(m => !m.instance && m.status !== 'ready')
            const recipientSharing = data.attributes.owner !== true && members[0]?.status === 'owner'
            check(recipientSharing, 'Recipient sharing identity mismatch')
            check(own.length === 1 || awaitingIdentity, 'Recipient sharing member identity mismatch')
            if (awaitingIdentity) return false
            proof.own_status = own[0]!.status ?? null
            proof.active = data.attributes.active === true
            good = proof.active === !removed && proof.own_status === (removed ? 'revoked' : 'ready') &&
              !own[0]!.read_only
          } else if (!removed) return false // Invitation not delivered yet.
          const rootReply = await api(actor, 'GET', proxyRoot, '', deadline)
          const fileReply = await api(actor, 'GET', marker, '', deadline)
          proof.root_status = rootReply.status
          proof.marker_status = fileReply.status
          proof.marker_matches = fileReply.status === 200 && fileReply.body === canary
          if (fileReply.status === 200 && !proof.marker_matches) {
            throw new ContentMismatch('Recipient read different marker bytes')
          }
          const denied = (status: number) => [403, 404, 410].includes(status)
          const expectedAccess = [200, 403, 404, 410].includes(rootReply.status) &&
            [200, 403, 404, 410].includes(fileReply.status)
          check(expectedAccess, 'Unexpected recipient access response')
          if (removed) good = good && denied(rootReply.status) && denied(fileReply.status)
          else good = good && rootReply.status === 200 && proof.marker_matches
          proof.converged = good
          proof.observed_elapsed_ms = now() - (deadline - timeout)
          return good
        }
        // Reads get only the time left before the phase deadline: one cut short has missed the
        // SLA exactly like a late reply, rather than failing as an observer.
        const timed = async (read: () => Promise<boolean>): Promise<boolean> => {
          try {
            return await read()
          } catch (error) {
            throw error instanceof FixtureDeadline ? new Deadline() : error
          }
        }
        // Resolves with the end of the first round of the final all-good streak, so callers can
        // report convergence separately from the stability window.
        const observe = async (removed: boolean, start: number): Promise<number> => {
          const deadline = start + timeout
          let stableSince: number | undefined
          while (now() < deadline) {
            let good = true
            await bounded([
              async () => { good = await timed(() => ownerCheck(removed, deadline)) && good },
              ...selected.map((a, i) => async () => {
                good = await timed(() => recipientCheck(a, removed, deadline, i)) && good
              }),
            ], concurrency)
            const observed = now()
            if (observed > deadline) throw new Deadline()
            stableSince = good ? stableSince ?? observed : undefined
            if (stableSince !== undefined && observed - stableSince >= stable) return stableSince
            const remaining = deadline - now()
            // Do not squeeze an extra, almost-certainly timed-out round into a fractional tail.
            if (remaining <= poll) {
              while (now() < deadline) await sleep(Math.max(1, Math.ceil(deadline - now())))
              throw new Deadline()
            }
            await sleep(poll)
          }
          throw new Deadline()
        }
        for (const removed of [false, true]) {
          p.phase = removed ? 'remove' : 'add'
          const phase = removed ? p.remove : p.add
          let index = -1
          if (removed) {
            const data = resource(expect(await api(owner, 'GET', sharingPath)))
            const members = roster(data, sid)
            const unchangedOwner = data.attributes.owner === true && data.attributes.org_drive === true &&
              data.attributes.active === true && canonical(members[0]?.instance, root).protocol === scheme &&
              members[0]?.status === 'owner'
            check(unchangedOwner, 'Owner sharing changed before group removal')
            const indices = (data.attributes.groups ?? [])
              .flatMap((g, i) => g.id === groupID && !g.revoked ? [i] : [])
            check(indices.length === 1, 'Cannot rediscover active group index')
            index = indices[0]!
          }
          const start = now()
          phase.status = 'observing'
          phase.mutation = 'unknown'
          p.retained_autoaccept_work_may_continue = !removed
          try {
            const reply = await api(
              owner, removed ? 'DELETE' : 'POST',
              removed ? `${sharingPath}/groups/${index}` : `${sharingPath}/recipients`,
              removed ? '' : JSON.stringify({ data: { type: 'io.cozy.sharings', id: sid, relationships: {
                recipients: { data: [{ id: groupID, type: 'io.cozy.contacts.groups' }] },
              } } }), start + timeout)
            phase.http_status = reply.status
            phase.mutation = reply.status === (removed ? 204 : 200) ? 'accepted' : 'http_error'
            expect(reply, removed ? 204 : 200)
            phase.converged_ms = await observe(removed, start) - start
            phase.status = 'passed'
            p.retained_autoaccept_work_may_continue = false
          } catch (error) {
            const missedPhaseDeadline = error instanceof Deadline && phase.mutation === 'accepted'
            phase.status = missedPhaseDeadline || error instanceof ContentMismatch ? 'failed' : 'inconclusive'
            throw error
          } finally {
            phase.elapsed_ms = now() - start
          }
        }
        p.status = 'passed'
        p.phase = 'complete'
      } catch (error) {
        const missedAddDeadline = error instanceof Deadline && p.phase === 'add' && p.add.mutation === 'accepted'
        p.status = missedAddDeadline || error instanceof ContentMismatch ? 'failed' : 'inconclusive'
        if (error instanceof Deadline) p.error = 'Observation SLA deadline exceeded'
        else if (error instanceof FanoutError || error instanceof ContentMismatch) p.error = error.message
        else p.error = 'Transport or observer failure; server write completion unknown'
        throw error
      }
    }
    report.status = report.points.length ? 'passed' : 'inconclusive'
  } catch (error) {
    report.content_mismatch = error instanceof ContentMismatch
    if (error instanceof RuntimeBudget || (!report.content_mismatch && now() >= workDeadline)) {
      report.budget_reached = 'runtime'
      report.error = 'MAX_RUNTIME exhausted'
    } else if (error instanceof FanoutError || error instanceof FanoutSetupError || error instanceof ContentMismatch) {
      report.error = error.message
    }
    else if (error instanceof Deadline) report.error = 'Observation SLA deadline exceeded'
    else if (error instanceof FixtureError) report.error = 'Transport failure; server write completion unknown'
    else report.error = 'Fanout execution failed; artifacts retained'
    const last = report.points.at(-1)
    report.status = last?.status === 'failed' ? 'failed' : 'inconclusive'
  } finally {
    const passes = report.points.filter(p => p.status === 'passed')
    const lower = passes.at(-1)?.n ?? null
    const last = report.points.at(-1)
    if (report.status === 'passed' || (report.budget_reached === 'runtime' && lower !== null)) {
      report.capacity = { kind: 'lower_bound', lower, upper_exclusive: null, max: null }
    } else if (last?.status === 'failed' && lower !== null && !report.content_mismatch) {
      report.capacity = { kind: 'bracket', lower, upper_exclusive: last.n, max: null }
    }
    // Cleanup has its own bounded allowance after workload traffic has stopped.
    workDeadline = Infinity
    measuring = false
    if (cleanupRun) await cleanupRun()
    if (report.retained && cleanupRun && !report.content_mismatch) {
      report.status = 'inconclusive'
      report.error = [report.error, `fixtures remain: ${report.cleanup!.retained.join(' ')}`].filter(Boolean).join('; ')
    }
    // Exit contract (README): a bound or bracket is a measurement; a missed first level or
    // a content mismatch at any level is a failure.
    process.exitCode = report.status === 'inconclusive' ? 2
      : report.capacity.kind !== 'inconclusive' ? 0 : report.status === 'failed' ? 1 : 2
    report.outcome = process.exitCode
    let output = render()
    if (reportFD !== undefined) {
      try {
        writeFileSync(reportFD, output)
      } catch {
        report.status = 'inconclusive'
        report.capacity = { kind: 'inconclusive', lower: null, upper_exclusive: null, max: null }
        report.error = 'Private report write failed'
        process.exitCode = 2
        report.outcome = 2
        output = render()
      } finally {
        closeSync(reportFD)
      }
    }
    process.stdout.write(output)
  }
}
void main()
