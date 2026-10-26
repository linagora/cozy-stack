// The fixture manifest records what a seed planned and confirmed, so verification and cleanup act
// only on recorded identities. It never holds tokens. See SEEDING.md.
import { mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { dirname } from 'node:path'

import { FixtureError } from './fixture-http.ts'

export interface ManifestInstance {
  domain: string
  uuid: string
  role: 'owner' | 'organization' | 'member'
  // False until the admin API confirmed it: after a lost creation reply only the planned UUID is known.
  created: boolean
  files: string[]
  drives: Array<{ sharing: string, root: string }>
  // Entries in the root directory once populated, to reconcile the fixture after a run.
  root_entries?: number
}
export interface FixtureManifest {
  version: 1
  // The flat dataset: identical files at the root and empty owner drives.
  profile: 'flat'
  status: 'preparing' | 'complete' | 'failed'
  domain: string
  organization: string | null
  parameters: { users: number, files: number, drives: number, context: string | null }
  content: { bytes: number, md5: string }
  instances: ManifestInstance[]
}

const id = (value: unknown) => typeof value === 'string' && value.length > 0 && !/[\s\x00-\x1f\x7f]/.test(value)
const count = (value: unknown) => Number.isSafeInteger(value) && (value as number) >= 0

export function manifestPath(domain: string): string {
  return process.env.SEED_MANIFEST || `data/results/${domain.split('.')[0]}-fixture.json`
}

// The first write refuses an existing file: it may record another fixture's uncertain writes.
export function writeManifest(path: string, manifest: FixtureManifest, create = false): void {
  const text = `${JSON.stringify(manifest, null, 2)}\n`
  mkdirSync(dirname(path), { recursive: true })
  if (create) {
    try { return writeFileSync(path, text, { flag: 'wx', mode: 0o600 }) } catch {
      throw new FixtureError(`Cannot create ${path}; an existing manifest may record another fixture`)
    }
  }
  writeFileSync(`${path}.tmp`, text, { mode: 0o600 })
  renameSync(`${path}.tmp`, path)
}

export function removeManifest(path: string): void {
  rmSync(path)
}

export function readManifest(path: string, domain: string): FixtureManifest {
  let manifest: FixtureManifest
  try { manifest = JSON.parse(readFileSync(path, 'utf8')) } catch {
    throw new FixtureError(`No readable fixture manifest at ${path}`)
  }
  const p = manifest?.parameters
  const valid = manifest?.version === 1 && manifest.profile === 'flat' &&
    ['preparing', 'complete', 'failed'].includes(manifest.status) && manifest.domain === domain &&
    (manifest.organization === null || manifest.organization === domain.split('.')[0]) &&
    count(p?.users) && count(p.files) && count(p.drives) && (p.context === null || id(p.context)) &&
    count(manifest.content?.bytes) && id(manifest.content.md5) && Array.isArray(manifest.instances) &&
    manifest.instances[0]?.domain === domain && manifest.instances.every(i => id(i?.domain) && id(i.uuid) &&
      ['owner', 'organization', 'member'].includes(i.role) && typeof i.created === 'boolean' &&
      Array.isArray(i.files) && i.files.every(id) && Array.isArray(i.drives) &&
      i.drives.every(d => id(d?.sharing) && id(d.root)) && (i.root_entries === undefined || count(i.root_entries)))
  if (!valid) throw new FixtureError(`${path} is not the fixture manifest of ${domain}`)
  return manifest
}
