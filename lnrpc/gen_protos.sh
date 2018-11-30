#!/bin/sh

echo "Generating root gRPC server protos"

# Generate the protos.
protoc -I/usr/local/include -I. \
       -I$GOPATH/src \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --go_out=plugins=grpc:. \
       rpc.proto

# Generate the REST reverse proxy.
protoc -I/usr/local/include -I. \
       -I$GOPATH/src \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --grpc-gateway_out=logtostderr=true:. \
       rpc.proto

# Finally, generate the swagger file which describes the REST API in detail.
protoc -I/usr/local/include -I. \
       -I$GOPATH/src \
       -I$GOPATH/src/github.com/grpc-ecosystem/grpc-gateway/third_party/googleapis \
       --swagger_out=logtostderr=true:. \
       rpc.proto

# For each of the sub-servers, we then generate their protos, but a restricted
# set as they don't yet require REST proxies, or swagger docs.
for file in **/*.proto
do
    DIRECTORY=$(dirname ${file})
    echo "Generating protos from ${file}, into ${DIRECTORY}"

    protoc -I/usr/local/include -I. \
           -I$GOPATH/src \
           --go_out=plugins=grpc:. \
           ${file}
done
