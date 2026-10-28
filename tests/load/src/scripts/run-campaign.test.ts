import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawn, spawnSync } from 'node:child_process'
import {
  chmodSync, copyFileSync, cpSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

import { mixedCapacityEnvelope } from './result-envelope.ts'

const harness = fileURLToPath(new URL('../../', import.meta.url))

// Plays one step from $FAKE_STEPS/<step>.*: setup failure before the producer,
// a delay, a graceful stop that logs each SIGTERM, the report it leaves and the
// producer's own exit, which make turns into 2.
const fakeMake = `#!/usr/bin/env bash
target=$1 id= record= load_target=
for arg in "$@"; do
  case $arg in
    CAPACITY_RUN_ID=* | EXECUTION_ID=*) id=\${arg#*=} ;;
    RECORD_EXIT=*) record=\${arg#*=} ;;
    LOAD_TARGET=*) load_target=\${arg#*=} ;;
  esac
done
case $target in
  upload-capacity | shared-drive-ramp | org-drive-fanout | mixed-capacity) suffix=$target ;;
  *) suffix= ;;
esac
echo "$target" >> "$FAKE_STEPS/targets"
echo "$*" >> "$FAKE_STEPS/commands"
# Runners take the shared DEV lock on DEV, as the Makefile does.
[ -z "$suffix" ] || [ "$load_target" != dev ] || bash campaigns/lib/shared-dev-lock.sh "$DEV_LOCK_FILE" true || exit 2
step=\${id#*-}
play=$FAKE_STEPS/\${step:-$target}
[ ! -e "$play.setup-fails" ] || exit 2
[ ! -e "$play.sleep" ] || sleep "$(cat "$play.sleep")"
if [ -e "$play.graceful" ]; then
  trap 'echo TERM >> "$play.terms"' TERM
  sleep 30 & wait $!
  # Like k6 cleaning up after its first signal; a second one would be logged.
  sleep 2 & wait $!
  exit 143
fi
[ ! -e "$play.json" ] || cp "$play.json" "data/results/$id-$suffix.json"
code=$(cat "$play.exit" 2>/dev/null || echo 0)
[ -z "$suffix" ] || $record sh -c "exit $code"
[ "$code" = 0 ] || exit 2
`

type Play = { exit?: number, graceful?: boolean, report?: object, setupFails?: boolean, sleep?: number }
type Run = {
  code: number | null, commands: string[], log: string, signals: (step: string) => number, steps: string[][], summary: string,
  targets: string[],
}
type Interrupt = { after: string, delayMs: number }
// campaign runs a real definition instead; during runs once the named step has started.
type Options = { campaign?: string, env?: Record<string, string>, during?: { after: string, run: () => void } }

async function runCampaign(
  t: TestContext, definition: string, plays: Record<string, Play>, interrupt?: Interrupt, options: Options = {},
): Promise<Run> {
  const root = mkdtempSync(join(tmpdir(), 'run-campaign-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))
  const [dir, bin, steps] = ['harness', 'bin', 'steps'].map(name => join(root, name)) as [string, string, string]
  mkdirSync(join(dir, 'campaigns', 'lib'), { recursive: true })
  mkdirSync(join(dir, 'data', 'results'), { recursive: true })
  mkdirSync(bin)
  mkdirSync(steps)
  for (const file of ['run-campaign.sh', 'lib/record-exit.sh', 'lib/shared-dev-lock.sh', 'capacity.sh', 'smoke.sh']) {
    copyFileSync(join(harness, 'campaigns', file), join(dir, 'campaigns', file))
  }
  // A copy, not a link: the summary only runs when invoked by its real path.
  cpSync(join(harness, 'src'), join(dir, 'src'), { recursive: true })
  symlinkSync(join(harness, 'node_modules'), join(dir, 'node_modules'))
  writeFileSync(join(dir, 'campaigns', 'scenario.sh'), `campaign() {\n${definition}\n}\n`)
  for (const [name, body] of [['make', fakeMake], ['docker', '#!/bin/sh\nexit 0\n']]) {
    writeFileSync(join(bin, name!), body!)
    chmodSync(join(bin, name!), 0o700)
  }
  for (const [step, play] of Object.entries(plays)) {
    if (play.report) writeFileSync(join(steps, `${step}.json`), JSON.stringify(play.report))
    if (play.exit !== undefined) writeFileSync(join(steps, `${step}.exit`), String(play.exit))
    if (play.sleep !== undefined) writeFileSync(join(steps, `${step}.sleep`), String(play.sleep))
    if (play.setupFails) writeFileSync(join(steps, `${step}.setup-fails`), '')
    if (play.graceful) writeFileSync(join(steps, `${step}.graceful`), '')
  }

  const child = spawn('bash', ['campaigns/run-campaign.sh', options.campaign ?? 'scenario'], {
    cwd: dir, env: { ...process.env, FAKE_STEPS: steps, PATH: `${bin}:${process.env.PATH}`, ...options.env },
  })
  let log = ''
  let interrupting = false
  let during = options.during
  child.stdout.on('data', (chunk: Buffer) => {
    log += chunk
    if (during && log.includes(`START ${during.after}`)) {
      during.run()
      during = undefined
    }
    if (interrupt === undefined || interrupting || !log.includes(`START ${interrupt.after}`)) return
    interrupting = true
    setTimeout(() => child.kill('SIGTERM'), interrupt.delayMs)
  })
  child.stderr.on('data', (chunk: Buffer) => { log += chunk })
  const code = await new Promise<number | null>(done => child.on('close', done))

  const runs = join(dir, 'data', 'results', 'campaigns')
  const [run] = readdirSync(runs)
  ok(run, log)
  const read = (name: string): string => {
    try { return readFileSync(join(runs, run, name), 'utf8') } catch { return '' }
  }
  const lines = (name: string): string[] => {
    try { return readFileSync(join(steps, name), 'utf8').split('\n').filter(Boolean) } catch { return [] }
  }
  return {
    code,
    commands: lines('commands'),
    log,
    signals: (step: string): number => {
      try {
        return readFileSync(join(steps, `${step}.terms`), 'utf8').split('\n').filter(Boolean).length
      } catch {
        return 0
      }
    },
    steps: read('steps.tsv').trim().split('\n').filter(Boolean).map(line => line.split('\t')),
    summary: read('summary.md'),
    targets: lines('targets'),
  }
}

const upload = { attempts: [{ cleanup_rate: 1, consistency_rate: 1 }], maximum_concurrent_uploads: 48,
  result_bound: 'exact', status: 'completed' }
const ramp = (bound: 'bracket' | 'lower_bound') => ({ levels: [], max_sustained_level: 8,
  result_bound: bound, status: 'completed' })
const fanout = { capacity: { kind: 'bracket', lower: 2, max: null, upper_exclusive: 4 }, points: [], status: 'failed' }
// The mixed search has no legacy classification: only its envelope gives a verdict.
const mixedSearch = { levels: [{ rate: 2, status: 'passed', correctness: { complete: true },
  cleanup: { cleaned: [], remaining: [] } }], max_sustained_rate: 2, first_failed_rate: null,
result_bound: 'lower_bound', status: 'completed', correctness_failures: [], outcome: 0, load_target: 'local' }
const mixed = { ...mixedSearch, result: mixedCapacityEnvelope(mixedSearch, { interrupted: false,
  versions: { harness: 'test', stack: 'test' }, topology: 'single-host', start: '2026-10-27T10:00:00Z',
  end: '2026-10-27T10:01:00Z' }) }
const producers = `upload_capacity upload FILE_SIZE=1M
shared_drive_ramp ramp CAPACITY_START=8 CAPACITY_MAX=16
org_drive_fanout fanout CAPACITY_START=2 CAPACITY_MAX=4`
const producerExits = (run: Run): string[] => run.steps.map(step => step[6] ?? '')

test('a campaign whose steps all completed exits 0', async t => {
  const run = await runCampaign(t, producers, {
    fanout: { exit: 0, report: { ...fanout, capacity: { kind: 'lower_bound', lower: 4 }, status: 'passed' } },
    ramp: { exit: 0, report: ramp('lower_bound') },
    upload: { exit: 0, report: upload },
  })
  equal(run.code, 0, run.log)
  match(run.summary, /- Outcome: 0 \(completed\)/)
  deepEqual(producerExits(run), ['0', '0', '0'])
})

test('a valid capacity bracket from every producer still exits 0', async t => {
  // k6 exits 99 when the ramp stops on mostly failed requests: expected overload.
  const run = await runCampaign(t, producers, {
    fanout: { exit: 0, report: fanout },
    ramp: { exit: 99, report: ramp('bracket') },
    upload: { exit: 0, report: upload },
  })
  equal(run.code, 0, run.log)
  deepEqual(run.steps.map(step => step[5]), ['0', '2', '0'], 'make still reports the k6 exit as 2')
  deepEqual(producerExits(run), ['0', '99', '0'])
})

test('a correctness failure exits 1 even when later steps complete', async t => {
  const corrupt = { ...upload, attempts: [{ cleanup_rate: 1, consistency_rate: 1 }, { cleanup_rate: 1, consistency_rate: 0.5 }] }
  const run = await runCampaign(t, producers, {
    fanout: { exit: 0, report: fanout },
    ramp: { exit: 0, report: ramp('lower_bound') },
    upload: { exit: 1, report: corrupt },
  })
  equal(run.code, 1, run.log)
  match(run.summary, /\| upload \| .* \| 1: uploaded content did not match after download \|/)
})

test('a step without a report leaves the campaign inconclusive', async t => {
  const run = await runCampaign(t, 'org_drive_fanout fanout', { fanout: { exit: 2 } })
  equal(run.code, 2, run.log)
  match(run.summary, /\| fanout \| no result file \(exit 2\) \| 2: no result file \(exit 2\) \|/)
})

test('a cleanup failure is inconclusive, never success', async t => {
  const run = await runCampaign(t, 'shared_drive_ramp ramp', { ramp: { exit: 99, report: {
    failure_reason: 'test-directory cleanup failed', levels: [], max_sustained_level: null,
    result_bound: 'inconclusive', status: 'infrastructure_failure',
  } } })
  equal(run.code, 2, run.log)
  match(run.summary, /2: test-directory cleanup failed/)
})

test('a correctness failure with a failed cleanup exits 1 and keeps both reasons', async t => {
  const run = await runCampaign(t, 'upload_capacity upload', { upload: { exit: 1, report: {
    attempts: [{ cleanup_rate: 1, consistency_rate: 0.5 }, { cleanup_rate: 0, consistency_rate: 1 }],
    failure_reason: 'test-directory cleanup failed', status: 'infrastructure_failure',
  } } })
  equal(run.code, 1, run.log)
  match(run.summary, /1: uploaded content did not match after download; test-directory cleanup failed/)
})

test('an early setup failure keeps the campaign inconclusive after a successful final step', async t => {
  const run = await runCampaign(t, 'upload_capacity upload\norg_drive_fanout fanout', {
    fanout: { exit: 0, report: fanout },
    upload: { setupFails: true },
  })
  equal(run.code, 2, run.log)
  deepEqual(producerExits(run), ['', '0'], 'the upload producer never ran')
  match(run.summary, /\| upload \| no result file \(exit 2\) \| 2: no result file \(exit 2\) \|/)
})

test('an interruption stops the running step once, skips the rest and still summarizes', async t => {
  for (const [moment, delayMs] of [['while it starts', 0], ['while it runs', 1500]] as const) {
    await t.test(moment, async t => {
      const started = Date.now()
      const run = await runCampaign(t, 'upload_capacity upload\norg_drive_fanout fanout', {
        fanout: { exit: 0, report: fanout },
        upload: { graceful: true },
      }, { after: 'upload', delayMs })
      equal(run.code, 2, run.log)
      ok(Date.now() - started < 20_000, 'the step was stopped, not waited for')
      equal(run.signals('upload'), 1, 'a second signal would abort a graceful stop')
      deepEqual(run.steps.map(step => [step[0], step[5]]), [['upload', '143']])
      match(run.log, /SKIP fanout: campaign interrupted/)
      match(run.summary, /- Outcome: 2 \(inconclusive\)\n- Interrupted by SIGTERM/)
    })
  }
})

const lockScript = join(harness, 'campaigns', 'lib', 'shared-dev-lock.sh')
const hasFlock = () => spawnSync('sh', ['-c', 'command -v flock']).status === 0

test('a shared DEV campaign stops after a failed or inconclusive step', async t => {
  if (!hasFlock()) return t.skip('flock is not installed')
  for (const step of ['upload-capacity', 'shared-drive-ramp']) {
    for (const exit of [1, 2]) await t.test(`${step} exits ${exit}`, async t => {
      const dir = mkdtempSync(join(tmpdir(), 'dev-campaign-stop-'))
      t.after(() => rmSync(dir, { recursive: true, force: true }))
      const run = await runCampaign(t, '', {
        'upload-capacity': { exit: 0, report: upload },
        [step]: { exit, report: exit === 1
          ? (step === 'upload-capacity'
            ? { ...upload, attempts: [{ cleanup_rate: 1, consistency_rate: 0 }] }
            : { ...ramp('bracket'), max_sustained_level: 0 })
          : { status: 'infrastructure_failure', failure_reason: 'cleanup unverified' } },
      }, undefined, { campaign: 'capacity', env: { LOAD_TARGET: 'dev', DEV_LOCK_FILE: join(dir, 'shared-dev.lock') } })
      equal(run.code, exit, run.log)
      match(run.log, new RegExp(`SKIP .*: ${step} did not complete on shared DEV`))
      deepEqual(run.targets, step === 'upload-capacity'
        ? ['upload-capacity'] : ['upload-capacity', 'shared-drive-ramp'])
    })
  }
})

// The value make uses: the last assignment on its command line.
const setting = (command: string | undefined, name: string): string | undefined =>
  command?.split(' ').filter(arg => arg.startsWith(`${name}=`)).at(-1)?.slice(name.length + 1)

test('smoke and capacity run every runner on their own ladders, locally on one organization fixture', async t => {
  const plays = {
    'upload-capacity': { exit: 0, report: upload },
    'shared-drive-ramp': { exit: 0, report: ramp('lower_bound') },
    'org-drive-fanout': { exit: 0, report: { ...fanout, capacity: { kind: 'lower_bound', lower: 4 }, status: 'passed' } },
    'mixed-capacity': { exit: 0, report: mixed },
  }
  // Locally the capacity ladders go past the VM's limits; shared DEV keeps them within its budgets.
  for (const [campaign, uploads, levels, batches, members, rates, devLevels] of [
    ['smoke', '2', '2', '2', '2', '2', '2'],
    ['capacity', undefined, '64', '256', '256', '32', '16'],
  ] as const) {
    const run = await runCampaign(t, '', plays, undefined, { campaign })
    equal(run.code, 0, run.log)
    // Locally, the bundled Stack is prepared before any runner, and the organization removed after the last.
    deepEqual(run.targets, ['cozy-prepare', 'upload-capacity', 'seed-org', 'shared-drive-ramp', 'org-drive-fanout',
      'seed-org-cleanup', 'seed-scale', 'mixed-capacity', 'seed-scale-cleanup'])
    deepEqual([setting(run.commands[1], 'LOAD_TARGET'), setting(run.commands[1], 'FILE_SIZE'),
      setting(run.commands[1], 'CAPACITY_MAX'), setting(run.commands[3], 'CAPACITY_MAX'),
      setting(run.commands[2], 'SEED_USERS'), setting(run.commands[4], 'CAPACITY_MAX')],
    ['local', '100K', uploads, levels, members, batches])
    const org = setting(run.commands[2], 'SEED_DOMAIN')!
    match(org, /^seed-[a-f0-9]{32}\./)
    deepEqual([setting(run.commands[3], 'ACTORS_DOMAIN'), setting(run.commands[4], 'FANOUT_ORG_DOMAIN'),
      setting(run.commands[5], 'SEED_DOMAIN')], [org, org, org])
    // The mixed search gets its own tree fixture, removed after it.
    const tree = setting(run.commands[6], 'SEED_DOMAIN')!
    match(tree, /^seed-[a-f0-9]{32}\./)
    ok(tree !== org)
    deepEqual([setting(run.commands[6], 'SEED_PROFILE'), setting(run.commands[7], 'MIXED_DOMAIN'),
      setting(run.commands[7], 'CAPACITY_MAX'), setting(run.commands[8], 'SEED_DOMAIN')], ['tree', tree, rates, tree])
    if (!hasFlock()) continue
    // On shared DEV, the agreed budgets apply and a smoke ladder stays below them.
    const dev = await runCampaign(t, '', plays, undefined, { campaign, env: {
      LOAD_TARGET: 'dev', DEV_LOCK_FILE: join(mkdtempSync(join(tmpdir(), 'dev-campaign-')), 'shared-dev.lock') } })
    equal(dev.code, 0, dev.log)
    deepEqual(dev.targets, ['upload-capacity', 'shared-drive-ramp', 'org-drive-fanout', 'mixed-capacity'])
    deepEqual(dev.commands.map(command => setting(command, 'LOAD_TARGET')), ['dev', 'dev', 'dev', 'dev'])
    deepEqual([setting(dev.commands[0], 'MAX_VUS'), setting(dev.commands[0], 'CAPACITY_MAX'),
      setting(dev.commands[0], 'MAX_UPLOAD_BYTES'), setting(dev.commands[1], 'CAPACITY_MAX'),
      setting(dev.commands[2], 'CAPACITY_MAX'), setting(dev.commands[3], 'CAPACITY_MAX'), setting(dev.commands[3], 'MAX_VUS')],
    ['128', uploads, '2560M', devLevels, devLevels, devLevels, '129'])
  }
})

test('a shared DEV campaign holds the lock once, through steps that reuse it', async t => {
  if (!hasFlock()) return t.skip('flock is not installed')
  const lock = join(mkdtempSync(join(tmpdir(), 'dev-campaign-')), 'shared-dev.lock')
  let outside: number | null = null
  const run = await runCampaign(t, producers, {
    fanout: { exit: 0, report: { ...fanout, capacity: { kind: 'lower_bound', lower: 4 }, status: 'passed' } },
    ramp: { exit: 0, report: ramp('lower_bound') },
    upload: { exit: 0, report: upload, sleep: 1 },
  }, undefined, {
    env: { LOAD_TARGET: 'dev', DEV_LOCK_FILE: lock },
    // Another run, even with the variable set, cannot borrow the campaign's lock.
    during: { after: 'upload', run: () => {
      outside = spawnSync('bash', [lockScript, lock, 'true'], { env: { ...process.env, DEV_LOCK_HELD: lock } }).status
    } },
  })
  equal(run.code, 0, run.log)
  equal(outside, 2)
  deepEqual(run.targets, ['upload-capacity', 'shared-drive-ramp', 'org-drive-fanout'])
  match(run.summary, /- Outcome: 0 \(completed\)\n- Topology: shared-dev\n.*\n- Limits: not measured on shared DEV/)
})

test('a shared DEV campaign does not start while another run holds the lock', async t => {
  if (!hasFlock()) return t.skip('flock is not installed')
  const lock = join(mkdtempSync(join(tmpdir(), 'dev-campaign-')), 'shared-dev.lock')
  const holder = spawn('bash', [lockScript, lock, 'sh', '-c', 'echo held; sleep 30'])
  t.after(() => holder.kill())
  await new Promise(resolve => holder.stdout.once('data', resolve))
  const run = await runCampaign(t, producers, {}, undefined, { env: { LOAD_TARGET: 'dev', DEV_LOCK_FILE: lock } })
  equal(run.code, 2, run.log)
  match(run.log, /Shared DEV is busy/)
  deepEqual(run.targets, [])
})
