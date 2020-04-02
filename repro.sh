#!/bin/bash
set -e

# Build lnd
GOOS=linux CGO_ENABLED=0 make

# Build docker image.
docker build -t repro -f repro/Dockerfile .

# Run repro. Map go cache and packages to speed up execution.
docker run -t -i -v $(go env GOCACHE):/root/.cache/go-build -v $GOPATH/pkg/mod:/go/pkg/mod repro
