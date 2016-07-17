FROM golang:1.6.2

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.network>

# Grab and install the latest version of lnd and all related dependencies.
# TODO(roasbeef): replace with glide install
RUN go get -u -v github.com/lightningnetwork/lnd/...

VOLUME ["/data"]

# Expose the p2p listening port, and the current RPC port.
EXPOSE 10009 10011

ENTRYPOINT ["/go/bin/lnd", "--datadir=/data", "--logdir=/data", "--segnet", "--btcdhost=btcd", "--rpccert=/data/rpc.cert"]
