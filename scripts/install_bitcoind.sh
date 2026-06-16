#!/usr/bin/env bash

set -ev

BITCOIND_VERSION=$1

# The docker image tag and the install directory version don't always use the
# same format. Major-only tags like `29` or `30` install into
# `/opt/bitcoin-29.0` and `/opt/bitcoin-30.0`, while tags that already carry a
# dot (patch releases like `29.1` or release candidates like `30.0rc1`) install
# into a directory that matches the tag verbatim (`/opt/bitcoin-29.1`,
# `/opt/bitcoin-30.0rc1`). Testing an RC therefore just means passing the full
# version string as the first argument, e.g. `install_bitcoind.sh 30.0rc1`.
if [[ "$BITCOIND_VERSION" == *.* ]]; then
  BITCOIND_DIR_VERSION="$BITCOIND_VERSION"
else
  BITCOIND_DIR_VERSION="${BITCOIND_VERSION}.0"
fi

# TAG_SUFFIX and DIR_SUFFIX are kept as escape hatches for one-off images that
# diverge from the conventions above (e.g. a privately pushed build); they are
# empty by default and should stay that way for normal releases.
TAG_SUFFIX=
DIR_SUFFIX=

# Useful for testing against an image pushed to a different Docker repo.
REPO=lightninglabs/bitcoin-core

if [ -z "$BITCOIND_VERSION" ]; then
  echo "Must specify a version of bitcoind to install."
  echo "Usage: install_bitcoind.sh <version>"
  exit 1
fi

docker pull ${REPO}:${BITCOIND_VERSION}${TAG_SUFFIX}
CONTAINER_ID=$(docker create ${REPO}:${BITCOIND_VERSION}${TAG_SUFFIX})
sudo docker cp $CONTAINER_ID:/opt/bitcoin-${BITCOIND_DIR_VERSION}${DIR_SUFFIX}/bin/bitcoind /usr/local/bin/bitcoind
docker rm $CONTAINER_ID
