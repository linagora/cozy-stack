import {
  isUploadDataSize,
  uploadDataSizes,
  type UploadDataSize,
} from './upload-sizes.ts'

const MUTATION_SIZE = 256
const configuredUploadDataSize = __ENV.FILE_SIZE || '1K'

if (!isUploadDataSize(configuredUploadDataSize)) {
  throw new Error(
    `Unknown FILE_SIZE ${configuredUploadDataSize}. Expected one of: ${uploadDataSizes.join(', ')}`,
  )
}

export const uploadDataSize: UploadDataSize = configuredUploadDataSize

const uploadData = open(
  `/upload-binaries/${uploadDataSize.toLowerCase()}.bin`,
  'b',
)
const uploadDataBytes = new Uint8Array(uploadData)

export function randomizeUploadData(marker: string): ArrayBuffer {
  const mutationLength = Math.min(MUTATION_SIZE, uploadDataBytes.length)
  const uniqueHeader = `twake-load:${marker}\n`
  let state = 2166136261

  for (let index = 0; index < marker.length; index += 1) {
    state ^= marker.charCodeAt(index)
    state = Math.imul(state, 16777619)
  }

  for (let index = 0; index < mutationLength; index += 1) {
    state ^= state << 13
    state ^= state >>> 17
    state ^= state << 5
    uploadDataBytes[index] =
      index < uniqueHeader.length
        ? uniqueHeader.charCodeAt(index) & 0xff
        : state & 0xff
  }

  return uploadData
}
