FROM golang:alpine as builder

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

RUN apk add --no-cache \
    git \
    make \
&&  git clone https://github.com/lightningnetwork/lnd /go/src/github.com/lightningnetwork/lnd \
&&  cd /go/src/github.com/lightningnetwork/lnd \
&&  make

FROM alpine as final

RUN apk --no-cache add \
    bash \
    ca-certificates

COPY --from=builder /go/src/github.com/lightningnetwork/lnd/lncli /bin/
COPY --from=builder /go/src/github.com/lightningnetwork/lnd/lnd /bin/
COPY "docker/lnd/start-lnd.sh" .

ENTRYPOINT ["/start-lnd.sh"]
CMD ["lncli"]
