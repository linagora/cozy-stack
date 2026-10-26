import { createHash, randomBytes, randomUUID } from 'node:crypto'

import { FixtureError as CliError, request, type Reply } from './fixture-http.ts'
import {
  manifestPath, readManifest, removeManifest, writeManifest, type FixtureManifest, type ManifestInstance,
} from './fixture-manifest.ts'
import { createSharedDrive } from './prepare-shared-drive.ts'

function check(value: unknown, message: string): asserts value {
  if (!value) throw new CliError(message)
}

function integer(name: string, fallback: string, max: number): number {
  const raw = process.env[name] ?? fallback
  const value = Number(raw)
  check(/^\d+$/.test(raw) && Number.isSafeInteger(value) && value <= max,
    `${name} must be an integer between 0 and ${max}`)
  return value
}

function validateDomain(domain: string): void {
  const [host = '', port, ...extra] = domain.split(':')
  const label = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/
  const validPort = port === undefined || (/^[1-9]\d{0,4}$/.test(port) && Number(port) <= 65535)
  check(/^seed-[a-f0-9]{32}\./.test(host) && host.length <= 253 &&
    host.split('.').every((part) => label.test(part)) && !extra.length && validPort,
  'Domain must be seed-<32 lowercase hex>.<DNS suffix>[:port]')
}

function origin(name: string): URL {
  const raw = process.env[name] ?? ''
  try {
    const url = new URL(raw)
    check(/^https?:\/\/[^/?#@\\\s]+\/?$/i.test(raw) &&
      ['http:', 'https:'].includes(url.protocol) && !url.username && !url.password &&
      url.pathname === '/' && !url.search && !url.hash, '')
    return url
  } catch {
    throw new CliError(`${name} must be an HTTP(S) origin without credentials, path, query or fragment`)
  }
}

function expectStatus(reply: Reply, status: number): Reply {
  check(reply.status === status, `Unexpected API status: HTTP ${reply.status}`)
  return reply
}

interface Resource {
  id: string
  type?: string
  attributes?: {
    domain?: string, uuid?: string, type?: string, org_id?: string, org_domain?: string, context?: string,
    md5sum?: string, size?: string, dir_id?: string
  }
  relationships?: {
    referenced_by?: { data?: Array<{ type?: string, id?: string }> }, contents?: { meta?: { count?: number } }
  }
}
function resource(reply: Reply): Resource {
  let data: Resource | undefined
  try { data = JSON.parse(reply.body)?.data } catch { throw new CliError('Invalid API JSON') }
  check(data && typeof data.id === 'string' && data.id.trim(), 'Invalid API resource')
  return data
}

// Instances created without a UUID (e.g. `cozy-stack instances add`) are ordinary
// registry entries; only fixture instances must carry one.
function identity(item: Resource, requireUUID = true): boolean {
  const fields = [item?.id, item?.attributes?.domain, ...(requireUUID ? [item?.attributes?.uuid] : [])]
  return fields.every((value) =>
    typeof value === 'string' && value.length > 0 && !/[\s\x00-\x1f\x7f]/.test(value))
}

type PublicAPI = (method: string, path: string, body?: string | Buffer, headers?: Record<string, string>) =>
  Promise<Reply>

async function checkDriveRoot(api: PublicAPI, root: string, sharing: string): Promise<void> {
  const dir = resource(expectStatus(await api('GET', `/files/${encodeURIComponent(root)}`), 200))
  const refs = dir.relationships?.referenced_by?.data
  const backlink = Array.isArray(refs) && refs.some((ref) => ref?.type === 'io.cozy.sharings' && ref.id === sharing)
  check(dir.id === root && dir.type === 'io.cozy.files' && dir.attributes?.type === 'directory' && backlink,
    'Shared-drive root or backlink mismatch')
}

// The Stack counts root entries without the trash.
async function rootEntries(api: PublicAPI): Promise<number> {
  const root = resource(expectStatus(await api('GET', '/files/io.cozy.files.root-dir?page[limit]=1'), 200))
  const entries = root.relationships?.contents?.meta?.count
  check(Number.isSafeInteger(entries) && entries! >= 0, 'Invalid root directory count')
  return entries!
}

function partial(error: unknown, domain: string): CliError {
  const message = error instanceof CliError ? error.message : 'Seed API operation failed'
  return new CliError(`${message}. Instance ${domain} may be partial; ` +
    'run explicit cleanup only after server writes have stopped (a timeout/disconnect is not proof).')
}

async function main(): Promise<void> {
  const [command, suppliedDomain, ...extra] = process.argv.slice(2)
  check(['seed', 'cleanup', 'seed-org', 'cleanup-org', 'verify'].includes(command ?? '') && !extra.length &&
    (command?.startsWith('seed') || suppliedDomain),
  'Usage: seed[-org] [domain] | cleanup[-org] <domain> | verify <domain>; ' +
    'credentials belong in environment variables')
  const organization = command?.endsWith('-org')
  check(suppliedDomain || process.env.SEED_DOMAIN_SUFFIX,
    'SEED_DOMAIN_SUFFIX is required when no seed domain is supplied')
  const domain = suppliedDomain ?? `seed-${randomBytes(16).toString('hex')}.${process.env.SEED_DOMAIN_SUFFIX}`
  validateDomain(domain)
  const adminURL = origin('SEED_ADMIN_URL')
  const username = process.env.SEED_ADMIN_USERNAME ?? 'admin'
  const password = process.env.SEED_ADMIN_PASSWORD
  check(password && username && !/[:\x00-\x1f\x7f]/.test(username),
    'SEED_ADMIN_PASSWORD and a valid SEED_ADMIN_USERNAME are required')
  const timeout = integer('SEED_REQUEST_TIMEOUT_MS', '1800000', 2_147_483_647)
  check(timeout, 'SEED_REQUEST_TIMEOUT_MS must be positive')
  const authorization = `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}`
  const admin = (method: string, path: string, params?: Record<string, string>) => {
    const url = new URL(path, adminURL)
    if (params) url.search = new URLSearchParams(params).toString()
    return request(url, method, { Authorization: authorization }, timeout)
  }
  // Echo's domain route does not decode %3A; preserve the validated literal public port.
  const instancePath = `/instances/${domain}`
  const manifestFile = manifestPath(domain)
  const session = async (stackURL: URL | undefined, name: string) => {
    check(stackURL, 'SEED_STACK_URL is required for population')
    const accessToken = expectStatus(await admin('POST', '/instances/token', {
      Domain: name, Audience: 'cli', Scope: 'io.cozy.files io.cozy.sharings',
    }), 200).body
    check(accessToken && !/[\s\x00-\x1f\x7f]/.test(accessToken), 'Invalid CLI token')
    const api: PublicAPI = (method, path, body = '', headers = {}) =>
      request(new URL(path, stackURL), method, {
        ...headers, Host: name, Authorization: `Bearer ${accessToken}`,
      }, timeout, body)
    return { accessToken, api }
  }
  if (command === 'cleanup') {
    // Only recorded identities are deleted: a planned UUID also proves a creation whose reply was lost.
    const manifest = readManifest(manifestFile, domain)
    check(!manifest.organization, `${manifestFile} records an organization; use cleanup-org`)
    for (const entry of manifest.instances) {
      const path = `/instances/${entry.domain}`
      const current = await admin('GET', path)
      if (current.status !== 404) {
        check(resource(expectStatus(current, 200)).attributes?.uuid === entry.uuid,
          `${entry.domain} is not the recorded instance; kept`)
        const deleted = await admin('DELETE', path)
        check(deleted.status === 204 || deleted.status === 404, `Cleanup failed: HTTP ${deleted.status}`)
      }
      const readback = await admin('GET', path)
      check(readback.status === 404, `Cleanup absence not confirmed: HTTP ${readback.status}`)
    }
    removeManifest(manifestFile)
    console.log(`Cleanup complete: ${domain}`)
    return
  }
  const orgID = domain.split('.')[0]!
  const suffix = domain.slice(orgID.length)
  const orgDomain = `${orgID}.seed.invalid`
  const list = async (): Promise<Resource[]> => {
    const items: Resource[] = []
    const seen = new Set<string>()
    const ids = new Set<string>()
    const domains = new Set<string>()
    let path = '/instances?page[limit]=10'
    while (path) {
      let url: URL
      try {
        check(typeof path === 'string' && !/[\s\\]/.test(path), '')
        url = new URL(path, adminURL)
        check(url.origin === adminURL.origin && url.pathname === '/instances' &&
          !url.username && !url.password && !url.hash, '')
        url.searchParams.sort()
        check(!seen.has(url.href), '')
        seen.add(url.href)
      } catch { throw new CliError('Invalid instance pagination link or loop') }
      const reply = expectStatus(await admin('GET', url.href), 200)
      try {
        const page = JSON.parse(reply.body)
        check(Array.isArray(page?.data), 'Invalid instance list')
        for (const item of page.data as Resource[]) {
          check(identity(item, false) && !ids.has(item.id) && !domains.has(item.attributes!.domain!), '')
          ids.add(item.id)
          domains.add(item.attributes!.domain!)
          items.push(item)
        }
        check(page.links == null || (typeof page.links === 'object' && !Array.isArray(page.links)), '')
        const next: unknown = page.links?.next
        check(next == null || (typeof next === 'string' && next.length > 0), '')
        path = next ?? ''
      } catch { throw new CliError('Invalid instance list') }
    }
    return items
  }
  const inScope = (item: Resource) =>
    item.attributes?.org_id === orgID || item.attributes?.org_domain === orgDomain
  const checkScope = (item: Resource) => {
    check(identity(item) && item.attributes?.org_id === orgID && item.attributes.org_domain === orgDomain,
      'Organization scope or identity mismatch')
    check(/^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$/.test(item.attributes.uuid!),
      'Organization fixture UUID identity mismatch')
    const name = item.attributes.domain!
    validateDomain(name)
    check(name.slice(name.indexOf('.')) === suffix, 'Organization scope suffix mismatch')
  }
  const same = (a: Resource, b: Resource) => a.id === b.id &&
    a.attributes?.domain === b.attributes?.domain && a.attributes?.uuid === b.attributes?.uuid &&
    a.attributes?.org_id === b.attributes?.org_id && a.attributes?.org_domain === b.attributes?.org_domain
  if (command === 'cleanup-org') {
    const manifest = readManifest(manifestFile, domain)
    check(manifest.organization === orgID, `${manifestFile} records no organization; use cleanup`)
    const recorded = (item: Resource) => manifest.instances.some((entry) =>
      entry.domain === item.attributes?.domain && entry.uuid === item.attributes.uuid)
    try {
      const rootReply = await admin('GET', instancePath)
      const root = rootReply.status === 404 ? undefined : resource(expectStatus(rootReply, 200))
      if (root) {
        checkScope(root)
        check(root.attributes?.domain === domain, 'Organization root identity mismatch')
      }
      const candidates = (await list()).filter(inScope)
      for (const item of candidates) checkScope(item)
      const listedRoot = candidates.find((item) => item.attributes?.domain === domain)
      check(root ? listedRoot && same(root, listedRoot) : !listedRoot,
        'Organization root registry identity mismatch')
      check(!root || recorded(root), 'Organization root is not the recorded instance; kept')
      const remove = async (item: Resource) => {
        const path = `/instances/${item.attributes!.domain!}`
        const current = resource(expectStatus(await admin('GET', path), 200))
        checkScope(current)
        check(same(item, current), 'Cleanup readback identity mismatch')
        const deleted = await admin('DELETE', path)
        check(deleted.status === 204 || deleted.status === 404, `Cleanup failed: HTTP ${deleted.status}`)
        expectStatus(await admin('GET', path), 404)
      }
      // Unrecorded members are someone else's: they stay, and so does the root.
      for (const item of candidates) if (item !== listedRoot && recorded(item)) await remove(item)
      check((await list()).filter(inScope).every((item) => item.attributes?.domain === domain),
        'Organization members remain; root retained')
      if (root) await remove(root)
      expectStatus(await admin('GET', instancePath), 404)
      check(!(await list()).some(inScope), 'Organization scope remains after cleanup')
    } catch (error) { throw partial(error, domain) }
    removeManifest(manifestFile)
    console.log(`Cleanup complete: ${domain}`)
    return
  }
  if (command === 'verify') {
    // Before use and after a run: recorded identities, objects, content and root entries must be unchanged.
    const manifest = readManifest(manifestFile, domain)
    check(manifest.status === 'complete',
      `The fixture manifest is ${manifest.status}; only a complete fixture is usable`)
    const { parameters, content } = manifest
    const stackURL = parameters.files || parameters.drives ? origin('SEED_STACK_URL') : undefined
    for (const entry of manifest.instances) {
      const current = resource(expectStatus(await admin('GET', `/instances/${entry.domain}`), 200)).attributes
      check(current?.domain === entry.domain && current.uuid === entry.uuid &&
        (current.org_id ?? null) === manifest.organization, `${entry.domain} is not the recorded instance`)
      const populated = entry.role !== 'organization'
      check(entry.created && entry.files.length === (populated ? parameters.files : 0) &&
        entry.drives.length === (populated ? parameters.drives : 0), `${entry.domain} lacks recorded objects`)
      if (!populated || (!parameters.files && !parameters.drives)) continue
      const { api } = await session(stackURL, entry.domain)
      for (const id of entry.files) {
        const file = resource(expectStatus(await api('GET', `/files/${encodeURIComponent(id)}`), 200))
        check(file.id === id && file.attributes?.md5sum === content.md5 && Number(file.attributes.size) ===
          content.bytes && file.attributes.dir_id === 'io.cozy.files.root-dir', `${entry.domain}: file ${id} changed`)
      }
      for (const drive of entry.drives) await checkDriveRoot(api, drive.root, drive.sharing)
      const entries = await rootEntries(api)
      check(entries === entry.root_entries,
        `${entry.domain}: the root holds ${entries} entries; the manifest records ${entry.root_entries}`)
    }
    if (manifest.organization) {
      const members = (await list()).filter(inScope)
      check(members.length === manifest.instances.length && manifest.instances.every((entry) =>
        members.some((item) => item.attributes?.domain === entry.domain && item.attributes.uuid === entry.uuid)),
      'Organization membership differs from the manifest')
    }
    console.log(`Fixture verified: ${domain}`)
    return
  }
  const contextName = process.env.SEED_CONTEXT_NAME
  check(contextName === undefined || (contextName.length > 0 && !/[\s\x00-\x1f\x7f]/.test(contextName)),
    'SEED_CONTEXT_NAME must be nonempty and contain no whitespace or control characters')
  const users = organization ? integer('SEED_USERS', '3', 1000) : 0
  const files = integer('SEED_FILES', organization ? '0' : '100', 1_000_000)
  const drives = integer('SEED_DRIVES', '0', 100_000)
  check(organization || files || drives, 'At least one file or drive is required')
  const stackURL = !organization || files || drives ? origin('SEED_STACK_URL') : undefined
  const names = [domain, ...Array.from({ length: users }, () =>
    `seed-${randomBytes(16).toString('hex')}${suffix}`)]
  for (const name of names) console.log(`Seed instance: ${name}`)
  const body = Buffer.alloc(1024, 's')
  const md5 = createHash('md5').update(body).digest('base64')
  // UUIDs are chosen before any write, so cleanup can prove ownership even after a lost creation reply.
  const manifest: FixtureManifest = {
    version: 1, profile: 'flat', status: 'preparing', domain, organization: organization ? orgID : null,
    parameters: { users, files, drives, context: contextName ?? null }, content: { bytes: body.length, md5 },
    instances: names.map((name, index) => ({
      domain: name, uuid: randomUUID(), role: organization ? (index ? 'member' : 'organization') : 'owner',
      created: false, files: [], drives: [],
    })),
  }
  const create = async ({ domain: name, uuid }: ManifestInstance): Promise<Resource> => {
    const created = resource(expectStatus(await admin('POST', '/instances', {
      Domain: name, UUID: uuid, Locale: 'en', Email: `${uuid}@spam.cozycloud.cc`,
      PublicName: 'Disposable seed fixture', CouchCluster: '-1', SwiftLayout: '-1', AutoUpdate: 'false',
      ...(organization ? { OrgID: orgID, OrgDomain: orgDomain } : {}),
      ...(contextName ? { ContextName: contextName } : {}),
    }), 201))
    const matches = (item: Resource) => identity(item) &&
      item.attributes?.domain === name && item.attributes?.uuid === uuid &&
      (!contextName || item.attributes.context === contextName) &&
      (!organization || (item.attributes.org_id === orgID && item.attributes.org_domain === orgDomain))
    check(matches(created), 'Created instance identity mismatch')
    const persisted = resource(expectStatus(await admin('GET', `/instances/${name}`), 200))
    check(matches(persisted) && persisted.id === created.id, 'Instance readback identity mismatch')
    return persisted
  }
  const populate = async (entry: ManifestInstance): Promise<void> => {
    if (!files && !drives) return
    if (organization) console.log(`Seed owner: ${entry.domain}; files=${files}; drives=${drives}`)
    const { accessToken, api: publicAPI } = await session(stackURL, entry.domain)
    for (let index = 1; index <= files; index++) {
      const query = new URLSearchParams({ Type: 'file', Name: `seed-file-${index}.txt` })
      const file = resource(expectStatus(await publicAPI('POST', `/files/io.cozy.files.root-dir?${query}`, body, {
        'Content-Type': 'text/plain', 'Content-MD5': md5,
      }), 201))
      entry.files.push(file.id)
    }
    for (let index = 1; index <= drives; index++) {
      const drive = await createSharedDrive({
        baseUrl: stackURL!.origin, accessToken, driveName: `Seed shared drive ${index}`,
        fetcher: async (input, init) => {
          const headers = Object.fromEntries(new Headers(init?.headers))
          const path = new URL(String(input)).pathname
          const reply = expectStatus(await publicAPI(init?.method ?? 'GET', path,
            String(init?.body ?? ''), headers), 201)
          return new Response(reply.body, { status: reply.status })
        },
      })
      check(typeof drive.sharingId === 'string' && drive.sharingId.trim() &&
        typeof drive.rootDirectoryId === 'string' && drive.rootDirectoryId.trim(), 'Invalid shared-drive identity')
      await checkDriveRoot(publicAPI, drive.rootDirectoryId, drive.sharingId)
      entry.drives.push({ sharing: drive.sharingId, root: drive.rootDirectoryId })
    }
    entry.root_entries = await rootEntries(publicAPI)
  }
  let recording = false
  try {
    for (const name of names) {
      const absent = await admin('GET', `/instances/${name}`)
      check(absent.status === 404, absent.status === 200 ? 'Instance already exists' :
        `Instance lookup failed: HTTP ${absent.status}`)
    }
    if (organization) check(!(await list()).some(inScope), 'Organization scope already exists')
    writeManifest(manifestFile, manifest, true)
    recording = true
    console.log(`Seed manifest: ${manifestFile}`)
    const created: Resource[] = []
    for (const entry of manifest.instances) {
      created.push(await create(entry))
      entry.created = true
      writeManifest(manifestFile, manifest)
      if (entry.role !== 'organization') await populate(entry)
      writeManifest(manifestFile, manifest)
    }
    if (organization) {
      const members = (await list()).filter(inScope)
      check(members.length === created.length && created.every((expected) =>
        members.some((item) => same(item, expected))), 'Organization membership mismatch')
    }
    manifest.status = 'complete'
    writeManifest(manifestFile, manifest)
    const counts = `${organization ? `users=${users}; ` : ''}files=${files}; drives=${drives}`
    console.log(`Seed complete: ${domain}; ${counts}`)
  } catch (error) {
    // The manifest keeps every confirmed object and planned identity for verification and cleanup.
    if (recording) writeManifest(manifestFile, { ...manifest, status: 'failed' })
    throw partial(error, domain)
  }
}

main().catch((error: unknown) => {
  console.error(error instanceof CliError ? error.message : 'Seed command failed')
  process.exitCode = 1
})
