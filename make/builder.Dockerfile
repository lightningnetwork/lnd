# If you change this value, please change it in the following files as well:
# /Dockerfile
# /dev.Dockerfile
# /.github/workflows/main.yml
# /.github/workflows/release.yaml
FROM golang:1.22.6-bookworm@sha256:d31e093e3aeaee68ccee6c4c96e554ef0f192ea37ae684d91b206bec17377f19

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.engineering>

# Golang build related environment variables that are static and used for all
# architectures/OSes.
ENV GODEBUG netdns=cgo
ENV GO111MODULE=auto
ENV CGO_ENABLED=0

# Set up cache directories. Those will be mounted from the host system to speed
# up builds. If go isn't installed on the host system, those will fall back to
# temp directories during the build (see make/release_flags.mk).
ENV GOCACHE=/tmp/build/.cache
ENV GOMODCACHE=/tmp/build/.modcache

RUN apt-get update && apt-get install -y \
    git \
    make \
    tar \
    zip \
    bash \
  && mkdir -p /tmp/build/lnd \
  && mkdir -p /tmp/build/.cache \
  && mkdir -p /tmp/build/.modcache \
  && chmod -R 777 /tmp/build/

WORKDIR /tmp/build/lnd
