import { assert } from 'chai'
import fluxMonitorJob from '../fixtures/flux-monitor-job'
import {
  assertAsync,
  createProvider,
  fundAddress,
  txWait,
  wait,
} from '../test-helpers/common'
import ChainlinkClient from '../test-helpers/chainlink-cli'
import { contract, helpers as h, matchers } from '@chainlink/test-helpers'
import { FluxAggregatorFactory } from '../../../evm-contracts/ethers/v0.6/FluxAggregatorFactory'
import { JobSpec } from '../../../operator_ui/@types/operator_ui'
import 'isomorphic-unfetch'
import { ethers } from 'ethers'

const NODE_1_URL = 'http://node:6688'
const NODE_2_URL = 'http://node-2:6688'
const NODE_1_CONTAINER = 'chainlink-node'
const NODE_2_CONTAINER = 'chainlink-node-2'
const EA_1_URL = 'http://external-adapter:6644'
const EA_2_URL = 'http://external-adapter-2:6644'

const provider = createProvider()
const carol = ethers.Wallet.createRandom().connect(provider)
const linkTokenFactory = new contract.LinkTokenFactory(carol)
const fluxAggregatorFactory = new FluxAggregatorFactory(carol)
const deposit = h.toWei('1000')
const clClient1 = new ChainlinkClient(NODE_1_URL, NODE_1_CONTAINER)
const clClient2 = new ChainlinkClient(NODE_2_URL, NODE_2_CONTAINER)

let linkToken: contract.Instance<contract.LinkTokenFactory>
let fluxAggregator: contract.Instance<FluxAggregatorFactory>
let node1Address: string
let node2Address: string

async function changePriceFeed(adapter: string, value: number) {
  const url = new URL('result', adapter).href
  const response = await fetch(url, {
    method: 'PATCH',
    headers: {
      'Content-Type': 'application/json',
    },
    body: JSON.stringify({ result: value }),
  })
  assert(response.ok)
}

async function assertJobRun(
  clClient: ChainlinkClient,
  jobId: string,
  count: number,
  errorMessage: string,
) {
  await assertAsync(() => {
    const jobRuns = clClient.getJobRuns()
    const jobRun = jobRuns[jobRuns.length - 1]
    return (
      clClient.getJobRuns().length === count &&
      jobRun.status === 'completed' &&
      jobRun.jobId === jobId
    )
  }, errorMessage)
}

async function assertAggregatorValues(
  _la: number,
  _lr: number,
  _rr: number,
  _ls1: number,
  _ls2: number,
  _w1: number,
  _w2: number,
  message: string,
): Promise<void> {
  const [la, lr, rr, ls1, ls2, w1, w2] = await Promise.all([
    fluxAggregator.latestAnswer(),
    fluxAggregator.latestRound(),
    fluxAggregator.reportingRound(),
    fluxAggregator.latestSubmission(node1Address).then(res => res[1]),
    fluxAggregator.latestSubmission(node2Address).then(res => res[1]),
    fluxAggregator.withdrawable(node1Address),
    fluxAggregator.withdrawable(node2Address),
  ])
  matchers.bigNum(_la, la, `${message}: latest answer`)
  matchers.bigNum(_lr, lr, `${message}: latest round`)
  matchers.bigNum(_rr, rr, `${message}: reporting round`)
  matchers.bigNum(_ls1, ls1, `${message}: node 1 latest submission`)
  matchers.bigNum(_ls2, ls2, `${message}: node 2 latest submission`)
  matchers.bigNum(_w1, w1, `${message}: node 1 withdrawable amount`)
  matchers.bigNum(_w2, w2, `${message}: node 2 withdrawable amount`)
}

beforeAll(async () => {
  clClient1.login()
  clClient2.login()
  node1Address = clClient1.getAdminInfo()[0].address
  node2Address = clClient2.getAdminInfo()[0].address
  await fundAddress(carol.address)
  await fundAddress(node1Address)
  await fundAddress(node2Address)
  linkToken = await linkTokenFactory.deploy()
  await linkToken.deployed()
})

beforeEach(async () => {
  fluxAggregator = await fluxAggregatorFactory.deploy(
    linkToken.address,
    1,
    30,
    1,
    ethers.utils.formatBytes32String('ETH/USD'),
  )
  await Promise.all([
    fluxAggregator.deployed(),
    changePriceFeed(EA_1_URL, 100), // original price
    changePriceFeed(EA_2_URL, 100),
  ])
})

describe('FluxMonitor / FluxAggregator integration with one node', () => {
  let job: JobSpec

  afterEach(async () => {
    clClient1.archiveJob(job.id)
  })

  it('updates the price', async () => {
    await fluxAggregator
      .addOracle(node1Address, node1Address, 1, 1, 0)
      .then(txWait)
    await linkToken.transfer(fluxAggregator.address, deposit).then(txWait)
    await fluxAggregator.updateAvailableFunds().then(txWait)

    expect(await fluxAggregator.getOracles()).toEqual([node1Address])
    matchers.bigNum(
      await linkToken.balanceOf(fluxAggregator.address),
      deposit,
      'Unable to fund FluxAggregator',
    )

    const initialJobCount = clClient1.getJobs().length
    const initialRunCount = clClient1.getJobRuns().length

    // create FM job
    fluxMonitorJob.initiators[0].params.address = fluxAggregator.address
    fluxMonitorJob.initiators[0].params.feeds = [EA_1_URL]
    job = clClient1.createJob(JSON.stringify(fluxMonitorJob))
    assert.equal(clClient1.getJobs().length, initialJobCount + 1)

    // Job should trigger initial FM run
    await assertJobRun(
      clClient1,
      job.id,
      initialRunCount + 1,
      'initial job never run',
    )
    matchers.bigNum(10000, await fluxAggregator.latestAnswer())

    // Nominally change price feed
    await changePriceFeed(EA_1_URL, 101)
    await wait(10000)
    assert.equal(
      clClient1.getJobRuns().length,
      initialRunCount + 1,
      'Flux Monitor should not run job after nominal price deviation',
    )

    // Significantly change price feed
    await changePriceFeed(EA_1_URL, 110)
    await assertJobRun(
      clClient1,
      job.id,
      initialRunCount + 2,
      'second job never run',
    )
    matchers.bigNum(11000, await fluxAggregator.latestAnswer())
  })
})

describe('FluxMonitor / FluxAggregator integration with two nodes', () => {
  // let job: JobSpec

  it('updates the price', async () => {
    await fluxAggregator
      .addOracle(node1Address, node1Address, 1, 1, 0)
      .then(txWait)
    await fluxAggregator
      .addOracle(node2Address, node2Address, 2, 2, 0)
      .then(txWait)
    await linkToken.transfer(fluxAggregator.address, deposit).then(txWait)
    await fluxAggregator.updateAvailableFunds().then(txWait)

    expect(await fluxAggregator.getOracles()).toEqual([
      node1Address,
      node2Address,
    ])
    matchers.bigNum(
      await linkToken.balanceOf(fluxAggregator.address),
      deposit,
      'Unable to fund FluxAggregator',
    )

    const node1InitialJobCount = clClient1.getJobs().length
    const node1InitialRunCount = clClient1.getJobRuns().length
    const node2InitialJobCount = clClient2.getJobs().length
    const node2InitialRunCount = clClient2.getJobRuns().length

    // TODO reset flux monitor job b/t tests (re-read from file?)
    fluxMonitorJob.initiators[0].params.address = fluxAggregator.address
    fluxMonitorJob.initiators[0].params.feeds = [EA_1_URL]
    const job1 = clClient1.createJob(JSON.stringify(fluxMonitorJob))
    fluxMonitorJob.initiators[0].params.feeds = [EA_2_URL]
    const job2 = clClient2.createJob(JSON.stringify(fluxMonitorJob))

    assert.equal(clClient1.getJobs().length, node1InitialJobCount + 1)
    assert.equal(clClient2.getJobs().length, node2InitialJobCount + 1)

    await assertJobRun(
      clClient1,
      job1.id,
      node1InitialRunCount + 1,
      'initial update never run by node 1',
    )
    await assertJobRun(
      clClient2,
      job2.id,
      node2InitialRunCount + 1,
      'initial update never run by node 2',
    )

    await assertAggregatorValues(10000, 1, 1, 1, 1, 1, 1, 'initial round')

    clClient2.pause()
    await changePriceFeed(EA_1_URL, 110)
    await changePriceFeed(EA_2_URL, 120)

    await assertJobRun(
      clClient1,
      job1.id,
      node1InitialRunCount + 2,
      "node 1's second update not run",
    )
    await assertAggregatorValues(10000, 1, 2, 2, 1, 2, 1, 'node 1 answers')

    clClient2.unpause()

    await assertJobRun(
      clClient2,
      job2.id,
      node2InitialRunCount + 2,
      "node 2's second update not run",
    )
    await assertAggregatorValues(11500, 2, 2, 2, 2, 2, , 'second round')
  })
})
