import http from 'k6/http'

const ROOT_DIRECTORY_ID = 'io.cozy.files.root-dir'

function makeFileUrl(baseUrl, fileId) {
  return `${baseUrl.replace(/\/+$/, '')}/files/${encodeURIComponent(fileId)}`
}

function makeJsonApiHeaders(accessToken, contentType) {
  return {
    Accept: 'application/vnd.api+json',
    Authorization: `Bearer ${accessToken}`,
    'Content-Type': contentType,
  }
}

export function fetchCreatedTestDirectory({
  accessToken,
  baseUrl,
  directoryName,
  tags,
  timeout,
}) {
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

  const responseBody = response.json()
  const directoryId = responseBody?.data?.id

  if (typeof directoryId !== 'string' || directoryId.length === 0) {
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
}) {
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
}) {
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
}) {
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
