import { deepEqual, equal, throws } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  makeByteRange,
  parseUploadedFile,
} from '../lib/upload-consistency.ts'

describe('parseUploadedFile', (): void => {
  it('parses Cozy file metadata with a string size', (): void => {
    deepEqual(
      parseUploadedFile(
        JSON.stringify({
          data: {
            attributes: {
              md5sum: 'rL0Y20zC+Fzt72VPzMSk2A==',
              name: 'load-100k.bin',
              size: '102400',
            },
            id: 'file-id',
          },
        }),
      ),
      {
        id: 'file-id',
        md5sum: 'rL0Y20zC+Fzt72VPzMSk2A==',
        name: 'load-100k.bin',
        size: 102400,
      },
    )
  })

  it('rejects malformed JSON', (): void => {
    throws((): void => {
      parseUploadedFile('not-json')
    }, /not valid JSON/)
  })

  it('rejects incomplete file metadata', (): void => {
    throws((): void => {
      parseUploadedFile('{"data":{"id":"file-id","attributes":{}}}')
    }, /data.attributes.size is invalid/)
  })
})

describe('makeByteRange', (): void => {
  it('returns one complete range for a small file', (): void => {
    deepEqual(makeByteRange(0, 1024, 16 * 1024 * 1024), {
      end: 1023,
      header: 'bytes=0-1023',
      length: 1024,
      start: 0,
    })
  })

  it('caps the final range at the file size', (): void => {
    deepEqual(makeByteRange(16, 20, 8), {
      end: 19,
      header: 'bytes=16-19',
      length: 4,
      start: 16,
    })
  })

  it('returns null after the full file has been read', (): void => {
    equal(makeByteRange(20, 20, 8), null)
  })

  it('rejects a range beyond the file', (): void => {
    throws((): void => {
      makeByteRange(21, 20, 8)
    }, /cannot exceed/)
  })
})
