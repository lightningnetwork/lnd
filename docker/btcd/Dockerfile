FROM golang:1.7

MAINTAINER Olaoluwa Osuntokun <laolu@lightning.network>

# Grab and install the latest version of roasbeef's fork of btcd and all
# related dependencies.
RUN go get -u -v github.com/roasbeef/btcd/...

# Expose the mainnet, testnet, simnet, and segnet listening ports.
EXPOSE 8333 18333 18335 28901

# Expose the mainnet, testnet, simnet, and segnet rpc ports.
EXPOSE 8333 18333 18336 28902

# Create a volume to house the RPC credentials. This will be shared with any
# lnd containers so they can securely query btcd's RPC server.
VOLUME ["/rpc"]

VOLUME ["/data"]

RUN mkdir /root/.btcd && mkdir /root/.btcctl

COPY btcd-start.sh /

# Finally, execute the shell script that will start btcd. We use a shell script
# rather than executing the command directly with ENTRYPOINT in order to ensure
# environment variables get properly substitued.
ENTRYPOINT ["/btcd-start.sh"]
