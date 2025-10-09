FROM golang:1.24.6-alpine

ENV GOFLAGS="-buildvcs=false"

RUN apk update && apk add --no-cache git

WORKDIR /tmp/tools

COPY . ./

RUN CGO_ENABLED=0 go install -trimpath github.com/golangci/golangci-lint/cmd/golangci-lint \
  && CGO_ENABLED=0 golangci-lint custom \
  && mv ./custom-gcl /usr/local/bin/custom-gcl \
  && git config --global --add safe.directory /build \
  && rm -rf /go/pkg/mod \
  && rm -rf /root/.cache/go-build \
  && rm -rf /var/cache/apk/* \
  && rm -rf /tmp/*

WORKDIR /build
