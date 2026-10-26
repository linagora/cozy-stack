import { deepEqual, equal, notDeepEqual, ok } from 'node:assert/strict'
import { test } from 'node:test'

import { TREE, treeContent, treeDigest, treeFiles } from './dataset-profile.ts'

test('tree content depends only on the seed and path, at the exact size', () => {
  equal(treeContent('1', 'a.bin', 1000).length, 1000)
  deepEqual(treeContent('1', 'a.bin', 100), treeContent('1', 'a.bin', 100))
  notDeepEqual(treeContent('1', 'a.bin', 100), treeContent('2', 'a.bin', 100))
  notDeepEqual(treeContent('1', 'a.bin', 100), treeContent('1', 'b.bin', 100))
  // A shorter file is a prefix of a longer one with the same seed and path.
  deepEqual(treeContent('1', 'a.bin', 40), treeContent('1', 'a.bin', 100).subarray(0, 40))
})

test('the tree profile spans depth, sizes and a drive, and digests the same for a seed', () => {
  ok(Math.max(...TREE.dirs.map(d => d.split('/').length)) >= 3)
  ok(new Set(TREE.files.map(f => f.size)).size >= 5)
  ok(TREE.files.some(f => f.path.startsWith('@drive/')))
  equal(treeDigest(treeFiles('7')), treeDigest(treeFiles('7')))
  ok(treeDigest(treeFiles('7')) !== treeDigest(treeFiles('8')))
})
