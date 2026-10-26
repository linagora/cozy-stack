import { deepEqual, equal, match, ok, rejects } from 'node:assert/strict'
import { chmodSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'

import { AccessFailure, prepareActors } from './drive-actors.ts'
import { fakeActors, org, reader, writer } from './upload/fake-actors.ts'

async function fixture(t: TestContext) {
  const f = await fakeActors(t)
  // Stands in for docker: the Stack container and its address.
  writeFileSync(join(f.dir, 'docker'), '#!/bin/sh\n[ "$1" = compose ] && echo stack-1 || echo "172.18.0.5 "\n')
  chmodSync(join(f.dir, 'docker'), 0o700)
  const path = process.env.PATH
  process.env.PATH = `${f.dir}:${path}`
  t.after(() => { process.env.PATH = path })
  return f
}

test('members play the drive roles with their own credentials, then lose only the removed grant', async t => {
  const f = await fixture(t)
  const actors = await prepareActors(f.env, 'run-1')
  deepEqual(actors.drive, { id: 'sid', root: 'root' })
  deepEqual(actors.report.placement.map(p => [p.role, p.domain, p.path]),
    [['owner', org, 'owner'], ['writer', writer, 'same-stack'], ['reader', reader, 'same-stack']])
  deepEqual(actors.report.access, { writer_read: 200, writer_write: 201, reader_read: 200 })
  equal(actors.target.baseUrl, `http://${org}`)
  deepEqual(actors.target.makeArgs, [`WRITER_BASE_URL=http://${writer}`, `READER_BASE_URL=http://${reader}`,
    `ACTOR_HOSTS=${[org, writer, reader].map(d => `${d.split(':')[0]}=172.18.0.5`).join(',')}`])
  deepEqual(actors.env, { WRITER_ACCESS_TOKEN: `token-${writer}`, READER_ACCESS_TOKEN: `token-${reader}` })
  equal((await actors.target.session()).token, `token-${org}`)
  // The writer is granted through a group only; the reader through a read-only group.
  deepEqual(f.state.groups.map(g => [g.members, g.readOnly]), [[[writer], false], [[reader], true]])
  // No member probe writes outside the drive, and the reader never wrote before the workload.
  ok(!f.state.requests.some(r => r.startsWith('c POST')))

  await actors.finish()
  deepEqual(actors.report.access, { writer_read: 200, writer_write: 201, reader_read: 200, reader_write: 403 })
  deepEqual(actors.report.revocation, { writer_read: 403, writer_write: 403, reader_read: 200 })
  deepEqual(actors.report.cleanup?.retained, [])
  equal(f.state.root, 'gone')
  equal(f.state.docs.size, 0)
})

test('a fixture instance replaced since the seed is refused before any drive write', async t => {
  const f = await fixture(t)
  f.state.replaced = reader
  await rejects(prepareActors(f.env, 'run-1'), /is not the recorded reader/)
  deepEqual(f.state.requests, [])
  writeFileSync(f.manifest, JSON.stringify({ version: 1 }))
  await rejects(prepareActors(f.env, 'run-1'), /is not the fixture manifest/)
})

test('a read-only member that can write, or a writer that keeps access, is an access failure', async t => {
  for (const flaw of ['readOnlyWrites', 'keepRevoked'] as const) await t.test(flaw, async t => {
    const f = await fixture(t)
    const actors = await prepareActors(f.env, 'run-1')
    f.state[flaw] = true
    const failure = await actors.finish().then(() => null, (error: unknown) => error)
    ok(failure instanceof AccessFailure)
    match(failure.message, flaw === 'readOnlyWrites' ? /read-only member could write/ : /kept drive access/)
    // The fixtures go even so.
    deepEqual(actors.report.cleanup?.retained, [])
  })
})
