const ROOT_DIRECTORY_ID = 'io.cozy.files.root-dir'

export type UploadTargetKind = 'personal' | 'shared-drive'

export interface PersonalUploadTarget {
  kind: 'personal'
  rootDirectoryId: string
}

export interface SharedDriveUploadTarget {
  driveId: string
  kind: 'shared-drive'
  rootDirectoryId: string
}

export type UploadTarget = PersonalUploadTarget | SharedDriveUploadTarget

export type UploadTargetEnvironment = Record<string, string | undefined>

function getRequiredEnvironmentValue(
  environment: UploadTargetEnvironment,
  environmentName: string,
): string {
  const value = environment[environmentName]

  if (!value) {
    throw new Error(`${environmentName} is required for shared-drive uploads`)
  }

  return value
}

export function getUploadTarget(
  environment: UploadTargetEnvironment,
): UploadTarget {
  const kind = environment.UPLOAD_TARGET || 'personal'

  if (kind === 'personal') {
    return {
      kind,
      rootDirectoryId: ROOT_DIRECTORY_ID,
    }
  }

  if (kind !== 'shared-drive') {
    throw new Error('UPLOAD_TARGET must be personal or shared-drive')
  }

  return {
    driveId: getRequiredEnvironmentValue(environment, 'SHARED_DRIVE_ID'),
    kind,
    rootDirectoryId: getRequiredEnvironmentValue(
      environment,
      'SHARED_DRIVE_ROOT_ID',
    ),
  }
}

function normalizeBaseUrl(baseUrl: string): string {
  return baseUrl.replace(/\/+$/, '')
}

export function makeUploadTargetFileUrl(
  baseUrl: string,
  fileId: string,
  target: UploadTarget,
): string {
  const encodedFileId = encodeURIComponent(fileId)

  if (target.kind === 'personal') {
    return `${normalizeBaseUrl(baseUrl)}/files/${encodedFileId}`
  }

  return `${normalizeBaseUrl(baseUrl)}/sharings/drives/${encodeURIComponent(target.driveId)}/${encodedFileId}`
}

export function makeUploadTargetTrashUrl(
  baseUrl: string,
  fileId: string,
  target: UploadTarget,
): string {
  const encodedFileId = encodeURIComponent(fileId)

  if (target.kind === 'personal') {
    return `${normalizeBaseUrl(baseUrl)}/files/trash/${encodedFileId}`
  }

  return `${normalizeBaseUrl(baseUrl)}/sharings/drives/${encodeURIComponent(target.driveId)}/trash/${encodedFileId}`
}
