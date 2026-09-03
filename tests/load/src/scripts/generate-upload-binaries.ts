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
  getUploadBinaryFilename,
  isUploadDataSize,
  uploadDataSizes,
  type UploadDataSize,
} from '../fixtures/upload-sizes.ts'

const MAXIMUM_BUFFER_SIZE = 1024 * 1024
const scriptDirectory = dirname(fileURLToPath(import.meta.url))
const defaultUploadBinaryDirectory = resolve(
  scriptDirectory,
  '..',
  '..',
  'data',
  'upload-binaries',
)

function getRequestedUploadSizes(requestedSize: string): UploadDataSize[] {
  if (requestedSize === 'all') {
    return [...uploadDataSizes]
  }

  if (!isUploadDataSize(requestedSize)) {
    throw new Error(
      `Unknown upload binary size: ${requestedSize}. Expected one of: ${uploadDataSizes.join(' ')}`,
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

function ensureUploadBinary(
  uploadBinaryDirectory: string,
  size: UploadDataSize,
): 'created' | 'existing' {
  const expectedSize = getUploadDataSizeBytes(size)
  const uploadBinaryPath = join(
    uploadBinaryDirectory,
    getUploadBinaryFilename(size),
  )

  if (
    existsSync(uploadBinaryPath) &&
    statSync(uploadBinaryPath).size === expectedSize
  ) {
    process.stdout.write(
      `Upload binary ${size} already exists: ${uploadBinaryPath}\n`,
    )
    return 'existing'
  }

  const temporaryPath = `${uploadBinaryPath}.tmp.${process.pid}`
  let fileDescriptor: number | null = null

  process.stdout.write(
    `Generating ${size} upload binary: ${uploadBinaryPath}\n`,
  )

  try {
    fileDescriptor = openSync(temporaryPath, 'w', 0o600)
    const bytesWritten = saveRandomBytes(fileDescriptor, expectedSize)

    if (bytesWritten !== expectedSize) {
      throw new Error(
        `Upload binary ${size} is incomplete: expected ${expectedSize} bytes, wrote ${bytesWritten}`,
      )
    }

    closeSync(fileDescriptor)
    fileDescriptor = null
    renameSync(temporaryPath, uploadBinaryPath)
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

function ensureRequestedUploadBinaries(): number {
  try {
    const requestedSize = process.argv[2] ?? 'all'
    const uploadBinaryDirectory =
      process.env.UPLOAD_BINARY_DIR ?? defaultUploadBinaryDirectory
    const requestedUploadSizes = getRequestedUploadSizes(requestedSize)

    mkdirSync(uploadBinaryDirectory, { recursive: true })

    for (const uploadSize of requestedUploadSizes) {
      ensureUploadBinary(uploadBinaryDirectory, uploadSize)
    }

    return 0
  } catch (error: unknown) {
    const message = error instanceof Error ? error.message : String(error)
    process.stderr.write(`${message}\n`)
    return 1
  }
}

process.exitCode = ensureRequestedUploadBinaries()
