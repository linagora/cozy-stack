import { randomFillSync } from 'node:crypto'
import {
  closeSync,
  existsSync,
  mkdirSync,
  openSync,
  renameSync,
  statSync,
  unlinkSync,
  writeSync,
} from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

import {
  getUploadDataSizeBytes,
  getUploadFixtureFilename,
  isUploadDataSize,
  uploadDataSizes,
  type UploadDataSize,
} from '../fixtures/upload-sizes.ts'

const MAXIMUM_BUFFER_SIZE = 1024 * 1024
const scriptDirectory = dirname(fileURLToPath(import.meta.url))
const defaultFixtureDirectory = resolve(scriptDirectory, '..', 'fixtures')

function getRequestedUploadSizes(requestedSize: string): UploadDataSize[] {
  if (requestedSize === 'all') {
    return [...uploadDataSizes]
  }

  if (!isUploadDataSize(requestedSize)) {
    throw new Error(
      `Unknown fixture size: ${requestedSize}. Expected one of: ${uploadDataSizes.join(' ')}`,
    )
  }

  return [requestedSize]
}

function saveRandomBytes(fileDescriptor: number, byteCount: number): number {
  const buffer = Buffer.allocUnsafe(Math.min(MAXIMUM_BUFFER_SIZE, byteCount))
  let totalBytesWritten = 0

  while (totalBytesWritten < byteCount) {
    const remainingBytes = byteCount - totalBytesWritten
    const currentChunkSize = Math.min(buffer.length, remainingBytes)
    const currentChunk = buffer.subarray(0, currentChunkSize)

    randomFillSync(currentChunk)

    let chunkBytesWritten = 0

    while (chunkBytesWritten < currentChunkSize) {
      chunkBytesWritten += writeSync(
        fileDescriptor,
        currentChunk,
        chunkBytesWritten,
        currentChunkSize - chunkBytesWritten,
      )
    }

    totalBytesWritten += currentChunkSize
  }

  return totalBytesWritten
}

function saveFixture(
  fixtureDirectory: string,
  size: UploadDataSize,
): 'created' | 'existing' {
  const expectedSize = getUploadDataSizeBytes(size)
  const fixturePath = join(fixtureDirectory, getUploadFixtureFilename(size))

  if (existsSync(fixturePath) && statSync(fixturePath).size === expectedSize) {
    process.stdout.write(`Fixture ${size} already exists: ${fixturePath}\n`)
    return 'existing'
  }

  const temporaryPath = `${fixturePath}.tmp.${process.pid}`
  let fileDescriptor: number | null = null

  process.stdout.write(`Generating ${size} upload fixture: ${fixturePath}\n`)

  try {
    fileDescriptor = openSync(temporaryPath, 'w', 0o600)
    const bytesWritten = saveRandomBytes(fileDescriptor, expectedSize)

    if (bytesWritten !== expectedSize) {
      throw new Error(
        `Fixture ${size} is incomplete: expected ${expectedSize} bytes, wrote ${bytesWritten}`,
      )
    }

    closeSync(fileDescriptor)
    fileDescriptor = null
    renameSync(temporaryPath, fixturePath)
    return 'created'
  } finally {
    if (fileDescriptor !== null) {
      closeSync(fileDescriptor)
    }
    if (existsSync(temporaryPath)) {
      unlinkSync(temporaryPath)
    }
  }
}

function runFixtureGenerator(): number {
  try {
    const requestedSize = process.argv[2] ?? 'all'
    const fixtureDirectory = process.env.FIXTURE_DIR ?? defaultFixtureDirectory
    const requestedUploadSizes = getRequestedUploadSizes(requestedSize)

    mkdirSync(fixtureDirectory, { recursive: true })

    for (const uploadSize of requestedUploadSizes) {
      saveFixture(fixtureDirectory, uploadSize)
    }

    return 0
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error)
    process.stderr.write(`${message}\n`)
    return 1
  }
}

process.exitCode = runFixtureGenerator()
