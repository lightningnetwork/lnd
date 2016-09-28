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

# Expose lnd ports (server, rpc).
EXPOSE 10011 10009

COPY "docker/lnd/start-lnd.sh" .
