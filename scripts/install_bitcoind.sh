#!/usr/bin/env bash

set -ev

BITCOIND_VERSION=$1

# Useful for testing RCs: e.g. TAG_SUFFIX=.0rc1, DIR_SUFFIX=.0rc1
TAG_SUFFIX=
DIR_SUFFIX=.0

# Useful for testing against an image pushed to a different Docker repo.
REPO=lightninglabs/bitcoin-core

if [ -z "$BITCOIND_VERSION" ]; then
  echo "Must specify a version of bitcoind to install."
  echo "Usage: install_bitcoind.sh <version>"
  exit 1
fi

docker pull ${REPO}:${BITCOIND_VERSION}${TAG_SUFFIX}
CONTAINER_ID=$(docker create ${REPO}:${BITCOIND_VERSION}${TAG_SUFFIX})
sudo docker cp $CONTAINER_ID:/opt/bitcoin-${BITCOIND_VERSION}${DIR_SUFFIX}/bin/bitcoind /usr/local/bin/bitcoind
docker rm $CONTAINER_ID
