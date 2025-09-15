FROM golang:1.25.3

RUN apt-get update && apt-get install -y git
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache

COPY . /tmp/tools

RUN cd /tmp \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && cd /tmp/tools \
  && CGO_ENABLED=0 go tool github.com/golangci/golangci-lint/v2/cmd/golangci-lint custom \
  && mv ./custom-gcl /usr/local/bin/custom-gcl \
  && chmod -R 777 /tmp/build/ \
  && git config --global --add safe.directory /build

WORKDIR /build
