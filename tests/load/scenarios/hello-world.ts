import { check } from 'k6'
import http from 'k6/http'

import type { Options } from 'k6/options'

export const options: Options = {
  scenarios: {
    hello_world: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1,
    },
  },
  thresholds: {
    checks: ['rate==1'],
    http_req_failed: ['rate==0'],
    http_req_duration: ['p(95)<500'],
  },
}

// k6 requires a default export for the virtual-user entry point.
export default function runHelloWorldScenario(): null {
  const baseUrl = __ENV.BASE_URL || 'http://mock'
  const response = http.get(`${baseUrl}/`, {
    responseType: 'text',
    tags: { operation: 'hello-world' },
  })

  check(response, {
    'status is 200': (result): boolean => result.status === 200,
    'body contains marker': (result): boolean =>
      result.body.includes('Twake Drive load-test mock'),
  })

  return null
}
