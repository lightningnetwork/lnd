#!/bin/sh

protoc -I . rpc.proto --go_out=plugins=grpc:.
