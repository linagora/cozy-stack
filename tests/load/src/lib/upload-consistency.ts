type JsonRecord = Record<string, unknown>

export interface UploadedFile {
  id: string
  md5sum: string
  name: string
  size: number
}

export interface ByteRange {
  end: number
  header: string
  length: number
  start: number
}

function isJsonRecord(value: unknown): value is JsonRecord {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function getRequiredString(
  record: JsonRecord,
  fieldName: string,
  fieldPath: string,
): string {
  const value = record[fieldName]

  if (typeof value !== 'string' || value.length === 0) {
    throw new Error(`${fieldPath} must be a non-empty string`)
  }

  return value
}

function getFileSize(value: unknown): number | null {
  if (typeof value === 'number') {
    return Number.isSafeInteger(value) && value >= 0 ? value : null
  }

  if (typeof value !== 'string' || !/^[0-9]+$/.test(value)) {
    return null
  }

  const size = Number(value)
  return Number.isSafeInteger(size) ? size : null
}

export function parseUploadedFile(responseBody: string): UploadedFile {
  let document: unknown

  try {
    document = JSON.parse(responseBody)
  } catch (error: unknown) {
    throw new Error('upload response is not valid JSON', { cause: error })
  }

  if (!isJsonRecord(document) || !isJsonRecord(document.data)) {
    throw new Error('upload response has no data object')
  }

  const { data } = document

  if (!isJsonRecord(data.attributes)) {
    throw new Error('upload response has no data.attributes object')
  }

  const size = getFileSize(data.attributes.size)

  if (size === null) {
    throw new Error('upload response data.attributes.size is invalid')
  }

  return {
    id: getRequiredString(data, 'id', 'upload response data.id'),
    md5sum: getRequiredString(
      data.attributes,
      'md5sum',
      'upload response data.attributes.md5sum',
    ),
    name: getRequiredString(
      data.attributes,
      'name',
      'upload response data.attributes.name',
    ),
    size,
  }
}

export function makeByteRange(
  start: number,
  totalSize: number,
  chunkSize: number,
): ByteRange | null {
  if (!Number.isSafeInteger(start) || start < 0) {
    throw new Error('range start must be a non-negative safe integer')
  }
  if (!Number.isSafeInteger(totalSize) || totalSize < 1) {
    throw new Error('total size must be a positive safe integer')
  }
  if (!Number.isSafeInteger(chunkSize) || chunkSize < 1) {
    throw new Error('chunk size must be a positive safe integer')
  }
  if (start === totalSize) {
    return null
  }
  if (start > totalSize) {
    throw new Error('range start cannot exceed total size')
  }

  const end = Math.min(start + chunkSize, totalSize) - 1

  return {
    end,
    header: `bytes=${start}-${end}`,
    length: end - start + 1,
    start,
  }
}
