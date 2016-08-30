FROM golang:1.7

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.network>

# TODO(roasbeef): just mount a volume from the build context to the GOPATH?
ADD . /go/src/github.com/lightningnetwork/lnd
WORKDIR /go/src/github.com/lightningnetwork/lnd

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

RUN go build
RUN go install . ./cmd/...

# Mount a volume where btcd's RPC credentials are stored. We'll need to read
# the TLS cert from this directory.
VOLUME ["/rpc"]

VOLUME ["/data"]

# Expose the p2p listening port, and the current RPC port.
EXPOSE 10009 10011

COPY docker/lnd/lnd-start.sh /

# Finally, execute the shell script that will start lnd. We use a shell script
# rather than executing the command directly with ENTRYPOINT in order to ensure
# environment variables get properly substitued.
ENTRYPOINT ["/lnd-start.sh"]
