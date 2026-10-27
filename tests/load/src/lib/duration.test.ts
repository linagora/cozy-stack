import assert from 'node:assert/strict'
import test from 'node:test'

import { parseDurationToMs } from './duration.ts'

test('parseDurationToMs parses single unit durations', () => {
  assert.equal(parseDurationToMs('500ms'), 500)
  assert.equal(parseDurationToMs('30s'), 30000)
  assert.equal(parseDurationToMs('1.5s'), 1500)
  assert.equal(parseDurationToMs('2m'), 120000)
  assert.equal(parseDurationToMs('1h'), 3600000)
  assert.equal(parseDurationToMs('1d'), 86400000)
})

test('parseDurationToMs parses compound durations accepted by k6', () => {
  assert.equal(parseDurationToMs('1m30s'), 90000)
  assert.equal(parseDurationToMs('1h30m'), 5400000)
  assert.equal(parseDurationToMs('2m0s'), 120000)
  assert.equal(parseDurationToMs('1h15m30s500ms'), 4530500)
  assert.equal(parseDurationToMs('  1m  30s  '), 90000)
})

test('parseDurationToMs throws on invalid duration strings', () => {
  assert.throws(() => parseDurationToMs(''), /Invalid duration string/)
  assert.throws(() => parseDurationToMs('   '), /Invalid duration string/)
  assert.throws(() => parseDurationToMs('abc'), /Invalid duration string/)
  assert.throws(() => parseDurationToMs('10'), /Invalid duration string/)
  assert.throws(() => parseDurationToMs('1m30x'), /Invalid duration string/)
  assert.throws(() => parseDurationToMs('-5s'), /Invalid duration string/)
})
