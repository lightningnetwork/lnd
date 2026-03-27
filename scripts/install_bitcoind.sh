#!/usr/bin/env bash

set -ev

BITCOIND_VERSION=$1

# Useful for testing RCs: e.g. TAG_SUFFIX=.0rc1, DIR_SUFFIX=.0rc1
TAG_SUFFIX=
DIR_SUFFIX=.0

# Useful for testing against a different Docker repo or tarball host.
REPO=${BITCOIND_REPO:-lightninglabs/bitcoin-core}
BITCOIND_TARBALL_BASE_URL=${BITCOIND_TARBALL_BASE_URL:-https://bitcoincore.org/bin}
BITCOIND_INSTALL_PATH=${BITCOIND_INSTALL_PATH:-/usr/local/bin/bitcoind}

if [ -z "$BITCOIND_VERSION" ]; then
  echo "Must specify a version of bitcoind to install."
  echo "Usage: install_bitcoind.sh <version>"
  exit 1
fi

IMAGE_TAG="${REPO}:${BITCOIND_VERSION}${TAG_SUFFIX}"
RELEASE_VERSION="${BITCOIND_VERSION}${DIR_SUFFIX}"
TARBALL="bitcoin-${RELEASE_VERSION}-x86_64-linux-gnu.tar.gz"
TARBALL_URL="${BITCOIND_TARBALL_BASE_URL}/bitcoin-core-${RELEASE_VERSION}/${TARBALL}"

install_from_docker() {
  local container_id
  local temp_dir

  if ! docker pull "${IMAGE_TAG}"; then
    return 1
  fi

  if ! container_id=$(docker create "${IMAGE_TAG}"); then
    return 1
  fi

  temp_dir=$(mktemp -d)

  if ! docker cp \
    "${container_id}:/opt/bitcoin-${RELEASE_VERSION}/bin/bitcoind" \
    "${temp_dir}/bitcoind"; then

    docker rm "${container_id}" || true
    rm -rf "${temp_dir}"
    return 1
  fi

  docker rm "${container_id}"
  install_bitcoind_binary "${temp_dir}/bitcoind"
  rm -rf "${temp_dir}"
}

install_bitcoind_binary() {
  local source_path=$1

  if install -m 0755 "${source_path}" "${BITCOIND_INSTALL_PATH}" 2>/dev/null; then
    return 0
  fi

  sudo install -m 0755 "${source_path}" "${BITCOIND_INSTALL_PATH}"
}

install_from_tarball() {
  local temp_dir

  temp_dir=$(mktemp -d)
  curl -fsSL "${TARBALL_URL}" -o "${temp_dir}/${TARBALL}"
  tar -xzf "${temp_dir}/${TARBALL}" -C "${temp_dir}"
  install_bitcoind_binary "${temp_dir}/bitcoin-${RELEASE_VERSION}/bin/bitcoind"
  rm -rf "${temp_dir}"
}

if ! install_from_docker; then
  echo "Docker-based bitcoind install failed, falling back to tarball ${TARBALL_URL}"
  install_from_tarball
fi
