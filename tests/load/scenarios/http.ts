import { check, sleep } from 'k6'
import http from 'k6/http'

import type { Options } from 'k6/options'

export const options: Options = {
  vus: Number(__ENV.K6_VUS || 1),
  duration: __ENV.K6_DURATION || '10s',
  thresholds: {
    checks: ['rate>0.99'],
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<1000'],
  },
}

// k6 requires a default export for the virtual-user entry point.
export default function runHttpScenario(): null {
  const baseUrl = __ENV.BASE_URL || 'http://mock'
  const response = http.get(`${baseUrl}/`, {
    tags: { operation: 'generic-http' },
  })

  check(response, {
    'status is successful': (result): boolean =>
      result.status >= 200 && result.status < 400,
  })

  sleep(1)
  return null
}
