// Prepares a target for the exact-work runner. Only preparation, credentials and coordination
// differ between targets; budgets, execution, verification and cleanup are shared.
import { readFileSync } from 'node:fs'

import { getUploadTarget } from '../../lib/upload-target.ts'
import { call, check, parse, stackClient, type Session, type Target } from './exact-work.ts'

type Env = Record<string, string | undefined>

function setting(env: Env, name: string): string {
  const value = env[name]
  check(value, `${name} is required`)
  return value
}

// A local Stack, the bundled mock or any target reached with a token from the environment.
export function localTarget(env: Env): Target {
  const baseUrl = env.BASE_URL || 'http://mock'
  const upload = getUploadTarget(env)
  const token = env.COZY_ACCESS_TOKEN || undefined
  // shortcut: the mock is unreachable from the host and no route lists a shared drive's trash, so
  // those runs rely on k6's own teardown check; list them too once a run must prove that cleanup.
  const listable = baseUrl !== 'http://mock' && upload.kind === 'personal'
  check(!listable || token, `COZY_ACCESS_TOKEN is required to verify cleanup on ${baseUrl}`)
  // STACK_URL is where the host reaches the Stack when k6 uses a Compose-internal name.
  const stack = listable ? stackClient(env.STACK_URL || baseUrl, new URL(baseUrl).host, token!) : null
  return {
    kind: 'local',
    baseUrl,
    makeArgs: [`UPLOAD_TARGET=${upload.kind}`, ...(upload.kind === 'shared-drive'
      ? [`SHARED_DRIVE_ID=${upload.driveId}`, `SHARED_DRIVE_ROOT_ID=${upload.rootDirectoryId}`] : [])],
    // Local runs hold no lock, so they never stop another run's k6.
    lockedUrls: [],
    maxPointMs: Infinity,
    session: async () => ({ token, stack }),
  }
}

// One instance of the Cloudery fixture manifest (SEEDING.md), with tokens minted for it alone.
// Each k6 run gets a fresh 30-minute instance token and must end well before it expires.
export async function devTarget(env: Env): Promise<Target> {
  check(env.MAX_VUS && env.MAX_UPLOAD_BYTES && env.MAX_RUNTIME,
    'Shared DEV needs MAX_VUS, MAX_UPLOAD_BYTES and MAX_RUNTIME agreed with the DEV owner')
  check((env.UPLOAD_TARGET || 'personal') === 'personal', 'Shared DEV uploads to the personal drive only')
  const fqdn = setting(env, 'DEV_INSTANCE')
  const fixture = parse(readFileSync(setting(env, 'SEED_CLOUDERY_MANIFEST'), 'utf8'))
  // An organization manifest names its instances fqdn, a tree fixture manifest (fixture-manifest.ts) domain.
  const name = (i: { fqdn?: string, domain?: string }) => i.fqdn ?? i.domain
  const entry = fixture.instances?.find((i: { fqdn?: string }) => name(i) === fqdn)
  check(entry?.uuid, `${fqdn} is not in the fixture manifest`)
  const secrets = parse(readFileSync(setting(env, 'SEED_CLOUDERY_TOKEN_FILE'), 'utf8'))
  check(secrets.public && secrets.manager_url, 'The token file needs public and manager_url')

  const session = async (): Promise<Session> => {
    const { token } = await call(new URL(`/api/public/instances/${fqdn}/token`, secrets.manager_url), 'POST', [200],
      { Authorization: `Bearer ${secrets.public}`, 'Content-Type': 'application/json' },
      JSON.stringify({ audience: 'cli', scope: 'io.cozy.files io.cozy.settings' }))
    check(typeof token === 'string' && token, `Invalid token for ${fqdn}`)
    // DEV_STACK_URL is a connection origin for tests; the instance stays the Host.
    return { token, stack: stackClient(env.DEV_STACK_URL || `https://${fqdn}`, fqdn, token) }
  }
  const { stack } = await session()
  const settings = (await stack!('GET', '/settings/instance', [200])).data?.attributes
  check(settings?.uuid === entry.uuid, `${fqdn} is no longer the fixture instance in the manifest`)
  return {
    kind: 'dev',
    baseUrl: `https://${fqdn}`,
    makeArgs: ['UPLOAD_TARGET=personal'],
    // One lock covers the whole shared DEV fixture.
    lockedUrls: (fixture.instances as Array<{ fqdn?: string }>).map(i => `https://${name(i)}`),
    maxPointMs: 20 * 60_000,
    session,
  }
}
