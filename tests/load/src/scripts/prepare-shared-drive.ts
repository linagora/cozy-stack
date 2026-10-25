import { existsSync, readFileSync } from 'node:fs'

import { getEnvironmentFilePath, saveEnvironmentFile } from './env.ts'

export const DEFAULT_DRIVE_NAME = 'Load-Test Shared Drive'

export interface SharedDriveInfo {
  sharingId: string
  rootDirectoryId: string
  description: string
}

export interface APISharingItem {
  id: string
  attributes?: {
    description?: string
    active?: boolean
    owner?: boolean
    drive_root_type?: string
    rules?: Array<{
      title?: string
      values?: string[]
    }>
  }
}

export async function fetchSharedDrives({
  baseUrl,
  accessToken,
}: {
  baseUrl: string
  accessToken: string
}): Promise<APISharingItem[]> {
  const url = `${baseUrl.replace(/\/+$/, '')}/sharings/drives`
  const res = await fetch(url, {
    headers: {
      Authorization: `Bearer ${accessToken}`,
      Accept: 'application/json',
    },
  })

  if (!res.ok) {
    throw new Error(`Failed to list shared drives: HTTP ${res.status} ${res.statusText}`)
  }

  const json = (await res.json()) as { data?: APISharingItem[] }
  return Array.isArray(json.data) ? json.data : []
}

export async function createSharedDrive({
  baseUrl,
  accessToken,
  driveName = DEFAULT_DRIVE_NAME,
  fetcher = fetch,
}: {
  baseUrl: string
  accessToken: string
  driveName?: string | undefined
  fetcher?: typeof fetch
}): Promise<SharedDriveInfo> {
  const url = `${baseUrl.replace(/\/+$/, '')}/sharings/drives`
  const res = await fetcher(url, {
    method: 'POST',
    headers: {
      Authorization: `Bearer ${accessToken}`,
      'Content-Type': 'application/vnd.api+json',
      Accept: 'application/vnd.api+json, application/json',
    },
    body: JSON.stringify({
      data: {
        type: 'io.cozy.sharings',
        attributes: {
          name: driveName,
          description: driveName,
        },
      },
    }),
  })

  if (!res.ok) {
    const text = await res.text()
    throw new Error(`Failed to create shared drive: HTTP ${res.status} ${text}`)
  }

  const json = (await res.json()) as { data: APISharingItem }
  const rootDirectoryId = json.data.attributes?.rules?.[0]?.values?.[0]
  if (!rootDirectoryId) {
    throw new Error(`Created shared drive ${json.data.id} has no root directory ID`)
  }

  return {
    sharingId: json.data.id,
    rootDirectoryId,
    description: json.data.attributes?.description || driveName,
  }
}

export async function ensureSharedDrive({
  baseUrl,
  accessToken,
  driveName = DEFAULT_DRIVE_NAME,
}: {
  baseUrl: string
  accessToken: string
  driveName?: string | undefined
}): Promise<SharedDriveInfo> {
  const drives = await fetchSharedDrives({ baseUrl, accessToken })
  const match = drives.find(
    (d) =>
      d.attributes?.description === driveName &&
      d.attributes?.active === true &&
      d.attributes?.owner === true &&
      d.attributes?.drive_root_type === 'directory' &&
      Boolean(d.attributes?.rules?.[0]?.values?.[0]),
  )

  if (match) {
    const rootDirectoryId = match.attributes?.rules?.[0]?.values?.[0] as string
    return {
      sharingId: match.id,
      rootDirectoryId,
      description: match.attributes?.description || driveName,
    }
  }

  return createSharedDrive({ baseUrl, accessToken, driveName })
}

export function updateEnvFileWithSharedDrive(
  envFilePath: string,
  drive: SharedDriveInfo,
): void {
  saveEnvironmentFile(envFilePath, {
    SHARED_DRIVE_ID: drive.sharingId,
    SHARED_DRIVE_ROOT_ID: drive.rootDirectoryId,
  })
}

async function main(): Promise<void> {
  const envPath = getEnvironmentFilePath()
  const envContent = existsSync(envPath) ? readFileSync(envPath, 'utf8') : ''
  const envMap = Object.fromEntries(
    envContent
      .split(/\r?\n/)
      .filter((l) => l && !l.startsWith('#') && l.includes('='))
      .map((l) => {
        const eq = l.indexOf('=')
        return [l.slice(0, eq).trim(), l.slice(eq + 1).trim()]
      }),
  )

  const accessToken =
    process.env.COZY_ACCESS_TOKEN || envMap.COZY_ACCESS_TOKEN
  if (!accessToken) {
    throw new Error('COZY_ACCESS_TOKEN is required to prepare shared drive')
  }

  const instanceDomain =
    process.env.COZY_INSTANCE_DOMAIN ||
    envMap.COZY_INSTANCE_DOMAIN ||
    'load.localhost:8080'
  const baseUrl =
    process.env.COZY_BASE_URL ||
    envMap.COZY_BASE_URL ||
    process.env.BASE_URL ||
    envMap.BASE_URL ||
    `http://${instanceDomain}`

  const driveName = process.env.SHARED_DRIVE_NAME || DEFAULT_DRIVE_NAME

  console.log(`Ensuring shared drive "${driveName}" at ${baseUrl}...`)
  const drive = await ensureSharedDrive({
    baseUrl,
    accessToken,
    driveName,
  })

  console.log(`Shared drive ready: ID=${drive.sharingId}, RootDir=${drive.rootDirectoryId}`)
  updateEnvFileWithSharedDrive(envPath, drive)
  console.log(`Updated ${envPath} with SHARED_DRIVE_ID and SHARED_DRIVE_ROOT_ID`)
}

if (process.argv[1]?.endsWith('prepare-shared-drive.ts')) {
  main().catch((err) => {
    console.error(err)
    process.exit(1)
  })
}
