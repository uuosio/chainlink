FROM henrynguyen5/base:1.0.3 as builder

ENV PATH /chainlink/node_modules/.bin:$PATH

ADD . .

RUN yarn install
RUN yarn setup:explorer
RUN yarn workspace @chainlink/explorer-client build

FROM node:10.16.3-alpine

WORKDIR /app

COPY --from=builder /chainlink/package.json package.json
COPY --from=builder /chainlink/yarn.lock yarn.lock
COPY --from=builder /chainlink/node_modules node_modules
COPY --from=builder /chainlink/explorer/node_modules explorer/node_modules
COPY --from=builder /chainlink/explorer/package.json explorer/package.json
COPY --from=builder /chainlink/explorer/src explorer/src
COPY --from=builder /chainlink/explorer/tsconfig.json explorer/tsconfig.json
COPY --from=builder /chainlink/tsconfig.cjs.json ./
COPY --from=builder /chainlink/explorer/ormconfig.json explorer/ormconfig.json
COPY --from=builder /chainlink/explorer/client/public explorer/client/public
COPY --from=builder /chainlink/explorer/client/build explorer/client/build
COPY --from=builder /chainlink/styleguide styleguide
COPY --from=builder /chainlink/tools/json-api-client tools/json-api-client
COPY --from=builder /chainlink/tools/redux tools/redux

ENV NODE_ENV production
ENTRYPOINT [ "yarn", "workspace", "@chainlink/explorer", "run", "prod" ]
