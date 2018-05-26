FROM golang:1.10-alpine as builder

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.network>

# Install build dependencies such as git and glide.
RUN apk add --no-cache \
    git \
&&  go get -u github.com/Masterminds/glide

WORKDIR $GOPATH/src/github.com/roasbeef/btcd

# Grab and install the latest version of roasbeef's fork of btcd and all
# related dependencies.
RUN git clone https://github.com/roasbeef/btcd . \
&&  glide install \
&&  go install . ./cmd/...

# Start a new image
FROM alpine as final

# Expose mainnet ports (server, rpc)
EXPOSE 8333 8334

# Expose testnet ports (server, rpc)
EXPOSE 18333 18334

# Expose simnet ports (server, rpc)
EXPOSE 18555 18556

# Expose segnet ports (server, rpc)
EXPOSE 28901 28902

# Copy the compiled binaries from the builder image.
COPY --from=builder /go/bin/addblock /bin/
COPY --from=builder /go/bin/btcctl /bin/
COPY --from=builder /go/bin/btcd /bin/
COPY --from=builder /go/bin/findcheckpoint /bin/
COPY --from=builder /go/bin/gencerts /bin/

COPY "start-btcctl.sh" .
COPY "start-btcd.sh" .

RUN apk add --no-cache \
    bash \
    ca-certificates \
&&  mkdir "/rpc" "/root/.btcd" "/root/.btcctl" \
&&  touch "/root/.btcd/btcd.conf" \
&&  chmod +x start-btcctl.sh \
&&  chmod +x start-btcd.sh \
# Manually generate certificate and add all domains, it is needed to connect
# "btcctl" and "lnd" to "btcd" over docker links.
&& "/bin/gencerts" --host="*" --directory="/rpc" --force

# Create a volume to house pregenerated RPC credentials. This will be
# shared with any lnd, btcctl containers so they can securely query btcd's RPC
# server.
# You should NOT do this before certificate generation!
# Otherwise manually generated certificate will be overridden with shared
# mounted volume! For more info read dockerfile "VOLUME" documentation.
VOLUME ["/rpc"]
