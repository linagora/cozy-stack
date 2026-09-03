import { deepEqual, equal, throws } from 'node:assert/strict'
import { describe, it } from 'node:test'

import {
  getUploadTarget,
  makeUploadTargetDownloadUrl,
  makeUploadTargetFileUrl,
  makeUploadTargetTrashUrl,
} from './upload-target.ts'

describe('getUploadTarget', (): void => {
  it('defaults to the personal drive root', (): void => {
    deepEqual(getUploadTarget({}), {
      kind: 'personal',
      rootDirectoryId: 'io.cozy.files.root-dir',
    })
  })

  it('returns a configured shared-drive target', (): void => {
    deepEqual(
      getUploadTarget({
        UPLOAD_TARGET: 'shared-drive',
        SHARED_DRIVE_ID: 'drive-id',
        SHARED_DRIVE_ROOT_ID: 'root-id',
      }),
      {
        driveId: 'drive-id',
        kind: 'shared-drive',
        rootDirectoryId: 'root-id',
      },
    )
  })

  it('requires both shared-drive identifiers', (): void => {
    throws(
      (): void => {
        getUploadTarget({
          UPLOAD_TARGET: 'shared-drive',
          SHARED_DRIVE_ID: 'drive-id',
        })
      },
      /SHARED_DRIVE_ROOT_ID is required/,
    )
  })

  it('rejects unknown targets', (): void => {
    throws(
      (): void => {
        getUploadTarget({ UPLOAD_TARGET: 'unknown' })
      },
      /UPLOAD_TARGET must be personal or shared-drive/,
    )
  })
})

describe('shared-drive URLs', (): void => {
  const target = getUploadTarget({
    UPLOAD_TARGET: 'shared-drive',
    SHARED_DRIVE_ID: 'drive/id',
    SHARED_DRIVE_ROOT_ID: 'root-id',
  })

  it('builds a file route with encoded identifiers', (): void => {
    equal(
      makeUploadTargetFileUrl('https://load.example/', 'file/id', target),
      'https://load.example/sharings/drives/drive%2Fid/file%2Fid',
    )
  })

  it('builds a download route with encoded identifiers', (): void => {
    equal(
      makeUploadTargetDownloadUrl(
        'https://load.example/',
        'file/id',
        target,
      ),
      'https://load.example/sharings/drives/drive%2Fid/download/file%2Fid',
    )
  })

  it('builds a trash route with encoded identifiers', (): void => {
    equal(
      makeUploadTargetTrashUrl('https://load.example/', 'file/id', target),
      'https://load.example/sharings/drives/drive%2Fid/trash/file%2Fid',
    )
  })
})

describe('personal-drive URLs', (): void => {
  const target = getUploadTarget({})

  it('builds a file route', (): void => {
    equal(
      makeUploadTargetFileUrl('https://load.example/', 'file/id', target),
      'https://load.example/files/file%2Fid',
    )
  })

  it('builds a download route', (): void => {
    equal(
      makeUploadTargetDownloadUrl(
        'https://load.example/',
        'file/id',
        target,
      ),
      'https://load.example/files/download/file%2Fid',
    )
  })

  it('builds a trash route', (): void => {
    equal(
      makeUploadTargetTrashUrl('https://load.example/', 'file/id', target),
      'https://load.example/files/trash/file%2Fid',
    )
  })
})
