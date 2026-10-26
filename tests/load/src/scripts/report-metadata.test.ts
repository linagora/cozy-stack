import assert from 'node:assert/strict'
import { once } from 'node:events'
import { createServer } from 'node:http'
import test from 'node:test'

import { createResultsServer } from './serve-capacity-results.ts'

test('report queries use current facts regardless of the telemetry window; label queries have no time filter', async t => {
  const requests: URL[] = []
  const upstream = createServer((request, response) => {
    requests.push(new URL(request.url!, 'http://prometheus'))
    response.setHeader('Content-Type', 'application/json')
    response.end(JSON.stringify({ status: 'success', data: { resultType: 'vector', result: [] } }))
  }).listen(0, '127.0.0.1')
  await once(upstream, 'listening')
  const address = upstream.address()
  assert.ok(address && typeof address !== 'string')
  const server = createResultsServer('/unused', `http://127.0.0.1:${address.port}`).listen(0, '127.0.0.1')
  await once(server, 'listening')
  const local = server.address()
  assert.ok(local && typeof local !== 'string')
  t.after(() => { server.closeAllConnections(); server.close(); upstream.closeAllConnections(); upstream.close() })
  const base = `http://127.0.0.1:${local.port}/metadata`
  const query = 'cozy_load_run_info{run="old-run"}'
  const result = await fetch(`${base}/api/v1/query?query=${encodeURIComponent(query)}&time=1&start=0&end=1`)
  assert.equal(result.status, 200)
  assert.equal(requests[0]!.searchParams.get('query'), query)
  assert.ok(Number(requests[0]!.searchParams.get('time')) > Date.now() / 1000 - 10)
  assert.equal(requests[0]!.searchParams.has('start'), false)
  await fetch(`${base}/api/v1/label/run/values?match[]=cozy_load_run_info&start=0&end=1`)
  assert.equal(requests[1]!.searchParams.has('start'), false)
  assert.equal(requests[1]!.searchParams.has('end'), false)
  assert.equal(requests[1]!.searchParams.get('match[]'), 'cozy_load_run_info')
  const denied = await fetch(`${base}/api/v1/admin/tsdb/delete_series?match[]=cozy_load_run_info`)
  assert.equal(denied.status, 404)
  assert.equal(requests.length, 2)
})

test('upstream failure cannot leak private response bodies', async t => {
  const upstream = createServer((request, response) => response.writeHead(500).end('private-token')).listen(0, '127.0.0.1')
  await once(upstream, 'listening')
  const address = upstream.address()
  assert.ok(address && typeof address !== 'string')
  const server = createResultsServer('/unused', `http://127.0.0.1:${address.port}`).listen(0, '127.0.0.1')
  await once(server, 'listening')
  const local = server.address()
  assert.ok(local && typeof local !== 'string')
  t.after(() => { server.closeAllConnections(); server.close(); upstream.closeAllConnections(); upstream.close() })
  const result = await fetch(`http://127.0.0.1:${local.port}/metadata/api/v1/query?query=up`)
  assert.equal(result.status, 503)
  assert.equal(await result.text(), 'metadata_unavailable\n')
})
