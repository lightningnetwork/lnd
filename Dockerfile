FROM golang:alpine as builder

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

# Install dependencies and build the binaries
RUN apk add --no-cache \
    git \
    make \
&&  git clone https://github.com/lightningnetwork/lnd /go/src/github.com/lightningnetwork/lnd \
&&  cd /go/src/github.com/lightningnetwork/lnd \
&&  make

# Start a new, final image
FROM alpine as final

# Define a root volume for data persistence
VOLUME /root/.lnd

# Add bash and ca-certs, for quality of life and SSL-related reasons
RUN apk --no-cache add \
    bash \
    ca-certificates

# Copy the binaries and entrypoint from the builder image
COPY --from=builder /go/src/github.com/lightningnetwork/lnd/lncli /bin/
COPY --from=builder /go/src/github.com/lightningnetwork/lnd/lnd /bin/
COPY "docker-entrypoint.sh" .

# Use the script to automatically start lnd
ENTRYPOINT ["/docker-entrypoint.sh"]
CMD ["lncli"]
