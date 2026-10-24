// Seeds a long-lived organization through the Cloudery partner API, for shared environments whose
// Stack admin API is internal (DEV). It never calls the Stack admin API. See SEEDING.md (Cloudery).
import { randomBytes } from 'node:crypto'
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'

import { FixtureError, request } from './fixture-http.ts'

const SCOPE = 'io.cozy.files io.cozy.sharings io.cozy.contacts io.cozy.settings'
const WARMUP = 'Load-test warm-up'

interface CloudInstance { _id: string, fqdn: string, deleting_at?: string | null }
interface Member { instance?: string, email?: string }

function check(value: unknown, message: string): asserts value {
  if (!value) throw new FixtureError(message)
}
function setting(name: string, fallback?: string): string {
  const value = process.env[name] ?? fallback
  check(value, `${name} is required`)
  return value
}
function integer(name: string, fallback: string): number {
  const value = Number(setting(name, fallback))
  check(Number.isSafeInteger(value) && value >= 0, `${name} must be a non-negative integer`)
  return value
}
function parse(text: string): any {
  try { return text ? JSON.parse(text) : {} } catch { throw new FixtureError('Invalid JSON reply') }
}
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))

async function main(): Promise<void> {
  const command = process.argv[2]
  check(command === 'seed' || command === 'teardown', 'Usage: seed-cloudery.ts seed | teardown')
  const secrets = parse(readFileSync(setting('SEED_CLOUDERY_TOKEN_FILE'), 'utf8'))
  check(secrets.id && secrets.public && secrets.manager_url, 'The token file needs id, public and manager_url')
  const manager = new URL(secrets.manager_url)
  const domain = setting('SEED_CLOUDERY_DOMAIN')
  const prefix = setting('SEED_CLOUDERY_PREFIX')
  // Ending with a letter keeps one prefix's names from matching another's, such as seed1 + 05.
  check(/^[a-z0-9]{0,23}[a-z]$/.test(prefix),
    'SEED_CLOUDERY_PREFIX must be 1 to 24 lowercase letters or digits, ending with a letter')
  check(/^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(domain), 'SEED_CLOUDERY_DOMAIN must be a DNS name')
  const users = integer('SEED_USERS', '20')
  const timeout = integer('SEED_REQUEST_TIMEOUT_MS', '60000')
  const pollMs = integer('SEED_CLOUDERY_POLL_MS', '3000')
  const waitMs = integer('SEED_CLOUDERY_WAIT_MS', '900000')
  // CLI tokens last 30 minutes.
  const tokenMaxAge = integer('SEED_CLOUDERY_TOKEN_MAX_AGE_MS', '1200000')
  const stackOrigin = process.env.SEED_STACK_URL

  const host = (slug: string) => `${slug}.${domain}`
  const orgHost = host(`${prefix}org`)
  const memberHosts = Array.from({ length: users }, (_, i) => host(`${prefix}${String(i + 1).padStart(2, '0')}`))
  // The prefix guard: nothing but these exact names is ever adopted or deleted.
  const fixtureName = new RegExp(`^${prefix}(org|\\d{2,})\\.${domain.replaceAll('.', '\\.')}$`)

  const send = async (url: URL, headers: Record<string, string>, method: string, ok: number[], body = '') => {
    const reply = await request(url, method, { Accept: 'application/json', ...headers }, timeout, body, 8 << 20)
    check(ok.includes(reply.status), `${method} ${url.pathname}: HTTP ${reply.status} ${reply.body.slice(0, 200)}`)
    return reply.body
  }
  const cloudery = async (method: string, path: string, ok: number[], body?: object, secret = secrets.id) =>
    parse(await send(new URL(path, manager), {
      Authorization: `Bearer ${secret}`, 'Content-Type': 'application/json',
    }, method, ok, body ? JSON.stringify(body) : ''))

  const tokens = new Map<string, { token: string, at: number }>()
  const token = async (instance: string): Promise<string> => {
    const cached = tokens.get(instance)
    if (cached && Date.now() - cached.at < tokenMaxAge) return cached.token
    const { token } = await cloudery('POST', `/api/public/instances/${instance}/token`, [200],
      { audience: 'cli', scope: SCOPE }, secrets.public)
    check(typeof token === 'string' && token, `Invalid token for ${instance}`)
    tokens.set(instance, { token, at: Date.now() })
    return token
  }
  const stackText = async (instance: string, method: string, path: string, ok: number[], body = '',
    type = 'application/vnd.api+json') =>
    send(new URL(path, stackOrigin ?? `https://${instance}`), {
      Host: instance, Authorization: `Bearer ${await token(instance)}`, 'Content-Type': type,
    }, method, ok, body)
  const stack = async (...args: Parameters<typeof stackText>) => parse(await stackText(...args))

  const poll = async <T>(label: string, probe: () => Promise<T | undefined>): Promise<T> => {
    for (const deadline = Date.now() + waitMs; Date.now() < deadline; await sleep(pollMs)) {
      const value = await probe()
      if (value !== undefined) return value
    }
    throw new FixtureError(`${label}: timed out`)
  }
  const workflow = (id: string, label: string) => poll(`${label} workflow`, async () => {
    const { status } = await cloudery('GET', `/api/v1/workflows/${id}`, [200])
    check(status !== 'failed', `${label}: workflow ${id} failed`)
    return status === 'succeeded' || undefined
  })
  const fixtureInstances = async (): Promise<Map<string, CloudInstance>> => {
    const page = await cloudery('GET', '/api/v2/instances?limit=1000', [200])
    check(Array.isArray(page.items) && page.items.length === page.count, 'Partner instances span several pages')
    return new Map((page.items as CloudInstance[]).filter(i => fixtureName.test(i.fqdn)).map(i => [i.fqdn, i]))
  }

  if (command === 'teardown') {
    const listed = [...(await fixtureInstances()).values()]
    // Members first: the organization instance owns the warm-up drive.
    listed.sort((a, b) => Number(a.fqdn === orgHost) - Number(b.fqdn === orgHost))
    for (const item of listed) {
      if (!item.deleting_at) {
        const { workflow: id } = await cloudery('DELETE', `/api/v1/instances/${item._id}?user_request=false`, [200])
        await workflow(id, item.fqdn)
      }
      // A 404 has an empty body.
      await poll(`${item.fqdn} deletion`, async () =>
        (await cloudery('GET', `/api/v1/instances/${item._id}`, [200, 404]))._id ? undefined : true)
      console.log(`Deleted ${item.fqdn}`)
    }
    // The Cloudery reserves a deleted FQDN for good; releasing it lets a later seed reuse the name.
    for (const name of new Set([...listed.map(i => i.fqdn), orgHost, ...memberHosts])) {
      await cloudery('DELETE', `/api/v2/fqdns/${name}`, [200])
    }
    console.log(`Organization ${prefix} kept: the Cloudery has no route to delete it`)
    return
  }

  const org = await cloudery('GET', `/api/v2/organizations/${prefix}`, [200, 404])
  if (!org.ldap_branch) {
    // The custom domain is the organization's mail domain; the local seeder's OrgDomain is alike.
    await cloudery('POST', '/api/v2/organizations', [201], {
      ldap_branch: prefix, name: 'Load-test organization', custom_domain: `${prefix}.seed.invalid`,
    })
  }
  const existing = await fixtureInstances()
  const creating: Array<[string, string]> = []
  for (const name of [orgHost, ...memberHosts]) {
    const item = existing.get(name)
    if (item) {
      check(!item.deleting_at, `${name} is being deleted`)
      continue
    }
    const slug = name.slice(0, name.indexOf('.'))
    const created = await cloudery('POST', '/api/v1/instances', [201], {
      offer: setting(name === orgHost ? 'SEED_CLOUDERY_ORG_OFFER' : 'SEED_CLOUDERY_MEMBER_OFFER',
        name === orgHost ? 'b2b_org_default' : 'b2b_twake_default'),
      slug, domain, org_id: prefix, email: `${slug}@example.com`, public_name: slug, locale: 'en',
      skip_email_validation: true,
    })
    check(created.fqdn === name && created.workflow, `${name}: unexpected creation reply`)
    console.log(`Creating ${name}`)
    creating.push([name, created.workflow])
  }
  for (const [name, id] of creating) await workflow(id, name)

  const listed = await fixtureInstances()
  for (const name of [orgHost, ...memberHosts]) {
    const attributes = (await stack(name, 'GET', '/settings/instance', [200])).data?.attributes
    check(attributes?.org_id === prefix && attributes.uuid === listed.get(name)?._id,
      `${name} is not the Cloudery instance of organization ${prefix}`)
  }

  // Warm-up: accepting one drive from the organization instance makes it a trusted contact, so
  // later drives auto-accept. shortcut: needed until DEV trusts the domain (#5070).
  const drives = (await stack(orgHost, 'GET', '/sharings/drives', [200])).data ?? []
  const sid: string = drives.find((d: any) => d.attributes?.owner && d.attributes.description === WARMUP)?.id ??
    (await stack(orgHost, 'POST', '/sharings/drives', [201], JSON.stringify({
      data: { type: 'io.cozy.sharings', attributes: { name: WARMUP, description: WARMUP } },
    }))).data.id
  const roster: Member[] = (await stack(orgHost, 'GET', `/sharings/${sid}`, [200])).data.attributes.members
  const invited = new Set(roster.map(m => m.instance && new URL(m.instance).host))
  const contacts: string[] = []
  for (const name of memberHosts.filter(name => !invited.has(name))) {
    contacts.push((await stack(orgHost, 'POST', '/data/io.cozy.contacts/', [201], JSON.stringify({
      fullname: name, email: [{ address: `${name.slice(0, name.indexOf('.'))}@example.com`, primary: true }],
      cozy: [{ url: `https://${name}/`, primary: true }],
    }), 'application/json')).id)
  }
  if (contacts.length) {
    await stack(orgHost, 'POST', `/sharings/${sid}/recipients`, [200], JSON.stringify({ data: {
      type: 'io.cozy.sharings', id: sid,
      relationships: { recipients: { data: contacts.map(id => ({ id, type: 'io.cozy.contacts' })) } },
    } }))
  }
  for (const name of memberHosts) {
    const sharing = await poll(`${name} warm-up invitation`, async () =>
      (await stack(name, 'GET', `/sharings/${sid}`, [200, 404])).data)
    if (!sharing.attributes.active) await stack(name, 'POST', `/sharings/drives/${sid}/accept`, [200])
    const sender = sharing.attributes.members?.[0]?.email
    const { rows } = await stack(name, 'GET', '/data/io.cozy.contacts/_all_docs?include_docs=true', [200])
    const trusted = rows?.some((r: any) => r.doc?.trustedForSharing === true &&
      r.doc.email?.some((e: any) => e.address === sender))
    check(sender && trusted, `${name} is not warmed up: the organization instance is not a trusted contact`)
  }
  console.log(`Warmed up ${memberHosts.length} members`)

  if (memberHosts[0]) {
    const content = randomBytes(512).toString('hex')
    const file = await stack(memberHosts[0], 'POST',
      `/files/io.cozy.files.root-dir?Type=file&Name=seed-smoke-${Date.now()}.txt`, [201], content, 'text/plain')
    const read = await stackText(memberHosts[0], 'GET', `/files/download/${file.data.id}`, [200])
    check(read === content, `${memberHosts[0]}: the 1 KiB smoke read back different content`)
    await stack(memberHosts[0], 'DELETE', `/files/${file.data.id}`, [200])
    console.log(`Smoke passed on ${memberHosts[0]}`)
  }

  const manifest = setting('SEED_CLOUDERY_MANIFEST', `data/results/${prefix}-cloudery-fixture.json`)
  mkdirSync(dirname(manifest), { recursive: true })
  writeFileSync(manifest, `${JSON.stringify({
    domain, organization: prefix, warmup_sharing_id: sid,
    instances: [...listed.values()].map(i => ({
      fqdn: i.fqdn, uuid: i._id, role: i.fqdn === orgHost ? 'organization' : 'member',
    })),
  }, null, 2)}\n`, { mode: 0o600 })
  console.log(`Seed complete: ${orgHost} with ${users} members; manifest ${manifest}`)
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : 'Seed command failed')
  process.exitCode = 1
})
