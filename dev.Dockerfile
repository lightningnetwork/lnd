# If you change this please also update GO_VERSION in Makefile (then run
# `make lint` to see where else it needs to be updated as well).
FROM golang:1.27.3-alpine AS builder

LABEL maintainer="Olaoluwa Osuntokun <laolu@lightning.engineering>"

# Install dependencies.
RUN apk add --no-cache --update alpine-sdk \
    bash \
    git \
    make 

# Copy in the local repository to build from.
COPY . /go/src/github.com/lightningnetwork/lnd

#  Install/build lnd.
# Note: When using `docker build`, setting the environmental variable
# `DOCKER_BUILDKIT=1` is required to enable
# [BuildKit](https://docs.docker.com/build/buildkit)
# so that the cache mounts can be used.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd /go/src/github.com/lightningnetwork/lnd \
    &&  make \
    &&  make release-install manpages

# Start a new, final image to reduce size.
FROM alpine AS final

# Expose lnd ports (server, rpc).
EXPOSE 9735 10009

# Add bash and jq for quality of life.
RUN apk --no-cache add bash jq

# Copy the binaries and entrypoint from the builder image.
COPY --from=builder /go/bin/lncli /bin/
COPY --from=builder /go/bin/lnd /bin/

# Copy the entrypoint script.
COPY "docker/lnd/start-lnd.sh" .
RUN chmod +x start-lnd.sh
