import { deepEqual, equal, rejects } from 'node:assert/strict'
import { createServer, type RequestListener } from 'node:http'
import { test, type TestContext } from 'node:test'

import { FixtureDeadline, FixtureError, request } from './fixture-http.ts'

async function fixture(t: TestContext, handler: RequestListener): Promise<URL> {
  const server = createServer(handler)
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', resolve))
  t.after(async () => {
    server.closeAllConnections()
    await new Promise<void>((resolve) => server.close(() => resolve()))
  })
  return new URL(`http://127.0.0.1:${(server.address() as { port: number }).port}/private-path?secret=value`)
}

test('sends exact tenant Host, method, credentials and byte length with a Buffer body', async (t) => {
  const body = Buffer.from('fixture-\u00e9')
  let received: unknown
  const url = await fixture(t, async (req, res) => {
    const chunks: Buffer[] = []
    for await (const chunk of req) chunks.push(Buffer.from(chunk))
    received = { method: req.method, path: req.url, host: req.headers.host,
      authorization: req.headers.authorization, length: req.headers['content-length'],
      body: Buffer.concat(chunks) }
    res.writeHead(201).end('reply-\u00e9')
  })
  deepEqual(await request(url, 'POST', {
    Host: 'seed-tenant.localhost:8080', Authorization: 'Bearer private-token', 'Content-Length': '1',
  }, 1000, body), { status: 201, body: 'reply-\u00e9' })
  deepEqual(received, { method: 'POST', path: '/private-path?secret=value',
    host: 'seed-tenant.localhost:8080', authorization: 'Bearer private-token',
    length: String(body.length), body })
})

test('explicit reply bound accepts one MiB but rejects one additional byte', async (t) => {
  const limit = 1_048_576
  let size = limit
  const url = await fixture(t, (_req, res) => res.end('x'.repeat(size)))
  equal((await request(url, 'GET', {}, 1000, '', limit)).body.length, limit)
  size++
  await rejects(request(url, 'GET', {}, 1000, '', limit), (error: unknown) =>
    error instanceof FixtureError && error.message === `API response exceeds ${limit} bytes`)
})

test('default reply bound counts UTF-8 bytes and retains its sanitized overflow error', async (t) => {
  let body = '\u00e9'.repeat(32_768)
  const url = await fixture(t, (_req, res) => res.end(body))
  equal((await request(url, 'GET', {}, 1000)).body, body)
  body += 'private-upstream-payload'
  await rejects(request(url, 'GET', { Authorization: 'private-header' }, 1000), (error: unknown) =>
    error instanceof FixtureError && error.message === 'API response exceeds 64 KiB')
})

test('an explicit smaller reply bound is enforced without changing the default', async (t) => {
  const url = await fixture(t, (_req, res) => res.end('12345'))
  await rejects(request(url, 'GET', {}, 1000, '', 4), (error: unknown) =>
    error instanceof FixtureError && error.message === 'API response exceeds 4 bytes')
  equal((await request(url, 'GET', {}, 1000)).body, '12345')
})

test('complete-response deadline covers stalled headers, stalled bodies and trickling bytes', async (t) => {
  for (const mode of ['headers', 'body', 'trickle']) await t.test(mode, async (t) => {
    let calls = 0
    let close!: () => void
    const closed = new Promise<void>((resolve) => { close = resolve })
    const url = await fixture(t, (_req, res) => {
      calls++
      if (mode !== 'headers') res.writeHead(200).write('private-upstream-payload')
      const pulse = mode === 'trickle' ? setInterval(() => res.write('x'), 20) : undefined
      const finish = setTimeout(() => res.end('too late'), 1500)
      res.once('close', () => { clearInterval(pulse); clearTimeout(finish); close() })
    })
    await rejects(request(url, 'GET', { Authorization: 'private-header' }, 100), (error: unknown) =>
      error instanceof FixtureDeadline && error instanceof FixtureError &&
      error.message === 'API request deadline exceeded')
    await closed
    equal(calls, 1)
  })
})

test('disconnects and truncated replies reject once without exposing upstream details', async (t) => {
  for (const mode of ['disconnect', 'truncated']) await t.test(mode, async (t) => {
    let calls = 0
    const url = await fixture(t, (_req, res) => {
      calls++
      if (mode === 'disconnect') res.destroy()
      else {
        res.writeHead(200, { 'Content-Length': '1000' }).write('private-upstream-payload')
        setImmediate(() => res.destroy())
      }
    })
    const message = mode === 'disconnect' ? 'API request failed' : 'API response incomplete'
    await rejects(request(url, 'POST', { Authorization: 'private-header' }, 1000, 'private-body'),
      (error: unknown) => error instanceof FixtureError && error.message === message)
    equal(calls, 1)
  })
})

test('redirects are returned without following or retrying requests', async (t) => {
  let calls = 0
  const url = await fixture(t, (_req, res) => {
    calls++
    res.writeHead(302, { Location: '/must-not-follow' }).end('redirect reply')
  })
  deepEqual(await request(url, 'GET', { Authorization: 'private-header' }, 1000),
    { status: 302, body: 'redirect reply' })
  equal(calls, 1)
})

test('waits out a busy instance for reads and deletes only, within the request timeout', async (t) => {
  const calls: string[] = []
  let busy = 0
  let retryAfter: string | undefined = '0'
  const url = await fixture(t, (req, res) => {
    calls.push(req.method!)
    if (busy-- > 0) res.writeHead(503, retryAfter === undefined ? {} : { 'Retry-After': retryAfter }).end('busy')
    else res.writeHead(204).end()
  })
  const run = async (method: string, times: number) => {
    busy = times
    calls.length = 0
    return [(await request(url, method, {}, 1000)).status, calls.length]
  }
  deepEqual(await run('DELETE', 2), [204, 3])
  deepEqual(await run('GET', 1), [204, 2])
  // A create may still have committed: it is never retried.
  deepEqual(await run('POST', 1), [503, 1])
  // A wait past the timeout, or a 503 the Stack did not ask to retry, comes back as is.
  retryAfter = '5'
  deepEqual(await run('DELETE', 1), [503, 1])
  retryAfter = undefined
  deepEqual(await run('DELETE', 1), [503, 1])
})
