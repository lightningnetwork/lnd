FROM lightninglabs/bitcoin-core:${BITCOIN_VERSION:-25}

# Copy the start script into the container
COPY start-bitcoind.sh .

# Set execute permissions for the script
RUN chmod +x ./start-bitcoind.sh
