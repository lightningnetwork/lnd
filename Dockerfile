FROM golang:alpine as builder

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

# Copy in the local repository to build from.
COPY . /go/src/github.com/lightningnetwork/lnd

# Install dependencies.
RUN apk add --no-cache --update alpine-sdk \
    git \
    make

# Build the binaries.
WORKDIR /go/src/github.com/lightningnetwork/lnd
RUN make install

# Start a new, final image.
FROM alpine as final

# Define a root volume for data persistence.
VOLUME /root/.lnd

# Expose lnd ports (p2p, rpc).
EXPOSE 9735 10009

# Specify the start command and entrypoint as the lnd daemon.
ENTRYPOINT ["lnd"]
CMD ["lnd"]

# Add a user and use it by default.
RUN adduser -D lnd
USER lnd

# Copy the binaries from the builder image.
COPY --from=builder /go/bin/lncli /bin/
COPY --from=builder /go/bin/lnd /bin/
