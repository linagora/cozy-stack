const SUPPORTED_UPLOAD_SIZES = ['1K', '100K', '1M', '10M', '100M', '1G']
const MUTATION_SIZE = 256

export const uploadDataSize = __ENV.FILE_SIZE || '1K'

if (!SUPPORTED_UPLOAD_SIZES.includes(uploadDataSize)) {
  throw new Error(
    `Unknown FILE_SIZE ${uploadDataSize}. Expected one of: ${SUPPORTED_UPLOAD_SIZES.join(', ')}`
  )
}

const uploadData = open(`/fixtures/${uploadDataSize.toLowerCase()}.bin`, 'b')
const uploadDataBytes = new Uint8Array(uploadData)

export function randomizeUploadData(marker) {
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
