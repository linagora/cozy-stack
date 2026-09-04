import { createHash, randomUUID } from 'node:crypto'
import {
  createReadStream,
  createWriteStream,
  mkdtempSync,
  rmSync,
} from 'node:fs'
import { createServer } from 'node:http'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pipeline } from 'node:stream/promises'

interface StoredFile {
  directoryId: string
  md5sum: string
  name: string
  path: string
  size: number
}

interface StoredBody {
  md5sum: string
  size: number
}

interface RequestedRange {
  end: number
  start: number
}

const MOCK_PORT = 80
const storageDirectory = mkdtempSync(join(tmpdir(), 'twake-load-mock-'))
const directories = new Set<string>()
const storedFiles = new Map<string, StoredFile>()

function sendJson(
  response: ServerResponse,
  statusCode: number,
  value: unknown,
): boolean {
  const body = JSON.stringify(value)

  response.writeHead(statusCode, {
    'Content-Length': Buffer.byteLength(body),
    'Content-Type': 'application/vnd.api+json',
  })
  response.end(body)
  return true
}

function sendStatus(response: ServerResponse, statusCode: number): boolean {
  response.writeHead(statusCode)
  response.end()
  return true
}

function getPathSegments(url: URL): string[] {
  return url.pathname
    .split('/')
    .filter((segment): boolean => segment.length > 0)
    .map((segment): string => decodeURIComponent(segment))
}

function getDirectoryId(segments: readonly string[]): string | null {
  if (segments.length === 2 && segments[0] === 'files') {
    return segments[1] ?? null
  }

  if (
    segments.length === 4 &&
    segments[0] === 'sharings' &&
    segments[1] === 'drives'
  ) {
    return segments[3] ?? null
  }

  return null
}

function getTrashedDirectoryId(segments: readonly string[]): string | null {
  if (
    segments.length === 3 &&
    segments[0] === 'files' &&
    segments[1] === 'trash'
  ) {
    return segments[2] ?? null
  }

  if (
    segments.length === 5 &&
    segments[0] === 'sharings' &&
    segments[1] === 'drives' &&
    segments[3] === 'trash'
  ) {
    return segments[4] ?? null
  }

  return null
}

function getDownloadFileId(segments: readonly string[]): string | null {
  if (
    segments.length === 3 &&
    segments[0] === 'files' &&
    segments[1] === 'download'
  ) {
    return segments[2] ?? null
  }

  if (
    segments.length === 5 &&
    segments[0] === 'sharings' &&
    segments[1] === 'drives' &&
    segments[3] === 'download'
  ) {
    return segments[4] ?? null
  }

  return null
}

function getHeader(request: IncomingMessage, name: string): string | null {
  const value = request.headers[name]

  if (Array.isArray(value)) {
    return value[0] ?? null
  }

  return value ?? null
}

async function saveRequestBody(
  request: IncomingMessage,
  filePath: string,
): Promise<StoredBody> {
  const hasher = createHash('md5')
  let size = 0

  request.on('data', (chunk: Buffer): void => {
    hasher.update(chunk)
    size += chunk.byteLength
  })

  try {
    await pipeline(request, createWriteStream(filePath, { flags: 'wx' }))
  } catch (error: unknown) {
    rmSync(filePath, { force: true })
    throw new Error('could not persist mock upload', { cause: error })
  }

  return { md5sum: hasher.digest('base64'), size }
}

async function handleUpload(
  request: IncomingMessage,
  response: ServerResponse,
  directoryId: string,
  filename: string,
): Promise<boolean> {
  if (!directories.has(directoryId)) {
    return sendStatus(response, 404)
  }

  const fileId = `mock-file-${randomUUID()}`
  const filePath = join(storageDirectory, fileId)
  const storedBody = await saveRequestBody(request, filePath)
  const expectedMD5 = getHeader(request, 'content-md5')

  if (expectedMD5 !== null && expectedMD5 !== storedBody.md5sum) {
    rmSync(filePath, { force: true })
    return sendStatus(response, 412)
  }

  storedFiles.set(fileId, {
    directoryId,
    md5sum: storedBody.md5sum,
    name: filename,
    path: filePath,
    size: storedBody.size,
  })

  return sendJson(response, 201, {
    data: {
      attributes: {
        md5sum: storedBody.md5sum,
        name: filename,
        size: String(storedBody.size),
      },
      id: fileId,
      type: 'io.cozy.files',
    },
  })
}

function parseRange(rangeHeader: string, fileSize: number): RequestedRange | null {
  const match = /^bytes=([0-9]+)-([0-9]+)$/.exec(rangeHeader)

  if (match === null) {
    return null
  }

  const start = Number(match[1])
  const requestedEnd = Number(match[2])

  if (
    !Number.isSafeInteger(start) ||
    !Number.isSafeInteger(requestedEnd) ||
    start < 0 ||
    start >= fileSize ||
    requestedEnd < start
  ) {
    return null
  }

  return { end: Math.min(requestedEnd, fileSize - 1), start }
}

async function handleDownload(
  request: IncomingMessage,
  response: ServerResponse,
  fileId: string,
): Promise<boolean> {
  const file = storedFiles.get(fileId)

  if (file === undefined) {
    return sendStatus(response, 404)
  }

  const rangeHeader = getHeader(request, 'range')

  if (rangeHeader === null) {
    response.writeHead(200, {
      'Accept-Ranges': 'bytes',
      'Content-Length': file.size,
      'Content-Type': 'application/octet-stream',
    })
    await pipeline(createReadStream(file.path), response)
    return true
  }

  const range = parseRange(rangeHeader, file.size)

  if (range === null) {
    response.writeHead(416, { 'Content-Range': `bytes */${file.size}` })
    response.end()
    return true
  }

  response.writeHead(206, {
    'Accept-Ranges': 'bytes',
    'Content-Length': range.end - range.start + 1,
    'Content-Range': `bytes ${range.start}-${range.end}/${file.size}`,
    'Content-Type': 'application/octet-stream',
  })
  await pipeline(
    createReadStream(file.path, { end: range.end, start: range.start }),
    response,
  )
  return true
}

function destroyDirectory(directoryId: string): number {
  let deletedFiles = 0

  for (const [fileId, file] of storedFiles) {
    if (file.directoryId !== directoryId) {
      continue
    }

    rmSync(file.path, { force: true })
    storedFiles.delete(fileId)
    deletedFiles += 1
  }

  directories.delete(directoryId)
  return deletedFiles
}

async function handleRequest(
  request: IncomingMessage,
  response: ServerResponse,
): Promise<boolean> {
  const url = new URL(request.url ?? '/', 'http://mock')
  const segments = getPathSegments(url)

  if (
    request.method === 'GET' &&
    segments.length === 1 &&
    segments[0] === 'health'
  ) {
    return sendStatus(response, 200)
  }

  const downloadFileId = getDownloadFileId(segments)

  if (request.method === 'GET' && downloadFileId !== null) {
    return await handleDownload(request, response, downloadFileId)
  }

  const directoryId = getDirectoryId(segments)

  if (
    request.method === 'POST' &&
    directoryId !== null &&
    url.searchParams.get('Type') === 'directory'
  ) {
    const createdDirectoryId = `mock-folder-${randomUUID()}`

    directories.add(createdDirectoryId)
    return sendJson(response, 201, {
      data: { id: createdDirectoryId, type: 'io.cozy.files' },
    })
  }

  if (
    request.method === 'POST' &&
    directoryId !== null &&
    url.searchParams.get('Type') === 'file'
  ) {
    return await handleUpload(
      request,
      response,
      directoryId,
      url.searchParams.get('Name') ?? 'upload.bin',
    )
  }

  if (request.method === 'DELETE' && directoryId !== null) {
    return sendStatus(response, directories.has(directoryId) ? 200 : 404)
  }

  const trashedDirectoryId = getTrashedDirectoryId(segments)

  if (request.method === 'DELETE' && trashedDirectoryId !== null) {
    destroyDirectory(trashedDirectoryId)
    return sendStatus(response, 204)
  }

  return sendStatus(response, 404)
}

const server = createServer((request, response): void => {
  handleRequest(request, response).catch((error: unknown): void => {
    const normalizedError =
      error instanceof Error ? error : new Error(String(error))

    if (response.headersSent) {
      response.destroy(normalizedError)
      return
    }

    sendJson(response, 500, { error: normalizedError.message })
  })
})

server.listen(MOCK_PORT, '0.0.0.0', (): void => {
  process.stdout.write(`Twake load mock listening on port ${MOCK_PORT}\n`)
})

function stopServer(): void {
  server.close((): void => {
    rmSync(storageDirectory, { force: true, recursive: true })
  })
}

process.on('SIGINT', stopServer)
process.on('SIGTERM', stopServer)
