#!/usr/bin/env bash

# Abort on error (-e) and print commands (-v).
set -ev

# See README.md in lnrpc why we need these specific versions/commits.
PROTOC_VERSION=3.4.0
PROTOBUF_VERSION="v1.3.2"
GENPROTO_VERSION="20e1ac93f88cf06d2b1defb90b9e9e126c7dfff6"
GRPC_GATEWAY_VERSION="v1.14.3"

# This script is specific to Travis CI so we only need to support linux x64.
PROTOC_URL="https://github.com/protocolbuffers/protobuf/releases/download/v${PROTOC_VERSION}/protoc-${PROTOC_VERSION}-linux-x86_64.zip"
PROTOC_DL_CACHE_DIR="${DOWNLOAD_CACHE:-/tmp/download_cache}/protoc"

# install_protoc copies the cached protoc binary to the $PATH or downloads it
# if no cached version is found.
install_protoc() {
  if [ -f "${PROTOC_DL_CACHE_DIR}/bin/protoc" ]; then
    echo "Using cached version of protoc"
  else
    wget -O /tmp/protoc.zip $PROTOC_URL
    mkdir -p "${PROTOC_DL_CACHE_DIR}"
    unzip -o /tmp/protoc.zip -d "${PROTOC_DL_CACHE_DIR}"
    chmod -R a+rx "${PROTOC_DL_CACHE_DIR}/"
  fi
  sudo cp "${PROTOC_DL_CACHE_DIR}/bin/protoc" /usr/local/bin
  sudo cp -r "${PROTOC_DL_CACHE_DIR}/include" /usr/local
}

# install_protobuf downloads and compiles the Golang protobuf library that
# encodes/decodes all protobuf messages from/to Go structs.
install_protobuf() {
  local install_path="$GOPATH/src/github.com/golang/protobuf"
  if [ ! -d "$install_path" ]; then
    git clone https://github.com/golang/protobuf "$install_path"
  fi
  pushd "$install_path"
  git reset --hard master && git checkout master && git pull
  git reset --hard $PROTOBUF_VERSION
  make
  popd
}

# install_genproto downloads the Golang protobuf generator that converts the
# .proto files into Go interface stubs.
install_genproto() {
  local install_path="$GOPATH/src/google.golang.org/genproto"
  if [ ! -d "$install_path" ]; then
    git clone https://github.com/google/go-genproto "$install_path"
  fi
  pushd "$install_path"
  git reset --hard master && git checkout master && git pull
  git reset --hard $GENPROTO_VERSION
  popd
}

# install_grpc_gateway downloads and installs the gRPC gateway that converts
# .proto files into REST gateway code.
install_grpc_gateway() {
  local install_path="$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway"
  if [ ! -d "$install_path" ]; then
    git clone https://github.com/grpc-ecosystem/grpc-gateway "$install_path"
  fi
  pushd "$install_path"
  git reset --hard master && git checkout master && git pull
  git reset --hard $GRPC_GATEWAY_VERSION
  GO111MODULE=on go install ./protoc-gen-grpc-gateway ./protoc-gen-swagger
  popd
}

install_protoc
install_protobuf
install_genproto
install_grpc_gateway
