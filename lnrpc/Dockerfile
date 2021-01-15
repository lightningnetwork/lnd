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

RUN cd /tmp \
  && export GO111MODULE=on \
  && go get github.com/golang/protobuf/protoc-gen-go@${PROTOC_GEN_VERSION} \
  && go get github.com/grpc-ecosystem/grpc-gateway/protoc-gen-grpc-gateway@${GRPC_GATEWAY_VERSION} \
  && go get github.com/grpc-ecosystem/grpc-gateway/protoc-gen-swagger@${GRPC_GATEWAY_VERSION}

WORKDIR /build

CMD ["/bin/bash", "/build/lnrpc/gen_protos.sh"]
