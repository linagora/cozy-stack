export const uploadDataSizes = [
  '1K',
  '100K',
  '1M',
  '10M',
  '100M',
  '1G',
] as const

export type UploadDataSize = (typeof uploadDataSizes)[number]

const uploadDataSizeBytes: Record<UploadDataSize, number> = {
  '1K': 1024,
  '100K': 100 * 1024,
  '1M': 1024 * 1024,
  '10M': 10 * 1024 * 1024,
  '100M': 100 * 1024 * 1024,
  '1G': 1024 * 1024 * 1024,
}

export function isUploadDataSize(value: string): value is UploadDataSize {
  return uploadDataSizes.some((size: UploadDataSize): boolean => size === value)
}

export function getUploadDataSizeBytes(size: UploadDataSize): number {
  return uploadDataSizeBytes[size]
}

export function getUploadBinaryFilename(size: UploadDataSize): string {
  return `${size.toLowerCase()}.bin`
}
