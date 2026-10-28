// Seeds a long-lived organization, or one instance holding the tree profile, through the Cloudery
// partner API, for shared environments whose Stack admin API is internal (DEV). It never calls the Stack
// admin API. See SEEDING.md (Cloudery).
import { randomBytes } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'

import { TREE, TREE_VERSION, treeDigest, treeFiles } from './dataset-profile.ts'
import { FixtureError, request } from './fixture-http.ts'
import { manifestPath, readManifest, writeManifest, type ManifestInstance } from './fixture-manifest.ts'
import { verifyTree, writeTree, type PublicAPI } from './tree-fixture.ts'

const SCOPE = 'io.cozy.files io.cozy.settings io.cozy.sharings'
// A tree instance belongs to no organization.
const TREE_OFFER = 'twake_default'
// Mail to test instances lands in a test-only mailbox domain.
const MAIL_DOMAIN = 'spam.cozycloud.cc'

interface CloudInstance { _id: string, fqdn: string, deleting_at?: string | null }
interface Recorded { fqdn: string, uuid: string }
// Exit 1 is reserved for proven correctness failures; everything else exits 2.
class CorrectnessError extends Error {}

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
  const command = process.argv[2] ?? ''
  check(['seed', 'teardown', 'seed-tree', 'verify-tree', 'teardown-tree'].includes(command),
    'Usage: seed-cloudery.ts seed | teardown | seed-tree | verify-tree | teardown-tree')
  const tree = command.endsWith('-tree')
  const secrets = parse(readFileSync(setting('SEED_CLOUDERY_TOKEN_FILE'), 'utf8'))
  check(secrets.id && secrets.public && secrets.manager_url, 'The token file needs id, public and manager_url')
  const manager = new URL(secrets.manager_url)
  const domain = setting('SEED_CLOUDERY_DOMAIN')
  const prefix = setting('SEED_CLOUDERY_PREFIX')
  // Ending with a letter keeps one prefix's names from matching another's, such as seed1 + 05.
  check(/^[a-z0-9]{0,23}[a-z]$/.test(prefix),
    'SEED_CLOUDERY_PREFIX must be 1 to 24 lowercase letters or digits, ending with a letter')
  check(/^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(domain), 'SEED_CLOUDERY_DOMAIN must be a DNS name')
  const users = tree ? 0 : integer('SEED_USERS', '20')
  const timeout = integer('SEED_REQUEST_TIMEOUT_MS', '60000')
  const pollMs = integer('SEED_CLOUDERY_POLL_MS', '3000')
  const waitMs = integer('SEED_CLOUDERY_WAIT_MS', '900000')
  // CLI tokens last 30 minutes.
  const tokenMaxAge = integer('SEED_CLOUDERY_TOKEN_MAX_AGE_MS', '1200000')
  const stackOrigin = process.env.SEED_STACK_URL

  const host = (slug: string) => `${slug}.${domain}`
  // The Stack treats an instance as its organization's own only when its first label is the
  // organization ID: only then are its drives organization drives.
  const orgHost = host(prefix)
  const memberHosts = Array.from({ length: users }, (_, i) => host(`${prefix}${String(i + 1).padStart(2, '0')}`))
  // The prefix guard: nothing but these exact names is ever adopted or deleted.
  const fixtureName = new RegExp(`^${prefix}${tree ? '' : '(\\d{2,})?'}\\.${domain.replaceAll('.', '\\.')}$`)

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
  const api: PublicAPI = async (method, path, body = '', headers = {}) =>
    request(new URL(path, stackOrigin ?? `https://${orgHost}`), method, {
      ...headers, Host: orgHost, Authorization: `Bearer ${await token(orgHost)}`,
    }, timeout, body)

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
  // A matching name alone may belong to another organization: the Stack must confirm both identities.
  const owned = async (item: CloudInstance): Promise<boolean> => {
    const attributes = (await stack(item.fqdn, 'GET', '/settings/instance', [200])).data?.attributes
    return (attributes?.org_id || null) === (tree ? null : prefix) && attributes.uuid === item._id
  }
  // A tree instance's manifest is a fixture manifest (fixture-manifest.ts), as the mixed workload reads it.
  const manifest = tree ? manifestPath(orgHost)
    : setting('SEED_CLOUDERY_MANIFEST', `data/results/${prefix}-cloudery-fixture.json`)

  if (command === 'verify-tree') {
    // Before and after each mixed run: the recorded instance, and its tree unchanged.
    const record = readManifest(manifest, orgHost)
    const entry = record.instances[0]!
    check(record.status === 'complete' && record.profile === 'tree' && record.profile_version === TREE_VERSION,
      `${manifest} is not a complete tree fixture, version ${TREE_VERSION}`)
    check(await owned({ _id: entry.uuid, fqdn: orgHost }), `${orgHost} is not the recorded instance`)
    await verifyTree(api, record.generation_seed!, entry)
    console.log(`Fixture verified: ${orgHost}`)
    return
  }

  if (command.startsWith('teardown')) {
    // The seed's manifest lists the identities it verified; it vouches for instances already being deleted.
    const record = existsSync(manifest) ? parse(readFileSync(manifest, 'utf8')) : { instances: [] }
    check(Array.isArray(record.instances) && (!record.organization || record.organization === prefix),
      `${manifest} is not the manifest of organization ${prefix}`)
    // An organization manifest names its instances fqdn, a tree fixture manifest domain.
    const recorded: Recorded[] = record.instances.map((i: Recorded & { domain?: string }) =>
      ({ fqdn: i.fqdn ?? i.domain, uuid: i.uuid }))
    const isRecorded = (i: CloudInstance) => recorded.some(r => r.fqdn === i.fqdn && r.uuid === i._id)
    const listed = [...(await fixtureInstances()).values()]
    // Members first: the organization instance owns the drives shared with them.
    listed.sort((a, b) => Number(a.fqdn === orgHost) - Number(b.fqdn === orgHost))
    const deleted: string[] = []
    const kept: string[] = []
    for (const item of listed) {
      if (!(item.deleting_at ? isRecorded(item) : await owned(item))) {
        kept.push(item.fqdn)
        continue
      }
      if (!item.deleting_at) {
        const { workflow: id } = await cloudery('DELETE', `/api/v1/instances/${item._id}?user_request=false`, [200])
        await workflow(id, item.fqdn)
      }
      // A 404 has an empty body.
      await poll(`${item.fqdn} deletion`, async () =>
        (await cloudery('GET', `/api/v1/instances/${item._id}`, [200, 404]))._id ? undefined : true)
      console.log(`Deleted ${item.fqdn}`)
      deleted.push(item.fqdn)
    }
    // The Cloudery reserves a deleted FQDN for good; releasing it lets a later seed reuse the name.
    // Only names this run deleted, or recorded ones no longer in use, are ours to release.
    const free = recorded.map(r => r.fqdn).filter(name => !listed.some(i => i.fqdn === name))
    for (const name of new Set([...deleted, ...free])) {
      await cloudery('DELETE', `/api/v2/fqdns/${name}`, [200])
    }
    check(!kept.length, `Kept instances not verified as ${tree ? 'the tree fixture' : `organization ${prefix}`}: ` +
      kept.join(', '))
    if (!tree) console.log(`Organization ${prefix} kept: the Cloudery has no route to delete it`)
    return
  }

  const org = tree ? {} : await cloudery('GET', `/api/v2/organizations/${prefix}`, [200, 404])
  if (!tree && !org.ldap_branch) {
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
      // The tree is written once, into a new instance: an existing one may hold a partial tree.
      check(!tree, `${name} already exists; make seed-scale-cleanup removes it first`)
      continue
    }
    const slug = name.slice(0, name.indexOf('.'))
    const created = await cloudery('POST', '/api/v1/instances', [201], {
      offer: tree ? TREE_OFFER : setting(name === orgHost ? 'SEED_CLOUDERY_ORG_OFFER' : 'SEED_CLOUDERY_MEMBER_OFFER',
        name === orgHost ? 'b2b_org_default' : 'b2b_twake_default'),
      slug, domain, ...(tree ? {} : { org_id: prefix }), email: `${slug}@${MAIL_DOMAIN}`, public_name: slug,
      locale: 'en', skip_email_validation: true,
    })
    check(created.fqdn === name && created.workflow, `${name}: unexpected creation reply`)
    console.log(`Creating ${name}`)
    creating.push([name, created.workflow])
  }
  for (const [name, id] of creating) await workflow(id, name)

  const listed = await fixtureInstances()
  const verified: CloudInstance[] = []
  for (const name of [orgHost, ...memberHosts]) {
    const item = listed.get(name)
    check(item && await owned(item),
      `${name} is not the Cloudery instance of ${tree ? 'the tree fixture' : `organization ${prefix}`}`)
    verified.push(item)
  }

  if (tree) {
    const startedAt = new Date()
    const entry: ManifestInstance = {
      domain: orgHost, uuid: verified[0]!._id, role: 'owner', created: true, files: [], dirs: [], drives: [],
    }
    const seed = process.env.SEED_GENERATION_SEED || '1'
    check(/^[A-Za-z0-9._-]{1,64}$/.test(seed), 'SEED_GENERATION_SEED must be 1 to 64 letters, digits, ., _ or -')
    let bytes: number
    try { bytes = await writeTree(api, seed, entry) } catch (error) {
      throw new FixtureError(`${error instanceof Error ? error.message : 'Writing the tree failed'}. ${orgHost} ` +
        'may hold a partial tree; make seed-scale-cleanup removes it')
    }
    const files = treeFiles(seed)
    const endedAt = new Date()
    writeManifest(manifest, {
      version: 1, profile: 'tree', profile_version: TREE_VERSION, generation_seed: seed, status: 'complete',
      domain: orgHost, organization: null,
      parameters: { users: 0, files: TREE.files.length, drives: 1, context: TREE_OFFER },
      content: { bytes: files.reduce((sum, f) => sum + f.size, 0), md5: treeDigest(files) },
      instances: [entry],
      preparation: { started_at: startedAt.toISOString(), ended_at: endedAt.toISOString(),
        duration_ms: endedAt.getTime() - startedAt.getTime(), bytes },
    })
    console.log(`Seed complete: ${orgHost}; profile=tree; manifest ${manifest}`)
    return
  }

  // Members auto-accept the organization's drives because they share its organization ID, which
  // their context enables with common_contacts and sharing.auto_accept_trusted (cozy-stack #4966).
  for (const name of memberHosts) {
    const context = (await stack(name, 'GET', '/settings/context', [200])).data?.attributes
    check(context?.common_contacts === true && context.sharing?.auto_accept_trusted === true,
      `${name} cannot auto-accept the organization's drives: its context needs common_contacts and sharing.auto_accept_trusted`)
  }

  if (memberHosts[0]) {
    const content = randomBytes(512).toString('hex')
    const file = await stack(memberHosts[0], 'POST',
      `/files/io.cozy.files.root-dir?Type=file&Name=seed-smoke-${Date.now()}.txt`, [201], content, 'text/plain')
    const read = await stackText(memberHosts[0], 'GET', `/files/download/${file.data.id}`, [200])
    if (read !== content) throw new CorrectnessError(`${memberHosts[0]}: the 1 KiB smoke read back different content`)
    await stack(memberHosts[0], 'DELETE', `/files/${file.data.id}`, [200])
    console.log(`Smoke passed on ${memberHosts[0]}`)
  }

  mkdirSync(dirname(manifest), { recursive: true })
  writeFileSync(manifest, `${JSON.stringify({
    domain, organization: prefix,
    instances: verified.map(i => ({
      fqdn: i.fqdn, uuid: i._id, role: i.fqdn === orgHost ? 'organization' : 'member',
    })),
  }, null, 2)}\n`, { mode: 0o600 })
  console.log(`Seed complete: ${orgHost} with ${users} members; manifest ${manifest}`)
}

main().catch((error: unknown) => {
  console.error(error instanceof Error ? error.message : 'Seed command failed')
  process.exitCode = error instanceof CorrectnessError ? 1 : 2
})
