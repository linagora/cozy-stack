import http from 'k6/http'

import type { JSONObject, JSONValue } from 'k6'
import type { RequestBody, Response } from 'k6/http'

const ROOT_DIRECTORY_ID = 'io.cozy.files.root-dir'

export type RunTags = Record<string, string>

export type TestDirectory = {
  id: string
  name: string
}

type RequestContext = {
  accessToken: string
  baseUrl: string
  tags: RunTags
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
  filename: string
}

function makeFileUrl(baseUrl: string, fileId: string): string {
  return `${baseUrl.replace(/\/+$/, '')}/files/${encodeURIComponent(fileId)}`
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
  timeout,
}: DirectoryCreationRequest): TestDirectory {
  const response = http.post(
    `${makeFileUrl(baseUrl, ROOT_DIRECTORY_ID)}?Type=directory&Name=${encodeURIComponent(directoryName)}`,
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
  directoryId,
  filename,
  tags,
  timeout,
}: UploadRequest): Response {
  return http.post(
    `${makeFileUrl(baseUrl, directoryId)}?Type=file&Name=${encodeURIComponent(filename)}`,
    body,
    {
      headers: makeJsonApiHeaders(accessToken, 'application/octet-stream'),
      tags: {
        ...tags,
        name: 'POST /files/:dir-id',
        operation: 'file-upload',
      },
      timeout,
    },
  )
}

export function fetchTrashResponse({
  accessToken,
  baseUrl,
  directoryId,
  tags,
  timeout,
}: DirectoryRequest): Response {
  return http.del(makeFileUrl(baseUrl, directoryId), null, {
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
  timeout,
}: DirectoryRequest): Response {
  return http.del(
    `${baseUrl.replace(/\/+$/, '')}/files/trash/${encodeURIComponent(directoryId)}`,
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
