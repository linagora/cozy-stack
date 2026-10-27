import * as http from 'node:http'
import * as https from 'node:https'
import { isIP } from 'node:net'

export class FixtureError extends Error {}
// Only the complete-response deadline below; callers may map it to their own budget.
export class FixtureDeadline extends FixtureError {}
export type Reply = { status: number, body: string }

// The Stack answers 503 with Retry-After when it could not take an instance lock, before doing anything.
// Reads and deletes wait that out within the request's own timeout. Nothing else is retried, and nothing
// is redirected: another failed request can still have committed upstream writes.
const RETRIED = ['GET', 'HEAD', 'DELETE']
const MAX_RETRY_AFTER_MS = 30_000

export async function request(
  url: URL, method: string, headers: Record<string, string>, timeout: number, body: string | Buffer = '',
  responseLimit = 65_536,
): Promise<Reply> {
  // Monotonic, as every deadline: wall-clock changes neither shorten nor extend it.
  const end = performance.now() + timeout
  for (;;) {
    const { retryAfterMs, ...reply } = await attempt(url, method, headers, end - performance.now(), body, responseLimit)
    if (retryAfterMs === null || !RETRIED.includes(method) || performance.now() + retryAfterMs >= end) return reply
    await new Promise(resolve => setTimeout(resolve, retryAfterMs))
  }
}

function attempt(
  url: URL, method: string, headers: Record<string, string>, timeout: number, body: string | Buffer,
  responseLimit: number,
): Promise<Reply & { retryAfterMs: number | null }> {
  let deadline: NodeJS.Timeout | undefined
  return new Promise<Reply & { retryAfterMs: number | null }>((resolve, reject) => {
    let response: http.IncomingMessage | undefined
    const transport = url.protocol === 'https:' ? https : http
    const hostname = url.hostname.replace(/^\[|\]$/g, '')
    const req = transport.request(url, {
      method, headers: { ...headers, 'Content-Length': String(Buffer.byteLength(body)) },
      // Otherwise HTTPS derives TLS identity from the overridden tenant Host header.
      ...(url.protocol === 'https:' ? { servername: isIP(hostname) ? '' : hostname } : {}),
    }, (res) => {
      response = res
      const chunks: Buffer[] = []
      let size = 0
      res.on('data', (chunk: Buffer) => {
        size += chunk.length
        if (size > responseLimit) {
          // Settle with a sanitized error before destruction emits aborted/error events.
          reject(new FixtureError(responseLimit === 65_536 ? 'API response exceeds 64 KiB' :
            `API response exceeds ${responseLimit} bytes`))
          res.destroy()
          req.destroy()
        } else chunks.push(chunk)
      })
      const retryAfter = String(res.headers['retry-after'] ?? '')
      const retryAfterMs = res.statusCode === 503 && /^\d+$/.test(retryAfter)
        ? Math.min(Number(retryAfter) * 1000, MAX_RETRY_AFTER_MS) : null
      res.on('end', () => resolve({
        status: res.statusCode ?? 0, body: Buffer.concat(chunks).toString('utf8'), retryAfterMs,
      }))
      res.on('aborted', () => reject(new FixtureError('API response incomplete')))
      res.on('error', () => reject(new FixtureError('API response failed')))
    })
    req.on('error', () => reject(new FixtureError('API request failed')))
    // A complete-response deadline, not a socket inactivity timeout (trickles must not extend it).
    deadline = setTimeout(() => {
      reject(new FixtureDeadline('API request deadline exceeded'))
      response?.destroy()
      req.destroy()
    }, timeout)
    req.end(body)
  }).finally(() => clearTimeout(deadline))
}
