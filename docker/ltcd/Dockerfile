FROM golang:1.7

MAINTAINER Olaoluwa Osuntokun <lightning.engineering>

# Expose mainnet ports (server, rpc)
EXPOSE 8333 8334

# Expose testnet ports (server, rpc)
EXPOSE 18333 18334

# Expose simnet ports (server, rpc)
EXPOSE 18555 18556

# Expose segnet ports (server, rpc)
EXPOSE 28901 28902

# Grab and install the latest version of roasbeef's fork of ltcd and all
# related dependencies.
WORKDIR $GOPATH/src/github.com/ltcsuite/ltcd
RUN git clone https://github.com/ltcsuite/ltcd ./
RUN go get -u github.com/Masterminds/glide
RUN glide install
RUN go install . ./cmd/ltcctl ./cmd/gencerts

RUN mkdir "/rpc" "/root/.ltcd" "/root/.ltcctl"
RUN touch "/root/.ltcd/ltcd.conf"

# "ltcctl" and "lnd" to "ltcd" over docker links.
RUN "/go/bin/gencerts" --host="*" --directory="/rpc" --force

# Create a volume to house pregenerated RPC credentials. This will be
# shared with any lnd, btcctl containers so they can securely query ltcd's RPC
# server.
# You should NOT do this before certificate generation!
# Otherwise manually generated certificate will be overriden with shared
# mounted volume! For more info read dockerfile "VOLUME" documentation.
VOLUME ["/rpc"]

COPY "start-ltcctl.sh" .
COPY "start-ltcd.sh" .

RUN chmod +x start-ltcctl.sh
RUN chmod +x start-ltcd.sh

