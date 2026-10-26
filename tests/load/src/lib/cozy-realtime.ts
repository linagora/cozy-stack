export interface RealtimeAuthMessage {
  method: 'AUTH'
  payload: string
}

export interface RealtimeEventPayload {
  type: string
  id: string
  doc?: Record<string, unknown>
}

export interface RealtimeErrorPayload {
  status?: string
  code?: string
  title?: string
  source?: unknown
}

export interface RealtimeErrorMessage {
  event: 'error'
  payload?: RealtimeErrorPayload
}

export interface RealtimeEventMessage {
  event: string
  payload?: RealtimeEventPayload
}

export type RealtimeIncomingMessage =
  | RealtimeErrorMessage
  | RealtimeEventMessage

export function formatAuthMessage(token: string): string {
  return JSON.stringify({ method: 'AUTH', payload: token } satisfies RealtimeAuthMessage)
}

export function formatSubscribeMessage(doctype: string): string {
  return JSON.stringify({ method: 'SUBSCRIBE', payload: { type: doctype } })
}

export function parseRealtimeMessage(
  rawMessage: string,
): RealtimeIncomingMessage | null {
  try {
    const parsed = JSON.parse(rawMessage) as Record<string, unknown>
    if (!parsed || typeof parsed !== 'object') {
      return null
    }
    if (typeof parsed.event !== 'string') {
      return null
    }
    return parsed as unknown as RealtimeIncomingMessage
  } catch {
    return null
  }
}

export function getSharedDriveWsUrl(
  baseUrl: string,
  sharingId: string,
): string {
  const trimmed = baseUrl.trim().replace(/\/+$/, '')
  const wsBase = trimmed.startsWith('https://')
    ? trimmed.replace(/^https:\/\//, 'wss://')
    : trimmed.startsWith('http://')
      ? trimmed.replace(/^http:\/\//, 'ws://')
      : `ws://${trimmed}`

  return `${wsBase}/sharings/drives/${encodeURIComponent(sharingId)}/realtime`
}
