FROM golang:1.8

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.network>

# Expose mainnet ports (server, rpc)
EXPOSE 8333 8334

# Expose testnet ports (server, rpc)
EXPOSE 18333 18334

# Expose simnet ports (server, rpc)
EXPOSE 18555 18556

# Expose segnet ports (server, rpc)
EXPOSE 28901 28902

# Grab and install the latest version of roasbeef's fork of btcd and all
# related dependencies.
RUN go get -u github.com/Masterminds/glide

WORKDIR $GOPATH/src/github.com/roasbeef/btcd
RUN git clone https://github.com/roasbeef/btcd .
RUN glide install
RUN go install . ./cmd/...

RUN mkdir "/rpc" "/root/.btcd" "/root/.btcctl"
RUN touch "/root/.btcd/btcd.conf"

# Manually generate certificate and add all domains, it is needed to connect
# "btcctl" and "lnd" to "btcd" over docker links.
RUN "/go/bin/gencerts" --host="*" --directory="/rpc" --force

# Create a volume to house pregenerated RPC credentials. This will be
# shared with any lnd, btcctl containers so they can securely query btcd's RPC
# server.
# You should NOT do this before certificate generation!
# Otherwise manually generated certificate will be overriden with shared
# mounted volume! For more info read dockerfile "VOLUME" documentation.
VOLUME ["/rpc"]

COPY "start-btcctl.sh" .
COPY "start-btcd.sh" .

RUN chmod +x start-btcctl.sh
RUN chmod +x start-btcd.sh

