// The search load test (README, Search load test), on an organization fixture (ACTORS_DOMAIN).
// prepare: the organization creates the drives and a big drive of many folders, shares them with groups
// of members through contacts groups (members accept on their own), gives each member its own files,
// turns search on and waits until every member finds its last drive and its own files. run: mints the
// members' tokens and runs k6 (make search-load-run) at a fixed rate per group.
import { spawnSync } from 'node:child_process'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'

import { stackAddress } from './drive-actors.ts'
import { request, type Reply } from './fixture-http.ts'
import { manifestPath, readManifest } from './fixture-manifest.ts'
import { check, harness, parse } from './upload/exact-work.ts'

type Api = (method: string, path: string, body?: string, type?: string) => Promise<Reply>
// group is the drive count, or big for the members who hold only the big drive (drives 0).
interface User { domain: string, uuid: string, drives: number, group: string }

const env = process.env
const REQUEST_MS = 60_000
const OWNER_SCOPE = 'io.cozy.files io.cozy.sharings io.cozy.contacts io.cozy.contacts.groups'
// The words of the file names; the k6 scenario searches for them and for their starts.
const WORDS = ['report', 'budget', 'invoice', 'meeting', 'notes', 'plan', 'photo', 'contract', 'draft', 'summary']

function setting(name: string): string {
  const value = env[name]
  check(value, `${name} is required for the search load test`)
  return value
}
function integer(name: string, fallback: number): number {
  const value = Number(env[name] || fallback)
  check(Number.isSafeInteger(value) && value > 0, `${name} must be a positive whole number`)
  return value
}

const domain = setting('ACTORS_DOMAIN')
const prepared = `data/results/search-load-${domain.split('.')[0]}.json`
const adminURL = new URL(setting('SEED_ADMIN_URL'))
const stackOrigin = setting('SEED_STACK_URL')
const auth = `Basic ${Buffer.from(`${env.SEED_ADMIN_USERNAME || 'admin'}:${setting('SEED_ADMIN_PASSWORD')}`)
  .toString('base64')}`
const admin = (method: string, path: string) => request(new URL(path, adminURL), method, { Authorization: auth },
  REQUEST_MS)
const as = (host: string, token: string): Api => (method, path, body = '', type = 'application/json') =>
  request(new URL(path, stackOrigin), method, {
    Host: host, Authorization: `Bearer ${token}`, Accept: 'application/json', ...(body ? { 'Content-Type': type } : {}),
  }, REQUEST_MS, body)
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms))

// The Stack limits a member to 30 sharing shortcuts an hour (push jobs), then falls back to mails, 200 an
// hour per owner: a member of 200 drives would wait hours. The bundled Stack's counters (Redis database
// rate_limiting in config/cozy/cozy.yaml) are reset before each batch of grants.
const GRANT_BATCH = 25
function resetRateLimits(): void {
  const reset = spawnSync('docker', ['compose', 'exec', '-T', 'redis', 'redis-cli', '-n', '8', 'FLUSHDB'],
    { cwd: harness, encoding: 'utf8' })
  check(reset.status === 0 && reset.stdout.trim() === 'OK', `Cannot reset the Stack's rate limits: ${reset.stderr}`)
}

async function expect(reply: Promise<Reply>, status: number, what: string): Promise<any> {
  const { status: got, body } = await reply
  check(got === status, `${what}: HTTP ${got} ${body.slice(0, 200)}`)
  return body ? parse(body) : null
}
async function token(host: string, scope: string): Promise<string> {
  const minted = await admin('POST', `/instances/token?${new URLSearchParams({ Domain: host, Audience: 'cli', Scope: scope })}`)
  check(minted.status === 200 && minted.body && !/\s/.test(minted.body), `No token for ${host}: HTTP ${minted.status}`)
  return minted.body
}
// Runs the tasks a few at a time: the Stack and the Mac are shared with the rest of the fixture.
async function pool(tasks: Array<() => Promise<void>>, concurrency = 8): Promise<void> {
  const queue = [...tasks]
  await Promise.all(Array.from({ length: concurrency }, async () => {
    for (let task = queue.shift(); task; task = queue.shift()) await task()
  }))
}
async function until(what: string, timeoutMs: number, done: () => Promise<boolean>): Promise<void> {
  for (const deadline = Date.now() + timeoutMs; !(await done()); await sleep(1000)) {
    check(Date.now() < deadline, `${what} did not happen within ${timeoutMs / 1000} s`)
  }
}
const mkdir = async (api: Api, dirID: string, name: string): Promise<string> =>
  String((await expect(api('POST', `/files/${dirID}?Type=directory&Name=${encodeURIComponent(name)}`), 201,
    `Creating ${name}`)).data.id)
const upload = (api: Api, dirID: string, name: string) =>
  expect(api('POST', `/files/${dirID}?Type=file&Name=${encodeURIComponent(name)}`, name, 'text/plain'), 201, `Uploading ${name}`)
const finds = async (api: Api, name: string) => {
  const reply = await api('POST', '/files/search?page[limit]=100', JSON.stringify({ q: name }))
  return reply.status === 200 && (parse(reply.body)?.data ?? []).some((hit: any) => hit.attributes?.name === name)
}

async function prepare(): Promise<void> {
  check(!existsSync(prepared), `${prepared} exists: this fixture is prepared already (make seed-org-cleanup, then seed-org)`)
  const levels = (env.SEARCH_LEVELS || '1,10,200').split(',').map(Number)
  check(levels.every((l, i) => Number.isSafeInteger(l) && l > (levels[i - 1] ?? 0)), 'SEARCH_LEVELS must be increasing drive counts')
  const perLevel = integer('SEARCH_USERS_PER_LEVEL', 5)
  const filesPerDrive = integer('SEARCH_FILES_PER_DRIVE', 20)
  const ownFiles = integer('SEARCH_OWN_FILES', 50)
  const bigFolders = integer('SEARCH_BIG_DRIVE_FOLDERS', 50_000)
  const settleMs = integer('SEARCH_SETTLE_MS', 1_800_000)
  const manifest = readManifest(env.SEED_MANIFEST || manifestPath(domain), domain)
  const members = manifest.instances.filter(i => i.role === 'member')
  check(manifest.status === 'complete' && manifest.organization, `${domain} is not a complete organization fixture`)
  const needed = (levels.length + 1) * perLevel
  check(members.length >= needed, `The fixture needs ${needed} members: ` +
    `make seed-org SEED_USERS=${needed} SEED_CONTEXT_NAME=load-org-sharing`)
  const org = manifest.instances[0]!
  const users: User[] = members.slice(0, needed).map((m, i) => {
    const drives = levels[Math.floor(i / perLevel)] ?? 0
    return { domain: m.domain, uuid: m.uuid, drives, group: drives ? String(drives) : 'big' }
  })

  // First, so that the index jobs follow the fixture as it is built; 503 when the Stack has no search.
  for (const host of [org.domain, ...users.map(u => u.domain)]) {
    await expect(admin('POST', `/instances/${host}/search/enable`), 204, `Turning search on for ${host} (make search-up?)`)
  }
  const owner = as(org.domain, await token(org.domain, OWNER_SCOPE))
  const createDrive = async (name: string) => {
    const drive = (await expect(owner('POST', '/sharings/drives', JSON.stringify({ data: {
      type: 'io.cozy.sharings', attributes: { name, description: name } } }), 'application/vnd.api+json'),
    201, 'Creating a drive')).data
    return { id: String(drive.id), root: String(drive.attributes.rules[0].values[0]) }
  }
  const drives: Array<{ id: string, root: string }> = []
  await pool(Array.from({ length: levels.at(-1)! }, (_, i) => async () => {
    drives[i] = await createDrive(`search-load-${i}`)
    await upload(owner, drives[i]!.root, `marker-d${i}.txt`)
    for (let j = 0; j < filesPerDrive; j++) await upload(owner, drives[i]!.root, `${WORDS[j % WORDS.length]} ${i}-${j}.txt`)
  }))

  // The search sends every folder key of the caller's drives to OpenSearch (ADR 008): the big drive has
  // bigFolders folders, 100 under its root and the others under them, and files in some of them.
  const big = await createDrive('search-load-big')
  const folders: string[] = []
  await pool(Array.from({ length: Math.min(100, bigFolders) }, (_, i) => async () => {
    folders[i] = await mkdir(owner, big.root, `folder ${i}`)
  }))
  await pool(Array.from({ length: bigFolders - folders.length }, (_, i) => async () => {
    await mkdir(owner, folders[i % folders.length]!, `folder ${i % folders.length}-${i}`)
  }))
  for (let j = 0; j < filesPerDrive; j++) {
    await upload(owner, folders[j % folders.length]!, `${WORDS[j % WORDS.length]} big-${j}.txt`)
  }
  // After the folders: once a member finds it, the scope of the drive has all of them.
  await upload(owner, big.root, 'marker-big.txt')

  // One contacts group per group of members; drive i goes to the groups of more than i drives, in one
  // grant, and the big drive to the big group.
  const groups = new Map<string, { id: string, type: string }>()
  for (const name of new Set(users.map(u => u.group))) {
    const group = await expect(owner('POST', '/data/io.cozy.contacts.groups/',
      JSON.stringify({ name: `search-load-${name}` })), 201, 'Creating a group')
    groups.set(name, { id: String(group.id), type: 'io.cozy.contacts.groups' })
    for (const user of users.filter(u => u.group === name)) {
      await expect(owner('POST', '/data/io.cozy.contacts/', JSON.stringify({
        fullname: `Search load ${user.uuid}`, email: [{ address: `${user.uuid}@spam.cozycloud.cc`, primary: true }],
        cozy: [{ url: `${new URL(stackOrigin).protocol}//${user.domain}/`, primary: true }],
        relationships: { groups: { data: [{ _id: group.id, _type: 'io.cozy.contacts.groups' }] } },
      })), 201, 'Creating a contact')
    }
  }
  const grant = (drive: { id: string }, recipients: Array<{ id: string, type: string }>) =>
    expect(owner('POST', `/sharings/${drive.id}/recipients`, JSON.stringify({ data: {
      type: 'io.cozy.sharings', id: drive.id, relationships: { recipients: { data: recipients } } } }),
    'application/vnd.api+json'), 200, 'Granting a drive')
  for (let start = 0; start < drives.length; start += GRANT_BATCH) {
    resetRateLimits()
    await pool(drives.slice(start, start + GRANT_BATCH).map((drive, k) => async () => {
      await grant(drive, levels.filter(level => level > start + k).map(level => groups.get(String(level))!))
    }))
  }
  await grant(big, [groups.get('big')!])

  await pool(users.map(user => async () => {
    const api = as(user.domain, await token(user.domain, 'io.cozy.files'))
    await upload(api, 'io.cozy.files.root-dir', 'marker-own.txt')
    for (let k = 0; k < ownFiles; k++) await upload(api, 'io.cozy.files.root-dir', `${WORDS[k % WORDS.length]} own-${k}.txt`)
  }))
  await pool([...drives, big].map((drive, i) => async () => {
    const expected = drive === big ? users.filter(u => u.group === 'big').length : users.filter(u => u.drives > i).length
    await until(`The members accepting drive ${drive === big ? 'big' : i}`, settleMs, async () => {
      const members = parse((await owner('GET', `/sharings/${drive.id}`)).body)?.data?.attributes?.members ?? []
      const ready = members.slice(1).filter((m: { status?: string }) => m.status === 'ready').length
      return ready === expected
    })
  }))
  await pool(users.map(user => async () => {
    const api = as(user.domain, await token(user.domain, 'io.cozy.files'))
    const lastDrive = user.drives ? `marker-d${user.drives - 1}.txt` : 'marker-big.txt'
    await until(`${user.domain} finding its files`, settleMs, async () =>
      await finds(api, lastDrive) && await finds(api, 'marker-own.txt'))
  }))
  writeFileSync(prepared, `${JSON.stringify({ organization: org.domain, bigFolders, users }, null, 2)}\n`)
  console.log(`Search fixture ready: ${drives.length} drives and one of ${bigFolders} folders, ` +
    `${users.length} members, in ${prepared}`)
}

async function run(): Promise<void> {
  check(existsSync(prepared), `${prepared} is missing: run make search-load-prepare first`)
  const { users } = JSON.parse(readFileSync(prepared, 'utf8')) as { users: User[] }
  const address = stackAddress()
  const scheme = new URL(stackOrigin).protocol
  const tokens = await Promise.all(users.map(async u => ({
    url: `${scheme}//${u.domain}`, token: await token(u.domain, 'io.cozy.files'), group: u.group })))
  const hosts = users.map(u => `${new URL(`${scheme}//${u.domain}`).hostname}=${address}`).join(',')
  const k6 = spawnSync('make', ['search-load-run', `ACTOR_HOSTS=${hosts}`, `EXECUTION_ID=${env.EXECUTION_ID || 'search'}`],
    { cwd: harness, stdio: 'inherit', env: { ...env, SEARCH_USERS: JSON.stringify(tokens) } })
  process.exitCode = k6.status ?? 2
}

const command = process.argv[2]
check(command === 'prepare' || command === 'run', 'Usage: search-load.ts prepare|run')
await (command === 'prepare' ? prepare() : run())
