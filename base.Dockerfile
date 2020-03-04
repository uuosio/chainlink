FROM smartcontract/builder:1.0.30
WORKDIR /chainlink
COPY . . 
RUN yarn
RUN go mod download
RUN yarn setup
RUN make install-chainlink
RUN yarn workspace @chainlink/explorer-client build