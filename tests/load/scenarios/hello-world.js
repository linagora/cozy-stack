import http from 'k6/http'
import { check } from 'k6'

export const options = {
  scenarios: {
    hello_world: {
      executor: 'shared-iterations',
      vus: 1,
      iterations: 1
    }
  },
  thresholds: {
    checks: ['rate==1'],
    http_req_failed: ['rate==0'],
    http_req_duration: ['p(95)<500']
  }
}

// k6 executes the default export as the virtual-user entry point.
export default function runHelloWorldScenario() {
  const baseUrl = __ENV.BASE_URL || 'http://mock'
  const response = http.get(`${baseUrl}/`, {
    tags: { operation: 'hello-world' }
  })

  check(response, {
    'status is 200': result => result.status === 200,
    'body contains marker': result =>
      result.body.includes('Twake Drive load-test mock')
  })
}
