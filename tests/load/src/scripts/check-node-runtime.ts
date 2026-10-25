// npm enforces `engines` only at install time (.npmrc engine-strict). This also stops a run on an
// existing install: Node 22.23.2 and 22.23.3 reject IPv6 IP-Address SANs, which breaks TLS to ::1.
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

export function minimumNode(range: string): string {
  const minimum = range.match(/^>=(\d+\.\d+\.\d+)$/)?.[1]
  if (minimum === undefined) throw new Error(`Unsupported engines.node range '${range}'; expected '>=X.Y.Z'`)
  return minimum
}

export function satisfiesMinimum(version: string, minimum: string): boolean {
  const [actual, required] = [version, minimum].map(v => v.replace(/^v/, '').split('.').map(Number))
  for (let i = 0; i < 3; i++) {
    if (actual![i] !== required![i]) return actual![i]! > required![i]!
  }
  return true
}

function isMainModule(): boolean {
  const entryPath = process.argv[1]
  return entryPath !== undefined && resolve(entryPath) === fileURLToPath(import.meta.url)
}

if (isMainModule()) {
  const manifest = JSON.parse(readFileSync(new URL('../../package.json', import.meta.url), 'utf8'))
  const minimum = minimumNode(manifest.engines.node)
  if (!satisfiesMinimum(process.version, minimum)) {
    process.stderr.write(`Node ${process.version} is not supported here; tests/load needs ${minimum} or newer. ` +
      'Run make node-checks (README) or upgrade Node.\n')
    process.exitCode = 1
  }
}
