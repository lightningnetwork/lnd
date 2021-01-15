FROM golang:1.15.6-buster

RUN apt-get update && apt-get install -y \
  git \
  protobuf-compiler='3.6.1*' \
  clang-format='1:7.0*'

# We don't want any default values for these variables to make sure they're
# explicitly provided by parsing the go.mod file. Otherwise we might forget to
# update them here if we bump the versions.
ARG PROTOC_GEN_VERSION
ARG GRPC_GATEWAY_VERSION

ENV FALAFEL_VERSION="v0.7.1"
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache

RUN cd /tmp \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && export GO111MODULE=on \
  && go get github.com/golang/protobuf/protoc-gen-go@${PROTOC_GEN_VERSION} \
  && go get github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway@${GRPC_GATEWAY_VERSION} \
  && go get github.com/grpc-ecosystem/grpc-gateway/protoc-gen-swagger@${GRPC_GATEWAY_VERSION} \
  && go get github.com/lightninglabs/falafel@${FALAFEL_VERSION} \
  && go get golang.org/x/tools/cmd/goimports \
  && chmod -R 777 /tmp/build/

WORKDIR /build

CMD ["/bin/bash", "/build/lnrpc/gen_protos.sh"]
