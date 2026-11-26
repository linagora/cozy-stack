// The search load test (README, Search load test), on an organization fixture (ACTORS_DOMAIN).
// prepare: the organization creates the drives, shares them with groups of members through contacts
// groups (members accept on their own), gives each member its own files, turns search on and waits until
// every member finds its last drive and its own files. run: mints the members' tokens and runs k6
// (make search-load-run) at a fixed rate per drive count.
import { spawnSync } from 'node:child_process'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'

import { stackAddress } from './drive-actors.ts'
import { request, type Reply } from './fixture-http.ts'
import { manifestPath, readManifest } from './fixture-manifest.ts'
import { check, harness, parse } from './upload/exact-work.ts'

type Api = (method: string, path: string, body?: string, type?: string) => Promise<Reply>
interface User { domain: string, uuid: string, drives: number }

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
  const settleMs = integer('SEARCH_SETTLE_MS', 1_800_000)
  const manifest = readManifest(env.SEED_MANIFEST || manifestPath(domain), domain)
  const members = manifest.instances.filter(i => i.role === 'member')
  check(manifest.status === 'complete' && manifest.organization, `${domain} is not a complete organization fixture`)
  check(members.length >= levels.length * perLevel, `The fixture needs ${levels.length * perLevel} members: ` +
    `make seed-org SEED_USERS=${levels.length * perLevel} SEED_CONTEXT_NAME=load-org-sharing`)
  const org = manifest.instances[0]!
  const users: User[] = members.slice(0, levels.length * perLevel)
    .map((m, i) => ({ domain: m.domain, uuid: m.uuid, drives: levels[Math.floor(i / perLevel)]! }))

  // First, so that the index jobs follow the fixture as it is built; 503 when the Stack has no search.
  for (const host of [org.domain, ...users.map(u => u.domain)]) {
    await expect(admin('POST', `/instances/${host}/search/enable`), 204, `Turning search on for ${host} (make search-up?)`)
  }
  const owner = as(org.domain, await token(org.domain, OWNER_SCOPE))
  const drives: Array<{ id: string, root: string }> = []
  await pool(Array.from({ length: levels.at(-1)! }, (_, i) => async () => {
    const name = `search-load-${i}`
    const drive = (await expect(owner('POST', '/sharings/drives', JSON.stringify({ data: {
      type: 'io.cozy.sharings', attributes: { name, description: name } } }), 'application/vnd.api+json'),
    201, 'Creating a drive')).data
    drives[i] = { id: String(drive.id), root: String(drive.attributes.rules[0].values[0]) }
    await upload(owner, drives[i]!.root, `marker-d${i}.txt`)
    for (let j = 0; j < filesPerDrive; j++) await upload(owner, drives[i]!.root, `${WORDS[j % WORDS.length]} ${i}-${j}.txt`)
  }))

  // One contacts group per drive count; drive i goes to the groups of more than i drives, in one grant.
  const groups = new Map<number, { id: string, type: string }>()
  for (const level of levels) {
    const group = await expect(owner('POST', '/data/io.cozy.contacts.groups/',
      JSON.stringify({ name: `search-load-${level}` })), 201, 'Creating a group')
    groups.set(level, { id: String(group.id), type: 'io.cozy.contacts.groups' })
    for (const user of users.filter(u => u.drives === level)) {
      await expect(owner('POST', '/data/io.cozy.contacts/', JSON.stringify({
        fullname: `Search load ${user.uuid}`, email: [{ address: `${user.uuid}@spam.cozycloud.cc`, primary: true }],
        cozy: [{ url: `${new URL(stackOrigin).protocol}//${user.domain}/`, primary: true }],
        relationships: { groups: { data: [{ _id: group.id, _type: 'io.cozy.contacts.groups' }] } },
      })), 201, 'Creating a contact')
    }
  }
  for (let start = 0; start < drives.length; start += GRANT_BATCH) {
    resetRateLimits()
    await pool(drives.slice(start, start + GRANT_BATCH).map((drive, k) => async () => {
      const recipients = levels.filter(level => level > start + k).map(level => groups.get(level)!)
      await expect(owner('POST', `/sharings/${drive.id}/recipients`, JSON.stringify({ data: {
        type: 'io.cozy.sharings', id: drive.id, relationships: { recipients: { data: recipients } } } }),
      'application/vnd.api+json'), 200, 'Granting a drive')
    }))
  }

  await pool(users.map(user => async () => {
    const api = as(user.domain, await token(user.domain, 'io.cozy.files'))
    await upload(api, 'io.cozy.files.root-dir', 'marker-own.txt')
    for (let k = 0; k < ownFiles; k++) await upload(api, 'io.cozy.files.root-dir', `${WORDS[k % WORDS.length]} own-${k}.txt`)
  }))
  await pool(drives.map((drive, i) => async () => {
    const expected = users.filter(u => u.drives > i).length
    await until(`The members accepting drive ${i}`, settleMs, async () => {
      const members = parse((await owner('GET', `/sharings/${drive.id}`)).body)?.data?.attributes?.members ?? []
      const ready = members.slice(1).filter((m: { status?: string }) => m.status === 'ready').length
      return ready === expected
    })
  }))
  await pool(users.map(user => async () => {
    const api = as(user.domain, await token(user.domain, 'io.cozy.files'))
    await until(`${user.domain} finding its files`, settleMs, async () =>
      await finds(api, `marker-d${user.drives - 1}.txt`) && await finds(api, 'marker-own.txt'))
  }))
  writeFileSync(prepared, `${JSON.stringify({ organization: org.domain, users }, null, 2)}\n`)
  console.log(`Search fixture ready: ${drives.length} drives, ${users.length} members, in ${prepared}`)
}

async function run(): Promise<void> {
  check(existsSync(prepared), `${prepared} is missing: run make search-load-prepare first`)
  const { users } = JSON.parse(readFileSync(prepared, 'utf8')) as { users: User[] }
  const address = stackAddress()
  const scheme = new URL(stackOrigin).protocol
  const tokens = await Promise.all(users.map(async u => ({
    url: `${scheme}//${u.domain}`, token: await token(u.domain, 'io.cozy.files'), drives: u.drives })))
  const hosts = users.map(u => `${new URL(`${scheme}//${u.domain}`).hostname}=${address}`).join(',')
  const k6 = spawnSync('make', ['search-load-run', `ACTOR_HOSTS=${hosts}`, `EXECUTION_ID=${env.EXECUTION_ID || 'search'}`],
    { cwd: harness, stdio: 'inherit', env: { ...env, SEARCH_USERS: JSON.stringify(tokens) } })
  process.exitCode = k6.status ?? 2
}

const command = process.argv[2]
check(command === 'prepare' || command === 'run', 'Usage: search-load.ts prepare|run')
await (command === 'prepare' ? prepare() : run())
