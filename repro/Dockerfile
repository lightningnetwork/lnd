# Integration test dockerfile

FROM golang:1.13-alpine

RUN apk add --no-cache git gcc musl-dev make curl bash jq

RUN GO111MODULE=on go get github.com/btcsuite/btcd@v0.20.1-beta
RUN GO111MODULE=on go get github.com/btcsuite/btcd/cmd/btcctl@v0.20.1-beta

# Copy compiled lnd binaries. Assumption here is that the binary is up to date.
COPY lnd-debug /root/lnd
RUN chmod +x /root/lnd

COPY lncli-debug /root/lncli
RUN chmod +x /root/lncli

# Copy the test script into the image.
COPY repro/*.sh /root/
RUN chmod +x /root/*.sh

ENTRYPOINT [ "/root/test.sh" ]
