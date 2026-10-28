import { deepEqual, equal } from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'

const cwd = fileURLToPath(new URL('../../', import.meta.url))
test('Make forwards fanout settings as environment, not shell arguments', t => {
  const bin = mkdtempSync(join(tmpdir(), 'org-fanout-make-'))
  t.after(() => rmSync(bin, { recursive: true, force: true }))
  const script = join(bin, 'tsx')
  writeFileSync(script, `#!/usr/bin/env node
const keys = Object.keys(process.env).filter(k => /^(FANOUT|CAPACITY)_/.test(k))
console.log(JSON.stringify({ args: process.argv.slice(2),
  env: Object.fromEntries(keys.map(k => [k, process.env[k]])) }))
`)
  chmodSync(script, 0o700)
  const settings = { CAPACITY_MAX: '4', FANOUT_ORG_DOMAIN: 'seed-example.localhost:18090',
    FANOUT_REPORT: '/private/report with spaces.json', FANOUT_TIMEOUT_MS: '60000',
    FANOUT_POLL_MS: '500', FANOUT_POLL_CONCURRENCY: '8', FANOUT_STABLE_MS: '3000' }
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    !/^(FANOUT|CAPACITY|SEED)_/.test(key)))
  const result = spawnSync('make', ['--no-print-directory', '-s', '-o', 'node_modules/.load-ready',
    `NODE_BIN=${bin}`, 'org-drive-fanout', ...Object.entries(settings).map(([key, value]) => `${key}=${value}`)],
  { cwd, env, encoding: 'utf8' })
  equal(result.status, 0, result.stderr)
  const output = JSON.parse(result.stdout)
  deepEqual(output.args, ['src/scripts/org-drive-fanout.ts'])
  deepEqual(output.env, settings)
})
