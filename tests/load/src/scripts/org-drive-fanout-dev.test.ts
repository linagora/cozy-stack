import { deepEqual, equal } from 'node:assert/strict'
import { performance } from 'node:perf_hooks'
import { test } from 'node:test'

import { removeDrive } from './org-drive-fanout-dev.ts'

// Answers like the organization instance: the roster changes on each of the first reads.
function owner(rosters: string[][]) {
  const calls: string[] = []
  const times: number[] = []
  let reads = 0
  const api = async (method: string, path: string) => {
    calls.push(`${method} ${path}`)
    times.push(performance.now())
    if (method === 'GET' && path === '/sharings/s1') {
      const statuses = rosters[Math.min(reads++, rosters.length - 1)]!
      return { status: 200, body: JSON.stringify({ data: { attributes: { members: statuses.map(status => ({ status })) } } }) }
    }
    if (method === 'GET') return { status: calls.includes('DELETE /files/r1') ? 404 : 200, body: '{}' }
    return { status: path.startsWith('/files/trash/') ? 204 : 200, body: '{}' }
  }
  return { api, calls, times }
}

test('a drive is trashed only once its roster has stopped changing', async () => {
  const { api, calls, times } = owner([['owner', 'pending'], ['owner', 'ready'], ['owner', 'revoked']])
  equal(await removeDrive(api, 's1', 'r1', 30, 5, performance.now() + 5_000), true)
  const firstDelete = calls.indexOf('DELETE /files/r1')
  // The third read shows the last roster; it must hold for the 30 ms stable window first.
  equal(times[firstDelete]! - times[2]! >= 30, true)
  deepEqual(calls.slice(firstDelete), ['DELETE /files/r1', 'DELETE /files/trash/r1', 'GET /files/r1'])
})

test('a roster that never settles before the deadline keeps the drive', async () => {
  let n = 0
  const { api, calls } = owner(Array.from({ length: 1000 }, () => ['owner', `pending-${n++}`]))
  equal(await removeDrive(api, 's1', 'r1', 50, 5, performance.now() + 100), false)
  equal(calls.some(call => call.startsWith('DELETE')), false)
})
