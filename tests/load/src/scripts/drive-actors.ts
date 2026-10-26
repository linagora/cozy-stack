// Shared-drive actors from an organization fixture (SEEDING.md, Fixture manifest): the organization
// instance owns the drive, its first member writes through a group-only grant and its second reads
// through a read-only grant. Preparation and revocation stay outside the measured workload.
import { spawnSync } from 'node:child_process'

import { FixtureError, request, type Reply } from './fixture-http.ts'
import { manifestPath, readManifest } from './fixture-manifest.ts'
import { removeDoc, removeDrive } from './org-drive-fanout-dev.ts'
import { check, harness, parse, stackClient, type Target } from './upload/exact-work.ts'

type Env = Record<string, string | undefined>
type Role = 'owner' | 'writer' | 'reader'
interface Actor { role: Role, domain: string, uuid: string, token: string }
type Api = (method: string, path: string, body?: string, type?: string) => Promise<Reply>
// A member allowed more than its grant, or refused what it was granted: never a slow convergence.
export class AccessFailure extends Error {}

const OWNER_SCOPE = 'io.cozy.files io.cozy.sharings io.cozy.contacts io.cozy.contacts.groups'
const MEMBER_SCOPE = 'io.cozy.files io.cozy.sharings'
const REQUEST_MS = 60_000
const POLL_MS = 500
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))

export interface ActorsReport {
  fixture: string
  drive: string
  // Where each actor lives: recipients on the owner's Stack take the same-Stack sharing path.
  placement: Array<{ role: Role, domain: string, stack: string, path: 'owner' | 'same-stack' | 'cross-stack' }>
  access: Record<string, number>
  // How long the members took to accept the drive: preparation, never part of the measured ramp.
  accept_ms: number
  revocation?: Record<string, number>
  cleanup?: { cleaned: string[], retained: string[] }
}
export interface DriveActors {
  drive: { id: string, root: string }
  // The organization instance as the runner's target; members reach k6 through makeArgs and env.
  target: Target
  env: Record<string, string>
  report: ActorsReport
  // Proves the reader cannot write, removes the writer's group and proves its access ends while the
  // reader's stays, then removes the fixtures. Throws AccessFailure for a proven access failure.
  finish(): Promise<void>
}

function setting(env: Env, name: string): string {
  const value = env[name]
  check(value, `${name} is required for shared-drive actors`)
  return value
}

// k6 runs in Compose, where only load.localhost resolves: it reaches the fixture's domains at the
// Stack container's address.
function stackAddress(): string {
  const run = (args: string[]) => spawnSync('docker', args, { cwd: harness, encoding: 'utf8' }).stdout?.trim() ?? ''
  const id = run(['compose', 'ps', '-q', 'cozy-stack'])
  const ip = id && run(['inspect', '-f', '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}', id]).split(' ')[0]
  check(ip && /^[0-9a-f.:]+$/.test(ip), 'Cannot find the Stack container address for k6')
  return ip
}

async function poll(settleMs: number, until: () => Promise<boolean>): Promise<boolean> {
  for (const deadline = Date.now() + settleMs; ; await sleep(POLL_MS)) {
    if (await until()) return true
    if (Date.now() > deadline) return false
  }
}

export async function prepareActors(env: Env, executionId: string): Promise<DriveActors> {
  const domain = setting(env, 'ACTORS_DOMAIN')
  const manifest = readManifest(env.SEED_MANIFEST || manifestPath(domain), domain)
  const members = manifest.instances.filter(i => i.role === 'member')
  check(manifest.status === 'complete' && manifest.organization, `${domain} is not a complete organization fixture`)
  check(members.length >= 2, 'The organization fixture needs two members: a writer and a reader')
  // How long members may take to reach the drive, and to lose it once their grant is removed. A Stack
  // still working through an earlier run's jobs can take minutes to accept.
  const settleMs = Number(env.ACTORS_SETTLE_MS || 300_000)
  const adminURL = new URL(setting(env, 'SEED_ADMIN_URL'))
  const stackOrigin = setting(env, 'SEED_STACK_URL')
  const auth = `Basic ${Buffer.from(`${env.SEED_ADMIN_USERNAME || 'admin'}:${setting(env, 'SEED_ADMIN_PASSWORD')}`)
    .toString('base64')}`
  const admin = (method: string, path: string) => request(new URL(path, adminURL), method, { Authorization: auth },
    REQUEST_MS)

  const actors = {} as Record<Role, Actor>
  const roles = [['owner', manifest.instances[0]!], ['writer', members[0]!], ['reader', members[1]!]] as const
  for (const [role, entry] of roles) {
    const current = await admin('GET', `/instances/${entry.domain}`)
    const attributes = current.status === 200 ? parse(current.body)?.data?.attributes : null
    check(attributes?.uuid === entry.uuid && attributes.org_id === manifest.organization,
      `${entry.domain} is not the recorded ${role} of ${domain}`)
    const minted = await admin('POST', `/instances/token?${new URLSearchParams({
      Domain: entry.domain, Audience: 'cli', Scope: role === 'owner' ? OWNER_SCOPE : MEMBER_SCOPE })}`)
    check(minted.status === 200 && minted.body && !/[\s\x00-\x1f\x7f]/.test(minted.body), `Invalid token for ${role}`)
    actors[role] = { role, domain: entry.domain, uuid: entry.uuid, token: minted.body }
  }
  const as = ({ domain, token }: Actor): Api => (method, path, body = '', type = 'application/json') =>
    request(new URL(path, stackOrigin), method, {
      Host: domain, Authorization: `Bearer ${token}`, Accept: 'application/json',
      ...(body ? { 'Content-Type': type } : {}),
    }, REQUEST_MS, body)
  const [owner, writer, reader] = [as(actors.owner), as(actors.writer), as(actors.reader)]
  const json = async (reply: Promise<Reply>, status: number, what: string) => {
    const { status: got, body } = await reply
    check(got === status, `${what}: HTTP ${got}`)
    return parse(body)
  }

  const name = `twake-load-actors-${executionId}`
  const created = (await json(owner('POST', '/sharings/drives', JSON.stringify({ data: {
    type: 'io.cozy.sharings', attributes: { name, description: name } } }), 'application/vnd.api+json'),
  201, 'Creating the drive')).data
  const drive = { id: String(created?.id), root: String(created?.attributes?.rules?.[0]?.values?.[0]) }
  check(created?.id && created.attributes?.org_drive === true, 'Expected an organization drive')
  const scheme = new URL(created.attributes.members[0].instance).protocol
  const fixtures = { groups: [] as string[], contacts: [] as string[] }
  const grant = async (member: Actor, relation: string) => {
    const group = await json(owner('POST', '/data/io.cozy.contacts.groups/',
      JSON.stringify({ name: `${name}-${member.role}` })), 201, 'Creating a group')
    fixtures.groups.push(group.id)
    const contact = await json(owner('POST', '/data/io.cozy.contacts/', JSON.stringify({
      fullname: `Load-test ${member.role}`, email: [{ address: `${member.uuid}@spam.cozycloud.cc`, primary: true }],
      cozy: [{ url: `${scheme}//${member.domain}/`, primary: true }],
      relationships: { groups: { data: [{ _id: group.id, _type: 'io.cozy.contacts.groups' }] } },
    })), 201, 'Creating a contact')
    fixtures.contacts.push(contact.id)
    await json(owner('POST', `/sharings/${drive.id}/recipients`, JSON.stringify({ data: {
      type: 'io.cozy.sharings', id: drive.id,
      relationships: { [relation]: { data: [{ id: group.id, type: 'io.cozy.contacts.groups' }] } } } }),
    'application/vnd.api+json'), 200, `Granting the ${member.role}`)
    return group.id as string
  }
  const writerGroup = await grant(actors.writer, 'recipients')
  await grant(actors.reader, 'read_only_recipients')

  // Members of the organization accept its drives on their own (ORG-DRIVE-FANOUT.md). A member asked
  // before its sharing arrived can stay refused for a while, so the owner's roster comes first.
  const granted = Date.now()
  let roster: string[] = []
  const ready = await poll(settleMs, async () => {
    const members = parse((await owner('GET', `/sharings/${drive.id}`)).body)?.data?.attributes?.members ?? []
    roster = members.slice(1).map((m: { status?: string }) => String(m.status))
    return roster.length === 2 && roster.every(status => status === 'ready')
  })
  check(ready, `The members did not accept the drive in time: ${roster.join(', ') || 'no recipients'}`)
  const acceptMs = Date.now() - granted
  const rootPath = `/sharings/drives/${drive.id}/${drive.root}`
  let statuses: number[] = []
  const accepted = await poll(settleMs, async () => {
    statuses = await Promise.all([writer, reader].map(async api => (await api('GET', rootPath)).status))
    return statuses.every(status => status === 200)
  })
  check(accepted, `The members did not reach the drive in time: writer HTTP ${statuses[0]}, reader HTTP ${statuses[1]}`)
  const upload = (api: Api, role: Role) =>
    api('POST', `${rootPath}?Type=file&Name=${name}-${role}.txt`, role, 'text/plain')
  const access: Record<string, number> = {
    writer_read: (await writer('GET', rootPath)).status, writer_write: (await upload(writer, 'writer')).status,
    reader_read: (await reader('GET', rootPath)).status,
  }
  check(access.writer_write === 201, `The writer could not write to the drive: HTTP ${access.writer_write}`)

  const report: ActorsReport = {
    fixture: domain, drive: drive.id, access, accept_ms: acceptMs,
    placement: Object.values(actors).map(({ role, domain: d }) => ({ role, domain: d, stack: adminURL.host,
      path: role === 'owner' ? 'owner' : 'same-stack' })),
  }
  const address = stackAddress()
  const url = (actor: Actor) => `${scheme}//${actor.domain}`
  return {
    drive, report,
    target: {
      kind: 'local', baseUrl: url(actors.owner), lockedUrls: [], maxPointMs: Infinity,
      makeArgs: [`WRITER_BASE_URL=${url(actors.writer)}`, `READER_BASE_URL=${url(actors.reader)}`,
        `ACTOR_HOSTS=${Object.values(actors).map(a => `${new URL(url(a)).hostname}=${address}`).join(',')}`],
      session: async () => ({ token: actors.owner.token,
        stack: stackClient(stackOrigin, actors.owner.domain, actors.owner.token) }),
    },
    env: { WRITER_ACCESS_TOKEN: actors.writer.token, READER_ACCESS_TOKEN: actors.reader.token },
    async finish() {
      access.reader_write = (await upload(reader, 'reader')).status
      const sharing = await json(owner('GET', `/sharings/${drive.id}`), 200, 'Reading the drive')
      const groups: Array<{ id?: string }> = sharing?.data?.attributes?.groups ?? []
      const index = groups.findIndex(g => g.id === writerGroup)
      check(index >= 0, "The writer's group is missing from the drive")
      const removal = await owner('DELETE', `/sharings/${drive.id}/groups/${index}`)
      check(removal.status === 204, `Removing the writer's group: HTTP ${removal.status}`)
      // The roster can keep the writer for a while; its drive access must end.
      const revoked = await poll(settleMs, async () => [403, 404].includes((await writer('GET', rootPath)).status))
      const revocation = {
        writer_read: (await writer('GET', rootPath)).status, writer_write: (await upload(writer, 'writer')).status,
        reader_read: (await reader('GET', rootPath)).status,
      }
      report.revocation = revocation
      const cleaned: string[] = []
      const retained: string[] = []
      const keep = (label: string, removed: boolean) => (removed ? cleaned : retained).push(label)
      keep(name, await removeDrive(owner, drive.id, drive.root, 2000, POLL_MS, Date.now() + 300_000))
      for (const id of fixtures.groups) keep(id, await removeDoc(owner, 'io.cozy.contacts.groups', id))
      for (const id of fixtures.contacts) keep(id, await removeDoc(owner, 'io.cozy.contacts', id))
      report.cleanup = { cleaned, retained }
      if (access.reader_write < 300) throw new AccessFailure('The read-only member could write to the drive')
      if (!revoked || revocation.writer_write < 300) {
        throw new AccessFailure('The writer kept drive access after its group was removed')
      }
      if (revocation.reader_read !== 200) throw new AccessFailure('The reader lost drive access with the writer')
      check(access.reader_write === 403, `Unexpected read-only write: HTTP ${access.reader_write}`)
      if (retained.length) throw new FixtureError(`Actor fixtures remain: ${retained.join(' ')}`)
    },
  }
}
