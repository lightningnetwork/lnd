#!/usr/bin/env bash

set -ev

BITCOIND_VERSION=${BITCOIN_VERSION:-23.0}

docker pull lightninglabs/bitcoin-core:$BITCOIND_VERSION
CONTAINER_ID=$(docker create lightninglabs/bitcoin-core:$BITCOIND_VERSION)
sudo docker cp $CONTAINER_ID:/opt/bitcoin-$BITCOIND_VERSION/bin/bitcoind /usr/local/bin/bitcoind
docker rm $CONTAINER_ID
