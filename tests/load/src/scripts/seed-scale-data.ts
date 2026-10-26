import { createHash, randomBytes, randomUUID } from 'node:crypto'

import { FixtureError as CliError, request, type Reply } from './fixture-http.ts'
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
    domain?: string, uuid?: string, type?: string, org_id?: string, org_domain?: string, context?: string
  }
  relationships?: { referenced_by?: { data?: Array<{ type?: string, id?: string }> } }
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

function partial(error: unknown, domain: string): CliError {
  const message = error instanceof CliError ? error.message : 'Seed API operation failed'
  return new CliError(`${message}. Instance ${domain} may be partial; ` +
    'run explicit cleanup only after server writes have stopped (a timeout/disconnect is not proof).')
}

async function main(): Promise<void> {
  const [command, suppliedDomain, ...extra] = process.argv.slice(2)
  check(['seed', 'cleanup', 'seed-org', 'cleanup-org'].includes(command ?? '') && !extra.length &&
    (!command?.startsWith('cleanup') || suppliedDomain),
  'Usage: seed[-org] [domain] | cleanup[-org] <domain>; credentials belong in environment variables')
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
  if (command === 'cleanup') {
    const deleted = await admin('DELETE', instancePath)
    check(deleted.status === 204 || deleted.status === 404, `Cleanup failed: HTTP ${deleted.status}`)
    const readback = await admin('GET', instancePath)
    check(readback.status === 404, `Cleanup absence not confirmed: HTTP ${readback.status}`)
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
      const remove = async (item: Resource) => {
        const path = `/instances/${item.attributes!.domain!}`
        const current = resource(expectStatus(await admin('GET', path), 200))
        checkScope(current)
        check(same(item, current), 'Cleanup readback identity mismatch')
        const deleted = await admin('DELETE', path)
        check(deleted.status === 204 || deleted.status === 404, `Cleanup failed: HTTP ${deleted.status}`)
        expectStatus(await admin('GET', path), 404)
      }
      for (const item of candidates) if (item !== listedRoot) await remove(item)
      check((await list()).filter(inScope).every((item) => item.attributes?.domain === domain),
        'Organization members remain; root retained')
      if (root) await remove(root)
      expectStatus(await admin('GET', instancePath), 404)
      check(!(await list()).some(inScope), 'Organization scope remains after cleanup')
      console.log(`Cleanup complete: ${domain}`)
    } catch (error) { throw partial(error, domain) }
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
  const create = async (name: string): Promise<Resource> => {
    const uuid = randomUUID()
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
  const populate = async (domain: string): Promise<void> => {
    if (!files && !drives) return
    check(stackURL, 'SEED_STACK_URL is required for population')
    if (organization) console.log(`Seed owner: ${domain}; files=${files}; drives=${drives}`)
    const accessToken = expectStatus(await admin('POST', '/instances/token', {
      Domain: domain, Audience: 'cli', Scope: 'io.cozy.files io.cozy.sharings',
    }), 200).body
    check(accessToken && !/[\s\x00-\x1f\x7f]/.test(accessToken), 'Invalid CLI token')
    const publicAPI = (method: string, path: string, body: string | Buffer = '', headers = {}) =>
      request(new URL(path, stackURL), method, {
        ...headers, Host: domain, Authorization: `Bearer ${accessToken}`,
      }, timeout, body)
    const body = Buffer.alloc(1024, 's')
    const md5 = createHash('md5').update(body).digest('base64')
    for (let index = 1; index <= files; index++) {
      const query = new URLSearchParams({ Type: 'file', Name: `seed-file-${index}.txt` })
      resource(expectStatus(await publicAPI('POST', `/files/io.cozy.files.root-dir?${query}`, body, {
        'Content-Type': 'text/plain', 'Content-MD5': md5,
      }), 201))
    }
    for (let index = 1; index <= drives; index++) {
      const drive = await createSharedDrive({
        baseUrl: stackURL.origin, accessToken, driveName: `Seed shared drive ${index}`,
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
      const root = resource(expectStatus(await publicAPI('GET',
        `/files/${encodeURIComponent(drive.rootDirectoryId)}`), 200))
      const refs = root.relationships?.referenced_by?.data
      const backlink = Array.isArray(refs) && refs.some((ref) =>
        ref?.type === 'io.cozy.sharings' && ref.id === drive.sharingId)
      check(root.id === drive.rootDirectoryId && root.type === 'io.cozy.files' &&
        root.attributes?.type === 'directory' && backlink, 'Shared-drive root or backlink mismatch')
    }
  }
  try {
    for (const name of names) {
      const absent = await admin('GET', `/instances/${name}`)
      check(absent.status === 404, absent.status === 200 ? 'Instance already exists' :
        `Instance lookup failed: HTTP ${absent.status}`)
    }
    if (organization) check(!(await list()).some(inScope), 'Organization scope already exists')
    const created: Resource[] = []
    for (const name of names) {
      created.push(await create(name))
      if (!organization || name !== domain) await populate(name)
    }
    if (organization) {
      const members = (await list()).filter(inScope)
      check(members.length === created.length && created.every((expected) =>
        members.some((item) => same(item, expected))), 'Organization membership mismatch')
    }
    const counts = `${organization ? `users=${users}; ` : ''}files=${files}; drives=${drives}`
    console.log(`Seed complete: ${domain}; ${counts}`)
  } catch (error) { throw partial(error, domain) }
}

main().catch((error: unknown) => {
  console.error(error instanceof CliError ? error.message : 'Seed command failed')
  process.exitCode = 1
})
