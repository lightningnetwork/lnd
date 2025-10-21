#!/bin/bash

set -e

# Directory of the script file, independent of where it's called from.
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# golang docker image version used in this script.
GO_IMAGE=docker.io/library/golang:1.25.3-alpine

PROTOBUF_VERSION=$(docker run --rm -v $DIR/../:/lnd -w /lnd $GO_IMAGE \
	go list -f '{{.Version}}' -m google.golang.org/protobuf)
GRPC_GATEWAY_VERSION=$(docker run --rm -v $DIR/../:/lnd -w /lnd $GO_IMAGE \
	go list -f '{{.Version}}' -m github.com/grpc-ecosystem/grpc-gateway/v2)

echo "Building protobuf compiler docker image..."
docker build -t lnd-protobuf-builder \
	--build-arg PROTOBUF_VERSION="$PROTOBUF_VERSION" \
	--build-arg GRPC_GATEWAY_VERSION="$GRPC_GATEWAY_VERSION" \
	.

echo "Compiling and formatting *.proto files..."
docker run \
	--rm \
	--user "$UID:$(id -g)" \
	-e UID=$UID \
	-e COMPILE_MOBILE \
	-e SUBSERVER_PREFIX \
	-v "$DIR/../:/build" \
	lnd-protobuf-builder
