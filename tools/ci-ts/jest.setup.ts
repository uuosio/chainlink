import { getArgs, waitForService } from './test-helpers/common'

const { CHAINLINK_URL, EXTERNAL_ADAPTER_URL } = getArgs([
  'CHAINLINK_URL',
  'EXTERNAL_ADAPTER_URL',
])

beforeAll(async () => {
  await Promise.all([
    waitForService(CHAINLINK_URL),
    waitForService('http://node-2:6688'),
    waitForService(EXTERNAL_ADAPTER_URL),
    waitForService('http://external-adapter-2:6644'),
  ])
})
