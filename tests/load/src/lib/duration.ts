const DURATION_TOKEN_REGEX = /(\d+(?:\.\d+)?)(ms|s|m|h|d)/gi
const FULL_DURATION_REGEX = /^((?:\d+(?:\.\d+)?)(?:ms|s|m|h|d))+$/i

/**
 * Parses a k6 duration string (including compound durations like 1h30m, 1m30s, 500ms) into milliseconds.
 */
export function parseDurationToMs(duration: string): number {
  const trimmed = duration.trim().replace(/\s+/g, '')
  if (!trimmed || !FULL_DURATION_REGEX.test(trimmed)) {
    throw new Error(`Invalid duration string: "${duration}"`)
  }

  let totalMs = 0
  const matcher = new RegExp(DURATION_TOKEN_REGEX.source, 'gi')
  let match: RegExpExecArray | null
  while ((match = matcher.exec(trimmed)) !== null) {
    const [, amountStr = '0', unitStr = 's'] = match
    const value = parseFloat(amountStr)
    const unit = unitStr.toLowerCase()
    switch (unit) {
      case 'ms':
        totalMs += value
        break
      case 's':
        totalMs += value * 1000
        break
      case 'm':
        totalMs += value * 60 * 1000
        break
      case 'h':
        totalMs += value * 60 * 60 * 1000
        break
      case 'd':
        totalMs += value * 24 * 60 * 60 * 1000
        break
    }
  }

  return Math.round(totalMs)
}

