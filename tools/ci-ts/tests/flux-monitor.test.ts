import { assert } from 'chai'
import _fluxMonitorJob from '../fixtures/flux-monitor-job'
import {
  assertAsync,
  createProvider,
  fundAddress,
  txWait,
  wait,
} from '../test-helpers/common'
import ChainlinkClient from '../test-helpers/chainlink-cli'
import { contract, helpers as h, matchers } from '@chainlink/test-helpers'
import { FluxAggregatorFactory } from '@chainlink/contracts/ethers/v0.6/FluxAggregatorFactory'
import 'isomorphic-unfetch'
import { ethers } from 'ethers'
import { cloneDeep } from 'lodash'

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
const clClient1 = new ChainlinkClient('node 1', NODE_1_URL, NODE_1_CONTAINER)
const clClient2 = new ChainlinkClient('node 2', NODE_2_URL, NODE_2_CONTAINER)

// TODO how to import JobSpecRequest from operator_ui/@types/core/store/models.d.ts
let fluxMonitorJob: any
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
  count: number,
  errorMessage: string,
) {
  console.log('running assertJobRun')
  await assertAsync(() => {
    console.log('getJobRuns...')
    const jobRuns = clClient.getJobRuns()
    const jobRun = jobRuns[jobRuns.length - 1]
    console.log('checking equality')
    return jobRuns.length === count && jobRun.status === 'completed'
  }, `${errorMessage} : job not run on ${clClient.name}`)
}

async function assertAggregatorValues(
  latestAnswer: number,
  latestRound: number,
  reportingRound: number,
  latestSubmission1: number,
  latestSubmission2: number,
  msg: string,
): Promise<void> {
  console.log('asserting agg values')
  const [la, lr, rr, ls1, ls2] = await Promise.all([
    fluxAggregator.latestAnswer(),
    fluxAggregator.latestRound(),
    fluxAggregator.reportingRound(),
    fluxAggregator.latestSubmission(node1Address).then(res => res[1]),
    fluxAggregator.latestSubmission(node2Address).then(res => res[1]),
  ])
  console.log('running matchers in assert agg values')
  matchers.bigNum(latestAnswer, la, `${msg} : latest answer`)
  matchers.bigNum(latestRound, lr, `${msg} : latest round`)
  matchers.bigNum(reportingRound, rr, `${msg} : reporting round`)
  matchers.bigNum(latestSubmission1, ls1, `${msg} : node 1 latest submission`)
  matchers.bigNum(latestSubmission2, ls2, `${msg} : node 2 latest submission`)
}

beforeAll(async () => {
  console.log('before all')
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
  console.log('before each')
  fluxMonitorJob = cloneDeep(_fluxMonitorJob)
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

afterEach(() => {
  clClient1.unpause()
  clClient2.unpause()
  clClient1.getJobs().forEach(job => clClient1.archiveJob(job.id))
  clClient2.getJobs().forEach(job => clClient2.archiveJob(job.id))
})

describe('FluxMonitor / FluxAggregator integration with one node', () => {
  it('updates the price', async () => {
    console.log('test # 1')
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
    clClient1.createJob(JSON.stringify(fluxMonitorJob))
    assert.equal(clClient1.getJobs().length, initialJobCount + 1)

    // Job should trigger initial FM run
    await assertJobRun(clClient1, initialRunCount + 1, 'initial update')
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
    await assertJobRun(clClient1, initialRunCount + 2, 'second update')
    matchers.bigNum(11000, await fluxAggregator.latestAnswer())
  })
})

describe('FluxMonitor / FluxAggregator integration with two nodes', () => {
  it.only('updates the price', async () => {
    console.log('test # 2')
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
    console.log('test # 2 done setup')

    const node1InitialRunCount = clClient1.getJobRuns().length
    const node2InitialRunCount = clClient2.getJobRuns().length

    // TODO reset flux monitor job b/t tests (re-read from file?)
    fluxMonitorJob.initiators[0].params.address = fluxAggregator.address
    fluxMonitorJob.initiators[0].params.feeds = [EA_1_URL]
    clClient1.createJob(JSON.stringify(fluxMonitorJob))
    fluxMonitorJob.initiators[0].params.feeds = [EA_2_URL]
    clClient2.createJob(JSON.stringify(fluxMonitorJob))
    console.log('test # 2 created jobs')

    // initial job run
    await assertJobRun(clClient1, node1InitialRunCount + 1, 'initial update')
    await assertJobRun(clClient2, node2InitialRunCount + 1, 'initial update')
    await assertAggregatorValues(10000, 1, 1, 1, 1, 'initial round')
    console.log('test # 2 initial round')

    // node 1 should still begin round even with unresponsive node 2
    clClient2.pause()
    await changePriceFeed(EA_1_URL, 110)
    await changePriceFeed(EA_2_URL, 120)
    await assertJobRun(clClient1, node1InitialRunCount + 2, 'second update')
    await assertAggregatorValues(10000, 1, 2, 2, 1, 'node 1 only')
    console.log('test # 2 node 1 only')

    // node 2 should finish round
    clClient2.unpause()
    await assertJobRun(clClient2, node2InitialRunCount + 2, 'second update')
    await assertAggregatorValues(11500, 2, 2, 2, 2, 'second round')
    console.log('test # 2 second round')

    // change contract to min 1 oracle, node 1 should finish round without node 2
    clClient2.pause()
    console.log('test # 2 paused node 2')
    await fluxAggregator.updateFutureRounds(1, 1, 2, 0, 5)
    console.log('updated rounds')
    await changePriceFeed(EA_1_URL, 130)
    await assertJobRun(clClient1, node1InitialRunCount + 3, 'third update')
    await assertAggregatorValues(13000, 3, 3, 3, 2, 'third round')
    console.log('test # 2 third round')

    await changePriceFeed(EA_1_URL, 140)
    console.log('test # 2 changed price feed')
    await assertJobRun(clClient1, node1InitialRunCount + 4, 'fourth update')
    console.log('test # 2 asserted job run...')
    await assertAggregatorValues(14000, 4, 4, 4, 2, 'fourth round')
    console.log('test # 2 fourth round')
  })

  it('respects the idleThreshold', async () => {
    console.log('test # 3')
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
    console.log('test # 3 setup done')

    await fluxAggregator.updateFutureRounds(1, 2, 2, 0, 10)

    const node1InitialRunCount = clClient1.getJobRuns().length
    const node2InitialRunCount = clClient2.getJobRuns().length
    console.log('test # 3 created jobs')

    fluxMonitorJob.initiators[0].params.idleThreshold = '10s'
    fluxMonitorJob.initiators[0].params.address = fluxAggregator.address

    fluxMonitorJob.initiators[0].params.feeds = [EA_1_URL]
    const job1 = clClient1.createJob(JSON.stringify(fluxMonitorJob))
    fluxMonitorJob.initiators[0].params.feeds = [EA_2_URL]
    const job2 = clClient2.createJob(JSON.stringify(fluxMonitorJob))
    console.log('test # 3 added job')

    // initial job run
    await assertJobRun(clClient1, node1InitialRunCount + 1, 'initial update')
    await assertJobRun(clClient2, node2InitialRunCount + 1, 'initial update')
    await assertAggregatorValues(10000, 1, 1, 1, 1, 'initial round')
    console.log('test # 3 initial job')

    // second job run
    await assertJobRun(clClient1, node1InitialRunCount + 2, 'second update')
    await assertJobRun(clClient2, node2InitialRunCount + 2, 'second update')
    await assertAggregatorValues(10000, 2, 2, 2, 2, 'second round')
    console.log('test # 3 second job')
    // third job run without node 2
    clClient2.pause()
    await assertJobRun(clClient1, node1InitialRunCount + 3, 'third update')
    await assertAggregatorValues(10000, 2, 3, 3, 2, 'third round')
    console.log('test # 3 third job')

    // archive
    clClient1.archiveJob(job1.id)
    clClient2.archiveJob(job2.id)
  })
})
