#!/bin/sh

echo "Generating root gRPC server protos"

PROTOS="rpc.proto walletunlocker.proto **/*.proto"

# For each of the sub-servers, we then generate their protos, but a restricted
# set as they don't yet require REST proxies, or swagger docs.
for file in $PROTOS; do
  DIRECTORY=$(dirname "${file}")
  echo "Generating protos from ${file}, into ${DIRECTORY}"

  # Generate the protos.
  protoc -I/usr/local/include -I. \
    --go_out=plugins=grpc,paths=source_relative:. \
    "${file}"

  # Generate the REST reverse proxy.
  protoc -I/usr/local/include -I. \
    --grpc-gateway_out=logtostderr=true,paths=source_relative,grpc_api_configuration=rest-annotations.yaml:. \
    "${file}"


  # Finally, generate the swagger file which describes the REST API in detail.
  protoc -I/usr/local/include -I. \
    --swagger_out=logtostderr=true,grpc_api_configuration=rest-annotations.yaml:. \
    "${file}"
done
