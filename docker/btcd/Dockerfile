FROM golang:1.6.2

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.network>

# Grab and install the latest version of roasbeef's fork of btcd and all
# related dependencies.
RUN go get -u -v github.com/roasbeef/btcd/...

# Expose the mainnet, testnet, simnet, and segnet listening ports.
EXPOSE 8333 18333 18335 28901

# Expose the mainnet, testnet, simnet, and segnet rpc ports.
EXPOSE 8333 18333 18336 28902

VOLUME ["/data"]

RUN mkdir /root/.btcd && mkdir /root/.btcctl

# Generate an automatic RPC conf.
ADD initrpc.go /root/
WORKDIR /root
RUN go build -o gen-config && ./gen-config

# TODO(roabeef): ENV or prog to parse --no-tls?
ENTRYPOINT ["/go/bin/btcd", "--datadir=/data", "--logdir=/data", "--segnet", "--rpccert=/data/rpc.cert", "--rpckey=/data/rpc.key"]
