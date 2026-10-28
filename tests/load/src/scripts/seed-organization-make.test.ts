import { deepEqual, equal } from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const cwd = fileURLToPath(new URL('../../', import.meta.url))
function run(t: TestContext, target: string, settings: Record<string, string> = {},
  keys = ['SEED_USERS', 'SEED_FILES', 'SEED_DRIVES']) {
  const root = mkdtempSync(join(tmpdir(), 'seed-org-make-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))
  const bin = join(root, 'bin')
  mkdirSync(bin)
  const executable = join(bin, 'tsx')
  writeFileSync(executable, `#!/usr/bin/env node
const keys = JSON.parse(process.env.FAKE_KEYS)
console.log(JSON.stringify({ args: process.argv.slice(2),
  env: Object.fromEntries(keys.filter(k => process.env[k] !== undefined).map(k => [k, process.env[k]])) }))
`)
  chmodSync(executable, 0o700)
  const env = { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !/^(SEED_|COZY_|LOAD_TARGET)/.test(key))),
    FAKE_KEYS: JSON.stringify(keys) }
  const result = spawnSync('make', ['--no-print-directory', '-s', '-o', 'node_modules/.load-ready',
    `NODE_BIN=${bin}`, target, ...Object.entries(settings).map(([key, value]) => `${key}=${value}`)],
  { cwd, env, encoding: 'utf8' })
  equal(result.status, 0, result.stderr)
  return JSON.parse(result.stdout)
}

test('Make forwards user count and explicit zero data counts to organization seeding', t => {
  const result = run(t, 'seed-org', { SEED_USERS: '100', SEED_FILES: '0', SEED_DRIVES: '0' })
  deepEqual(result.args, ['src/scripts/seed-scale-data.ts', 'seed-org'])
  deepEqual(result.env, { SEED_USERS: '100', SEED_FILES: '0', SEED_DRIVES: '0' })
})

test('Make leaves optional organization counts unset for CLI defaults', t => {
  const result = run(t, 'seed-org')
  deepEqual(result.args, ['src/scripts/seed-scale-data.ts', 'seed-org'])
  deepEqual(result.env, {})
})

test('A Cloudery token file switches organization seeding and cleanup to the Cloudery', t => {
  deepEqual(run(t, 'seed-org', { SEED_CLOUDERY_TOKEN_FILE: 'token.json' }).args,
    ['src/scripts/seed-cloudery.ts', 'seed'])
  deepEqual(run(t, 'seed-org-cleanup', { SEED_CLOUDERY_TOKEN_FILE: 'token.json' }).args,
    ['src/scripts/seed-cloudery.ts', 'teardown'])
  // The Cloudery seeds the tree profile only.
  for (const [target, command] of [['seed-scale', 'seed-tree'], ['seed-verify', 'verify-tree'],
    ['seed-scale-cleanup', 'teardown-tree']] as const) {
    deepEqual(run(t, target, { SEED_CLOUDERY_TOKEN_FILE: 'token.json' }).args, ['src/scripts/seed-cloudery.ts', command])
  }
})

test('Make keeps an explicit organization cleanup domain in one argument', t => {
  const domain = 'seed-example.localhost:18090 with spaces'
  const result = run(t, 'seed-org-cleanup', { SEED_DOMAIN: domain })
  deepEqual(result.args, ['src/scripts/seed-scale-data.ts', 'cleanup-org', domain])
  deepEqual(run(t, 'seed-verify', { SEED_DOMAIN: domain }).args, ['src/scripts/seed-scale-data.ts', 'verify', domain])
})

test('Locally, organization fixtures default to the bundled Stack; shared DEV and the Cloudery never do', t => {
  const keys = ['SEED_ADMIN_URL', 'SEED_STACK_URL', 'SEED_ADMIN_PASSWORD', 'SEED_DOMAIN_SUFFIX', 'SEED_CONTEXT_NAME']
  deepEqual(run(t, 'seed-org', { COZY_STACK_PORT: '18080' }, keys).env, {
    SEED_ADMIN_URL: 'http://127.0.0.1:16060', SEED_STACK_URL: 'http://127.0.0.1:18080', SEED_ADMIN_PASSWORD: 'cozy',
    SEED_DOMAIN_SUFFIX: 'localhost:8080', SEED_CONTEXT_NAME: 'load-org-sharing',
  })
  deepEqual(run(t, 'seed-org', { LOAD_TARGET: 'dev' }, keys).env, {})
  deepEqual(run(t, 'seed-org', { SEED_CLOUDERY_TOKEN_FILE: 'token.json' }, keys).env, {})
})
