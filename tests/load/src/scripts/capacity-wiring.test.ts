import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, posix, resolve } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const cwd = fileURLToPath(new URL('../../', import.meta.url))

// A tsx stand-in that prints the report path and arguments Make hands to the fanout runner.
function fakeTsx(t: TestContext): string {
  const bin = mkdtempSync(join(tmpdir(), 'capacity-wiring-'))
  t.after(() => rmSync(bin, { recursive: true, force: true }))
  const executable = join(bin, 'tsx')
  writeFileSync(executable,
    '#!/usr/bin/env node\nconsole.log(JSON.stringify({ report: process.env.FANOUT_REPORT,' +
    ' args: process.argv.slice(2) }))\n')
  chmodSync(executable, 0o700)
  return bin
}

const fanoutFreeEnv = (): NodeJS.ProcessEnv =>
  Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith('FANOUT_')))

test('Make persists standalone fanout reports in the observed results directory by default', t => {
  const bin = fakeTsx(t)
  const result = spawnSync('make', ['--no-print-directory', '-s', '-o', 'node_modules/.load-ready',
    `NODE_BIN=${bin}`, 'EXECUTION_ID=test-execution', 'org-drive-fanout'],
  { cwd, env: fanoutFreeEnv(), encoding: 'utf8' })
  equal(result.status, 0, result.stderr)
  deepEqual(JSON.parse(result.stdout), { report: 'data/results/test-execution-org-drive-fanout.json',
    args: ['src/scripts/org-drive-fanout.ts'] })
})

test('a campaign fanout step names its report after the step, not the campaign make', t => {
  const bin = fakeTsx(t)
  // `make campaign` runs each step as a nested make with its own EXECUTION_ID.
  const campaign = join(bin, 'campaign.mk')
  writeFileSync(campaign, 'campaign-step:\n\t@$(MAKE) --no-print-directory -s -o node_modules/.load-ready' +
    ' EXECUTION_ID=campaign-step org-drive-fanout\n')
  const result = spawnSync('make', ['--no-print-directory', '-s', '-f', 'Makefile', '-f', campaign,
    `NODE_BIN=${bin}`, 'campaign-step'], { cwd, env: fanoutFreeEnv(), encoding: 'utf8' })
  equal(result.status, 0, result.stderr)
  deepEqual(JSON.parse(result.stdout), { report: 'data/results/campaign-step-org-drive-fanout.json',
    args: ['src/scripts/org-drive-fanout.ts'] })
})

test('campaign steps record each producer exit that make itself reports as 2', async t => {
  const bin = mkdtempSync(join(tmpdir(), 'record-exit-'))
  t.after(() => rmSync(bin, { recursive: true, force: true }))
  // Stands in for node, tsx and docker compose: exits with the status the test chose.
  const producer = join(bin, 'tsx')
  writeFileSync(producer, '#!/bin/sh\nexit "$FAKE_EXIT"\n')
  chmodSync(producer, 0o700)
  writeFileSync(join(bin, 'node'), '#!/bin/sh\nexit "$FAKE_EXIT"\n')
  chmodSync(join(bin, 'node'), 0o700)
  // cozy-capacity reaches upload-capacity through a nested make, as campaigns do.
  const nested = join(bin, 'nested.mk')
  writeFileSync(nested, 'nested-upload-capacity:\n\t@$(MAKE) --no-print-directory -s -o node_modules/.load-ready upload-capacity\n')
  const steps: [string, string[], number][] = [
    ['upload-capacity', ['-f', 'Makefile', '-f', nested, 'nested-upload-capacity'], 1],
    ['shared-drive-ramp', ['-o', 'upload-binary', '-o', 'observability-up', `COMPOSE=${producer}`,
      'cozy-shared-drive-ramp'], 99],
    ['org-drive-fanout', ['-o', 'node_modules/.load-ready', 'org-drive-fanout'], 2],
  ]
  for (const [name, args, code] of steps) await t.test(name, () => {
    const file = join(bin, `${name}.exit`)
    const result = spawnSync('make', ['--no-print-directory', '-s', `NODE_BIN=${bin}`, 'EXECUTION_ID=wiring',
      `RECORD_EXIT=bash campaigns/lib/record-exit.sh ${file}`, ...args],
    { cwd, env: { ...fanoutFreeEnv(), PATH: `${bin}:${process.env.PATH}`, FAKE_EXIT: String(code) }, encoding: 'utf8' })
    equal(result.status, 2, result.stderr)
    equal(readFileSync(file, 'utf8'), `${code}\n`)
  })
})

test('Compose adds an internal read-only Node results service without exposing private reports', () => {
  const source = readFileSync(join(cwd, 'compose.yaml'), 'utf8')
  const section = source.match(/^  capacity-results:\n([\s\S]*?)(?=^  \S|^networks:)/m)?.[1]
  ok(section, 'missing capacity-results service')
  match(section, /image: \*node-image/)
  match(source, /^x-node-image: &node-image node:22-alpine@sha256:[0-9a-f]{64}$/m)
  match(section, /--experimental-strip-types/)
  match(section, /\.\/data\/results:\/results:ro/)
  match(section, /CAPACITY_RESULTS_DIR: \/results/)
  match(section, /serve-capacity-results\.ts/)
  equal(/^    ports:/m.test(section), false)
  match(source, /capacity-results:\n\s+condition: service_healthy/)
})

test('Compose mounts every results entrypoint runtime import as an existing read-only file', () => {
  const compose = readFileSync(join(cwd, 'compose.yaml'), 'utf8')
  const section = compose.match(/^  capacity-results:\n([\s\S]*?)(?=^  \S|^networks:)/m)?.[1]
  ok(section, 'missing capacity-results service')
  const entrypoint = section.match(/^      - (\/scripts\/[^\s]+\.ts)$/m)?.[1]
  ok(entrypoint, 'missing results entrypoint')
  const mounts = new Map([...section.matchAll(/^      - (\.\/[^:\n]+):(\/[^:\n]+):ro$/gm)]
    .map(([, source, target]) => [target!, resolve(cwd, source!)]))
  const visited = new Set<string>()
  const visit = (target: string): void => {
    if (visited.has(target)) return
    visited.add(target)
    const source = mounts.get(target)
    ok(source, `missing read-only Compose source mount for ${target}`)
    ok(statSync(source).isFile(), `Compose source must be an existing file: ${source}`)
    const text = readFileSync(source, 'utf8')
    const imports = text.matchAll(/^import\s+(?:([\s\S]*?)\s+from\s+)?['"]([^'"]+)['"]/gm)
    for (const [, clause, specifier] of imports) {
      if (/^type\b/.test(clause ?? '')) continue
      const named = clause?.trim().match(/^\{([\s\S]*)\}$/)?.[1]
      const bindings = named?.split(',').map(binding => binding.trim()).filter(Boolean)
      if (bindings?.length && bindings.every(binding => /^type\s/.test(binding))) continue
      if (specifier!.startsWith('.')) visit(posix.resolve(posix.dirname(target), specifier!))
    }
  }
  visit(entrypoint)
})

test('Prometheus scrapes results and Make observability starts their producer', () => {
  const prometheus = readFileSync(join(cwd, 'config/prometheus/prometheus.yml'), 'utf8')
  match(prometheus, /job_name: capacity-results/)
  match(prometheus, /capacity-results:9465/)
  const make = readFileSync(join(cwd, 'Makefile'), 'utf8')
  ok(/observability-up:\n(?:\t[^\n]*\n)*\t@.*capacity-results/.test(make),
    'observability-up must start the capacity-results service')
  match(make, /capacity-dashboard-url:/)
})

test('campaigns can include fanout as a classified persisted step', () => {
  const source = readFileSync(join(cwd, 'campaigns/run-campaign.sh'), 'utf8')
  match(source, /org_drive_fanout\(\)/)
  match(source, /run_step "\$name" org-drive-fanout/)
})
