import http from 'k6/http'

import type { JSONObject, JSONValue } from 'k6'
import type { RefinedResponse, RequestBody, Response } from 'k6/http'

import {
  makeUploadTargetDownloadUrl,
  makeUploadTargetFileUrl,
  makeUploadTargetTrashUrl,
  type UploadTarget,
} from './upload-target.ts'

export type RunTags = Record<string, string>

export type TestDirectory = {
  id: string
  name: string
}

type RequestContext = {
  accessToken: string
  baseUrl: string
  tags: RunTags
  target: UploadTarget
  timeout: string
}

type DirectoryCreationRequest = RequestContext & {
  directoryName: string
}

type DirectoryRequest = RequestContext & {
  directoryId: string
}

type UploadRequest = DirectoryRequest & {
  body: RequestBody
  contentMD5: string
  filename: string
}

type DownloadRangeRequest = RequestContext & {
  end: number
  fileId: string
  start: number
}

function makeJsonApiHeaders(
  accessToken: string,
  contentType: string,
): Record<string, string> {
  return {
    Accept: 'application/vnd.api+json',
    Authorization: `Bearer ${accessToken}`,
    'Content-Type': contentType,
  }
}

function isJsonObject(value: unknown): value is JSONObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function getDirectoryId(responseBody: JSONValue): string | null {
  if (!isJsonObject(responseBody)) {
    return null
  }

  const data = responseBody.data

  if (!isJsonObject(data)) {
    return null
  }

  return typeof data.id === 'string' && data.id.length > 0 ? data.id : null
}

export function fetchCreatedTestDirectory({
  accessToken,
  baseUrl,
  directoryName,
  tags,
  target,
  timeout,
}: DirectoryCreationRequest): TestDirectory {
  const response = http.post(
    `${makeUploadTargetFileUrl(baseUrl, target.rootDirectoryId, target)}?Type=directory&Name=${encodeURIComponent(directoryName)}`,
    null,
    {
      headers: makeJsonApiHeaders(accessToken, 'application/json'),
      responseType: 'text',
      tags: {
        ...tags,
        name: 'POST /files/:dir-id',
        operation: 'test-directory-create',
      },
      timeout,
    },
  )

  if (response.status !== 201) {
    throw new Error(
      `Could not create load-test directory: expected status 201, got ${response.status}`,
    )
  }

  const directoryId = getDirectoryId(response.json())

  if (directoryId === null) {
    throw new Error(
      'Could not create load-test directory: response has no data.id',
    )
  }

  return { id: directoryId, name: directoryName }
}

export function fetchUploadResponse({
  accessToken,
  baseUrl,
  body,
  contentMD5,
  directoryId,
  filename,
  tags,
  target,
  timeout,
}: UploadRequest): RefinedResponse<'text'> {
  return http.post(
    `${makeUploadTargetFileUrl(baseUrl, directoryId, target)}?Type=file&Name=${encodeURIComponent(filename)}`,
    body,
    {
      headers: {
        ...makeJsonApiHeaders(accessToken, 'application/octet-stream'),
        'Content-MD5': contentMD5,
      },
      responseType: 'text',
      tags: {
        ...tags,
        name: 'POST /files/:dir-id',
        operation: 'file-upload',
      },
      timeout,
    },
  )
}

export function fetchDownloadRangeResponse({
  accessToken,
  baseUrl,
  end,
  fileId,
  start,
  tags,
  target,
  timeout,
}: DownloadRangeRequest): RefinedResponse<'binary'> {
  return http.get(makeUploadTargetDownloadUrl(baseUrl, fileId, target), {
    headers: {
      Authorization: `Bearer ${accessToken}`,
      Range: `bytes=${start}-${end}`,
    },
    responseType: 'binary',
    tags: {
      ...tags,
      name: 'GET /files/download/:file-id',
      operation: 'file-consistency-download',
    },
    timeout,
  })
}

export function fetchTrashResponse({
  accessToken,
  baseUrl,
  directoryId,
  tags,
  target,
  timeout,
}: DirectoryRequest): Response {
  return http.del(makeUploadTargetFileUrl(baseUrl, directoryId, target), null, {
    headers: makeJsonApiHeaders(accessToken, 'application/json'),
    tags: {
      ...tags,
      name: 'DELETE /files/:id',
      operation: 'test-directory-trash',
    },
    timeout,
  })
}

export function fetchDestroyResponse({
  accessToken,
  baseUrl,
  directoryId,
  tags,
  target,
  timeout,
}: DirectoryRequest): Response {
  return http.del(
    makeUploadTargetTrashUrl(baseUrl, directoryId, target),
    null,
    {
      headers: makeJsonApiHeaders(accessToken, 'application/json'),
      tags: {
        ...tags,
        name: 'DELETE /files/trash/:id',
        operation: 'test-directory-destroy',
      },
      timeout,
    },
  )
}
