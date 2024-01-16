#!/usr/bin/env bash

set -ev

BITCOIND_VERSION=25

docker pull lightninglabs/bitcoin-core:$BITCOIND_VERSION
CONTAINER_ID=$(docker create lightninglabs/bitcoin-core:$BITCOIND_VERSION)
sudo docker cp $CONTAINER_ID:/opt/bitcoin-$BITCOIND_VERSION.0/bin/bitcoind /usr/local/bin/bitcoind
docker rm $CONTAINER_ID
