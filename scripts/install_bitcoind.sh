#!/usr/bin/env bash

set -ev

BITCOIND_VERSION=$1

if [ -z "$BITCOIND_VERSION" ]; then
  echo "Must specify a version of bitcoind to install."
  echo "Usage: install_bitcoind.sh <version>"
  exit 1
fi

docker pull guggero/bitcoin-core:${BITCOIND_VERSION}rc1
CONTAINER_ID=$(docker create guggero/bitcoin-core:${BITCOIND_VERSION}rc1)
sudo docker cp $CONTAINER_ID:/opt/bitcoin-${BITCOIND_VERSION}.0rc1/bin/bitcoind /usr/local/bin/bitcoind
docker rm $CONTAINER_ID
