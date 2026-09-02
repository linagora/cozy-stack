import assert from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  getOAuthClientIdFromJson,
  isMissingInstanceResult,
  isMissingOAuthClientResult,
  type CommandResult,
} from './provision-cozy-stack.ts'

function makeCommandResult(
  status: number,
  stderr: string,
  stdout = '',
): CommandResult {
  return { status, stderr, stdout }
}

describe('isMissingInstanceResult', (): void => {
  it('recognizes the instance-not-found response', (): void => {
    const result = makeCommandResult(
      1,
      'Error: Not Found: Instance not found\n',
    )

    assert.equal(isMissingInstanceResult(result), true)
  })

  it('preserves operational lookup failures', (): void => {
    const result = makeCommandResult(1, 'Error: connection refused\n')

    assert.equal(isMissingInstanceResult(result), false)
  })
})

describe('isMissingOAuthClientResult', (): void => {
  it('recognizes the expected missing-client response', (): void => {
    const softwareId = 'twake-drive-load-local'
    const result = makeCommandResult(
      1,
      `Error: Unqualified error: Could not find client with software_id ${softwareId}\n`,
    )

    assert.equal(isMissingOAuthClientResult(result, softwareId), true)
  })

  it('preserves operational lookup failures', (): void => {
    const result = makeCommandResult(1, 'Error: request timed out\n')

    assert.equal(
      isMissingOAuthClientResult(result, 'twake-drive-load-local'),
      false,
    )
  })
})

describe('getOAuthClientIdFromJson', (): void => {
  it('returns a client_id field', (): void => {
    assert.equal(
      getOAuthClientIdFromJson('{"client_id":"client-id"}'),
      'client-id',
    )
  })

  it('returns the _id field emitted by find-oauth-client', (): void => {
    assert.equal(
      getOAuthClientIdFromJson('{"_id":"stored-client-id"}'),
      'stored-client-id',
    )
  })

  it('rejects malformed successful output', (): void => {
    assert.equal(getOAuthClientIdFromJson('{"client_id":null}'), null)
    assert.equal(getOAuthClientIdFromJson('not-json'), null)
  })
})
