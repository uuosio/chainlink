export default {
  initiators: [
    {
      type: 'fluxmonitor',
      params: {
        address: '0x0000000000000000000000000000000000000000',
        requestData: {
          data: {
            coin: 'ETH',
            market: 'USD',
          },
        },
        feeds: [''],
        threshold: 5,
        pollingInterval: '5s',
        precision: 2,
      },
    },
  ],
  tasks: [
    {
      type: 'multiply',
      confirmations: null,
      params: {
        times: 100,
      },
    },
    {
      type: 'ethint256',
      confirmations: null,
      params: {},
    },
    {
      type: 'ethtx',
      confirmations: null,
      params: {},
    },
  ],
  startAt: null,
  endAt: null,
}
