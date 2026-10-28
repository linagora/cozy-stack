// Writes and verifies the tree profile (dataset-profile.ts) in one instance through its public API, with
// that instance's token: the local seeder (seed-scale-data.ts) and the Cloudery one (seed-cloudery.ts)
// share it.
import { createHash } from 'node:crypto'

import { nameOf, parentOf, TREE, TREE_DRIVE, treeContent, treeFiles } from './dataset-profile.ts'
import { FixtureError, type Reply } from './fixture-http.ts'
import type { ManifestInstance } from './fixture-manifest.ts'
import { createSharedDrive } from './prepare-shared-drive.ts'

function check(value: unknown, message: string): asserts value {
  if (!value) throw new FixtureError(message)
}

const ROOT = 'io.cozy.files.root-dir'

// One request to the instance: the API sets its origin, Host and token.
export type PublicAPI = (method: string, path: string, body?: string | Buffer, headers?: Record<string, string>) =>
  Promise<Reply>

export interface Resource {
  id: string
  type?: string
  attributes?: {
    domain?: string, uuid?: string, type?: string, org_id?: string, org_domain?: string, context?: string,
    md5sum?: string, size?: string, dir_id?: string, name?: string
  }
  relationships?: {
    referenced_by?: { data?: Array<{ type?: string, id?: string }> }, contents?: { meta?: { count?: number } }
  }
}

export function expectStatus(reply: Reply, status: number): Reply {
  check(reply.status === status, `Unexpected API status: HTTP ${reply.status}`)
  return reply
}

export function resource(reply: Reply): Resource {
  let data: Resource | undefined
  try { data = JSON.parse(reply.body)?.data } catch { throw new FixtureError('Invalid API JSON') }
  check(data && typeof data.id === 'string' && data.id.trim(), 'Invalid API resource')
  return data
}

export async function checkDriveRoot(api: PublicAPI, root: string, sharing: string): Promise<void> {
  const dir = resource(expectStatus(await api('GET', `/files/${encodeURIComponent(root)}`), 200))
  const refs = dir.relationships?.referenced_by?.data
  const backlink = Array.isArray(refs) && refs.some((ref) => ref?.type === 'io.cozy.sharings' && ref.id === sharing)
  check(dir.id === root && dir.type === 'io.cozy.files' && dir.attributes?.type === 'directory' && backlink,
    'Shared-drive root or backlink mismatch')
}

// The Stack counts root entries without the trash.
export async function rootEntries(api: PublicAPI): Promise<number> {
  const root = resource(expectStatus(await api('GET', '/files/io.cozy.files.root-dir?page[limit]=1'), 200))
  const entries = root.relationships?.contents?.meta?.count
  check(Number.isSafeInteger(entries) && entries! >= 0, 'Invalid root directory count')
  return entries!
}

// Creates a shared drive, records it in entry and returns its root.
export async function addDrive(api: PublicAPI, entry: ManifestInstance, driveName: string): Promise<string> {
  const drive = await createSharedDrive({
    // The API routes by path and authenticates every request itself.
    baseUrl: 'http://instance', accessToken: '', driveName,
    fetcher: async (input, init) => {
      const headers = Object.fromEntries(new Headers(init?.headers))
      const path = new URL(String(input)).pathname
      const reply = expectStatus(await api(init?.method ?? 'GET', path, String(init?.body ?? ''), headers), 201)
      return new Response(reply.body, { status: reply.status })
    },
  })
  check(typeof drive.sharingId === 'string' && drive.sharingId.trim() &&
    typeof drive.rootDirectoryId === 'string' && drive.rootDirectoryId.trim(), 'Invalid shared-drive identity')
  await checkDriveRoot(api, drive.rootDirectoryId, drive.sharingId)
  entry.drives.push({ sharing: drive.sharingId, root: drive.rootDirectoryId })
  return drive.rootDirectoryId
}

// Writes the tree, recording each ID in entry as it is confirmed; returns the bytes uploaded.
export async function writeTree(api: PublicAPI, seed: string, entry: ManifestInstance): Promise<number> {
  const add = async (parent: string, query: Record<string, string>, content: Buffer | '' = '') => resource(
    expectStatus(await api('POST', `/files/${encodeURIComponent(parent)}?${new URLSearchParams(query)}`,
      content, content ? { 'Content-Type': 'application/octet-stream',
        'Content-MD5': createHash('md5').update(content).digest('base64') } : {}), 201)).id
  const parents = new Map<string, string>([['.', ROOT], [TREE_DRIVE, await addDrive(api, entry, 'Seed tree drive')]])
  entry.dirs ??= []
  for (const path of TREE.dirs) {
    const id = await add(parents.get(parentOf(path))!, { Type: 'directory', Name: nameOf(path) })
    parents.set(path, id)
    entry.dirs.push(id)
  }
  let uploaded = 0
  for (const { path, size } of TREE.files) {
    const content = treeContent(seed, path, size)
    entry.files.push(await add(parents.get(parentOf(path))!, { Type: 'file', Name: nameOf(path) }, content))
    uploaded += size
  }
  entry.root_entries = await rootEntries(api)
  return uploaded
}

// Every directory and file sits under its profile parent, with its name, size and bytes; the drive keeps
// its root and the root its recorded entry count.
export async function verifyTree(api: PublicAPI, seed: string, entry: ManifestInstance): Promise<void> {
  const read = async (id: string) => resource(expectStatus(await api('GET', `/files/${encodeURIComponent(id)}`), 200))
  const dirs = entry.dirs ?? []
  check(dirs.length === TREE.dirs.length && entry.drives.length === 1, `${entry.domain} lacks recorded directories`)
  const parents = new Map<string, string>([['.', ROOT], [TREE_DRIVE, entry.drives[0]!.root]])
  TREE.dirs.forEach((path, index) => parents.set(path, dirs[index]!))
  for (const [index, path] of TREE.dirs.entries()) {
    const dir = await read(dirs[index]!)
    check(dir.attributes?.type === 'directory' && dir.attributes.name === nameOf(path) &&
      dir.attributes.dir_id === parents.get(parentOf(path)), `${entry.domain}: directory ${path} changed`)
  }
  for (const [index, expected] of treeFiles(seed).entries()) {
    const file = await read(entry.files[index]!)
    const parent = parents.get(parentOf(expected.path))
    check(file.attributes?.md5sum === expected.md5 && Number(file.attributes.size) === expected.size &&
      file.attributes.name === nameOf(expected.path) && file.attributes.dir_id === parent,
    `${entry.domain}: file ${expected.path} changed`)
  }
  await checkDriveRoot(api, entry.drives[0]!.root, entry.drives[0]!.sharing)
  const entries = await rootEntries(api)
  check(entries === entry.root_entries,
    `${entry.domain}: the root holds ${entries} entries; the manifest records ${entry.root_entries}`)
}
