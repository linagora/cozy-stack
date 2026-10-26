import { deepEqual, equal, match, ok, rejects } from 'node:assert/strict'
import { chmodSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'

import { AccessFailure, prepareActors } from './drive-actors.ts'
import { FixtureError } from './fixture-http.ts'
import { fakeActors, org, reader, writer } from './upload/fake-actors.ts'

async function fixture(t: TestContext, docker = '[ "$1" = compose ] && echo stack-1 || echo "172.18.0.5 "') {
  const f = await fakeActors(t)
  // Stands in for docker: the Stack container and its address.
  writeFileSync(join(f.dir, 'docker'), `#!/bin/sh\n${docker}\n`)
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

test('the writer is out only when it is explicitly refused both reading and writing', async t => {
  for (const [flaw, expected] of [['flap', AccessFailure], ['error', FixtureError]] as const) await t.test(flaw, async t => {
    const f = await fixture(t)
    const actors = await prepareActors(f.env, 'run-1')
    // A refusal that flips back, or a server error, never proves the writer is out.
    if (flaw === 'flap') f.state.flapAfterRevoke = true
    else f.state.revokedWriteStatus = 500
    const failure = await actors.finish().then(() => null, (error: unknown) => error)
    ok(failure instanceof expected, String(failure))
    match(failure.message, flaw === 'flap' ? /kept drive access/ : /access was not shown to end/)
    deepEqual(actors.report.cleanup?.retained, [])
  })
})

test('a failed setup or group removal still removes every fixture', async t => {
  await t.test('members never accept', async t => {
    const f = await fixture(t)
    f.state.neverReady = true
    await rejects(prepareActors(f.env, 'run-1'), /did not accept the drive in time: pending, pending; its fixtures were removed/)
    deepEqual([f.state.root, f.state.docs.size], ['gone', 0])
  })
  await t.test('group removal fails', async t => {
    const f = await fixture(t)
    const actors = await prepareActors(f.env, 'run-1')
    f.state.removalStatus = 500
    await rejects(actors.finish(), /Removing the writer's group: HTTP 500/)
    deepEqual([f.state.root, f.state.docs.size, actors.report.cleanup?.retained], ['gone', 0, []])
  })
  await t.test('no Stack address', async t => {
    const f = await fixture(t, 'exit 1')
    await rejects(prepareActors(f.env, 'run-1'), /Cannot find the Stack container address/)
    ok(!f.state.requests.some(r => r.includes('POST')), 'nothing is written before the address is known')
  })
})

test('a creation whose identity never came back is reported as possibly remaining', async t => {
  const f = await fixture(t)
  f.state.emptyDriveReply = true
  const failure = await prepareActors(f.env, 'run-1').then(() => null, (error: unknown) => error)
  ok(failure instanceof FixtureError)
  match(failure.message, /fixtures remain: drive twake-load-actors-run-1 \(creation outcome unknown\)/)
  ok(!failure.message.includes('were removed'))
  ok(!f.state.requests.some(r => r.includes('DELETE')), 'nothing it cannot identify is deleted')
})

test('preparation waits no longer than the run allows, and refuses an invalid bound', async t => {
  const f = await fixture(t)
  f.state.neverReady = true
  const started = Date.now()
  await rejects(prepareActors({ ...f.env, ACTORS_SETTLE_MS: '300000' }, 'run-1', Date.now() + 100),
    /did not accept the drive in time/)
  ok(Date.now() - started < 5000)
  await rejects(prepareActors({ ...f.env, ACTORS_SETTLE_MS: 'soon' }, 'run-1'), /ACTORS_SETTLE_MS/)
})
