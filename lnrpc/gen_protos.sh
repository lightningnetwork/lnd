#!/bin/bash

set -e

# generate compiles the *.pb.go stubs from the *.proto files.
function generate() {
  echo "Generating root gRPC server protos"

  PROTOS="lightning.proto walletunlocker.proto stateservice.proto **/*.proto"

  # For each of the sub-servers, we then generate their protos, but a restricted
  # set as they don't yet require REST proxies, or swagger docs.
  for file in $PROTOS; do
    DIRECTORY=$(dirname "${file}")
    echo "Generating protos from ${file}, into ${DIRECTORY}"

    # Generate the protos.
    protoc -I/usr/local/include -I. \
      --go_out . --go_opt paths=source_relative \
      --go-grpc_out . --go-grpc_opt paths=source_relative \
      "${file}"

    # Generate the REST reverse proxy.
    annotationsFile=${file//proto/yaml}
    protoc -I/usr/local/include -I. \
      --grpc-gateway_out . \
      --grpc-gateway_opt logtostderr=true \
      --grpc-gateway_opt paths=source_relative \
      --grpc-gateway_opt grpc_api_configuration=${annotationsFile} \
      "${file}"

    # Generate the swagger file which describes the REST API in detail.
    protoc -I/usr/local/include -I. \
      --openapiv2_out . \
      --openapiv2_opt logtostderr=true \
      --openapiv2_opt grpc_api_configuration=${annotationsFile} \
      --openapiv2_opt json_names_for_fields=false \
      "${file}"
  done
  
  # Generate the JSON/WASM client stubs.
  falafel=$(which falafel)
  pkg="lnrpc"
  opts="package_name=$pkg,js_stubs=1,build_tags=// +build js"
  protoc -I/usr/local/include -I. -I.. \
    --plugin=protoc-gen-custom=$falafel\
    --custom_out=. \
    --custom_opt="$opts" \
    lightning.proto stateservice.proto walletunlocker.proto
  
  PACKAGES="autopilotrpc chainrpc invoicesrpc peersrpc routerrpc signrpc verrpc walletrpc watchtowerrpc wtclientrpc devrpc"
  for package in $PACKAGES; do
    # Special import for the wallet kit.
    manual_import=""
    if [[ "$package" == "walletrpc" ]]; then
      manual_import="github.com/lightningnetwork/lnd/lnrpc/signrpc"
    fi

    # Special import for devrpc.
    if [[ "$package" == "devrpc" ]]; then
        manual_import="github.com/lightningnetwork/lnd/lnrpc"
    fi

    opts="package_name=$package,manual_import=$manual_import,js_stubs=1,build_tags=// +build js"
    pushd $package
    protoc -I/usr/local/include -I. -I.. \
      --plugin=protoc-gen-custom=$falafel\
      --custom_out=. \
      --custom_opt="$opts" \
      "$(find . -name '*.proto')"
    popd
  done
}

# format formats the *.proto files with the clang-format utility.
function format() {
  find . -name "*.proto" -print0 | xargs -0 clang-format --style=file -i
}

# Compile and format the lnrpc package.
pushd lnrpc
format
generate
popd

if [[ "$COMPILE_MOBILE" == "1" ]]; then
  pushd mobile
  ./gen_bindings.sh $FALAFEL_VERSION
  popd
fi
