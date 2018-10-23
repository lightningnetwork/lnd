#!/bin/sh

protoc -I/usr/local/include -I. \
       -I$GOPATH/src \
       --go_out=plugins=grpc:. \
       signer.proto
