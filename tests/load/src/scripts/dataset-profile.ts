// The tree profile (SEEDING.md, Profiles): a small, versioned dataset with directory depth, several
// file sizes and a shared drive holding part of it. The same generation seed always gives the same
// structure and bytes; resource IDs belong to each run. Paths under @drive live in the drive.
import { createHash } from 'node:crypto'
import { posix } from 'node:path'

export const TREE_VERSION = 1
export const TREE_DRIVE = '@drive'
export const TREE = {
  dirs: ['projects', 'projects/alpha', 'projects/alpha/specs', 'archive', `${TREE_DRIVE}/handbook`],
  files: [
    { path: 'readme.txt', size: 512 },
    { path: 'projects/plan.txt', size: 4 << 10 },
    { path: 'projects/alpha/notes.txt', size: 16 << 10 },
    { path: 'projects/alpha/specs/design.bin', size: 256 << 10 },
    { path: 'archive/2025.bin', size: 1 << 20 },
    { path: `${TREE_DRIVE}/handbook/welcome.txt`, size: 2 << 10 },
    { path: `${TREE_DRIVE}/handbook/policies.bin`, size: 128 << 10 },
  ],
} as const

export interface TreeFile { path: string, size: number, md5: string }

// Counter-mode SHA-256 of the seed and path: deterministic, and cheap at these sizes.
export function treeContent(seed: string, path: string, size: number): Buffer {
  const blocks: Buffer[] = []
  for (let index = 0; blocks.length * 32 < size; index++) {
    blocks.push(createHash('sha256').update(`${seed}\0${path}\0${index}`).digest())
  }
  return Buffer.concat(blocks).subarray(0, size)
}

export function treeFiles(seed: string): TreeFile[] {
  return TREE.files.map(({ path, size }) => ({
    path, size, md5: createHash('md5').update(treeContent(seed, path, size)).digest('base64'),
  }))
}

// One checksum for the whole dataset, recorded as the manifest's expected content.
export function treeDigest(files: readonly TreeFile[]): string {
  return createHash('md5').update(files.map(f => `${f.path}:${f.md5}`).join('\n')).digest('base64')
}

export const parentOf = (path: string) => posix.dirname(path)
export const nameOf = (path: string) => posix.basename(path)
