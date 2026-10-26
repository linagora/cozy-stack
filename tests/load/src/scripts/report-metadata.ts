import type { ServerResponse } from 'node:http'

// Report facts describe files, not the chart window. Telemetry uses Prometheus directly.
export async function fetchReportMetadata(url: URL, response: ServerResponse, prometheusUrl: string): Promise<void> {
  const path = url.pathname.replace(/^\/metadata/, '')
  if (!['/api/v1/query', '/api/v1/series', '/api/v1/labels'].includes(path) &&
      !/^\/api\/v1\/label\/[a-zA-Z_][a-zA-Z0-9_]*\/values$/.test(path)) {
    response.writeHead(404).end('not_found\n')
    return
  }
  const upstream = new URL(path, prometheusUrl)
  for (const [key, value] of url.searchParams) {
    if (['query', 'match[]', 'limit'].includes(key)) upstream.searchParams.append(key, value)
  }
  if (path === '/api/v1/query') upstream.searchParams.set('time', String(Date.now() / 1000))
  try {
    const result = await fetch(upstream, { signal: AbortSignal.timeout(3000), redirect: 'error' })
    if (!result.ok || !result.body) throw new Error('metadata_unavailable')
    const chunks: Uint8Array[] = []
    let bytes = 0
    for await (const chunk of result.body) {
      bytes += chunk.length
      if (bytes > 4 * 1024 * 1024) throw new Error('metadata_too_large')
      chunks.push(chunk)
    }
    response.setHeader('Content-Type', 'application/json')
    response.end(Buffer.concat(chunks))
  } catch {
    response.writeHead(503).end('metadata_unavailable\n')
  }
}
