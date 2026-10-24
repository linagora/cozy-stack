import { deepEqual, equal, match, ok } from 'node:assert/strict'
import { spawn } from 'node:child_process'
import {
  chmodSync, copyFileSync, cpSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, symlinkSync, writeFileSync,
} from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { test, type TestContext } from 'node:test'
import { fileURLToPath } from 'node:url'

const harness = fileURLToPath(new URL('../../', import.meta.url))

// Plays one step from $FAKE_STEPS/<step>.*: setup failure before the producer,
// a delay, a graceful stop that logs each SIGTERM, the report it leaves and the
// producer's own exit, which make turns into 2.
const fakeMake = `#!/usr/bin/env bash
target=$1 id= record=
for arg in "$@"; do
  case $arg in
    CAPACITY_RUN_ID=* | EXECUTION_ID=*) id=\${arg#*=} ;;
    RECORD_EXIT=*) record=\${arg#*=} ;;
  esac
done
case $target in
  cozy-capacity) suffix=upload-capacity ;;
  cozy-shared-drive-ramp) suffix=shared-drive-ramp ;;
  org-drive-fanout) suffix=org-drive-fanout ;;
  *) suffix= ;;
esac
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
type Run = { code: number | null, log: string, signals: (step: string) => number, steps: string[][], summary: string }
type Interrupt = { after: string, delayMs: number }

async function runCampaign(
  t: TestContext, definition: string, plays: Record<string, Play>, interrupt?: Interrupt,
): Promise<Run> {
  const root = mkdtempSync(join(tmpdir(), 'run-campaign-'))
  t.after(() => rmSync(root, { recursive: true, force: true }))
  const [dir, bin, steps] = ['harness', 'bin', 'steps'].map(name => join(root, name)) as [string, string, string]
  mkdirSync(join(dir, 'campaigns', 'lib'), { recursive: true })
  mkdirSync(join(dir, 'data', 'results'), { recursive: true })
  mkdirSync(bin)
  mkdirSync(steps)
  for (const file of ['run-campaign.sh', 'lib/record-exit.sh']) {
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

  const child = spawn('bash', ['campaigns/run-campaign.sh', 'scenario'], {
    cwd: dir, env: { ...process.env, FAKE_STEPS: steps, PATH: `${bin}:${process.env.PATH}` },
  })
  let log = ''
  let interrupting = false
  child.stdout.on('data', (chunk: Buffer) => {
    log += chunk
    if (interrupt === undefined || interrupting || !log.includes(`START ${interrupt.after}`)) return
    interrupting = true
    setTimeout(() => child.kill('SIGTERM'), interrupt.delayMs)
  })
  child.stderr.on('data', (chunk: Buffer) => { log += chunk })
  const code = await new Promise<number | null>(done => child.on('close', done))

  const runs = join(dir, 'data', 'results', 'campaigns')
  const [run] = readdirSync(runs)
  ok(run, log)
  const read = (name: string): string => readFileSync(join(runs, run, name), 'utf8')
  return {
    code,
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
  }
}

const upload = { attempts: [{ cleanup_rate: 1, consistency_rate: 1 }], maximum_concurrent_uploads: 48,
  result_bound: 'exact', status: 'completed' }
const ramp = (bound: 'bracket' | 'lower_bound') => ({ levels: [], max_sustained_level: 8,
  result_bound: bound, status: 'completed' })
const fanout = { capacity: { kind: 'bracket', lower: 2, max: null, upper_exclusive: 4 }, points: [], status: 'failed' }
const producers = `upload_capacity upload FILE_SIZE=1M
shared_drive_ramp ramp RAMP_LEVELS=8,16
org_drive_fanout fanout FANOUT_LEVELS=2,4`
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
