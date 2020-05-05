#!/bin/bash

# Simple bash script to build basic lnd tools for all the platforms
# we support with the golang cross-compiler.
#
# Copyright (c) 2016 Company 0, LLC.
# Use of this source code is governed by the ISC
# license.

set -e

LND_VERSION_REGEX="lnd version (.+) commit"
PKG="github.com/lightningnetwork/lnd"
PACKAGE=lnd

# green prints one line of green text (if the terminal supports it).
function green() {
  echo -e "\e[0;32m${1}\e[0m"
}

# red prints one line of red text (if the terminal supports it).
function red() {
  echo -e "\e[0;31m${1}\e[0m"
}

# check_tag_correct makes sure the given git tag is checked out and the git tree
# is not dirty.
#   arguments: <version-tag>
function check_tag_correct() {
  local tag=$1

  # If a tag is specified, ensure that that tag is present and checked out.
  if [[ $tag != $(git describe) ]]; then
    red "tag $tag not checked out"
    exit 1
  fi

  # Build lnd to extract version.
  go build ${PKG}/cmd/lnd

  # Extract version command output.
  lnd_version_output=$(./lnd --version)

  # Use a regex to isolate the version string.
  if [[ $lnd_version_output =~ $LND_VERSION_REGEX ]]; then
    # Prepend 'v' to match git tag naming scheme.
    lnd_version="v${BASH_REMATCH[1]}"
    green "version: $lnd_version"

    # If tag contains a release candidate suffix, append this suffix to the
    # lnd reported version before we compare.
    RC_REGEX="-rc[0-9]+$"
    if [[ $tag =~ $RC_REGEX ]]; then
      lnd_version+=${BASH_REMATCH[0]}
    fi

    # Match git tag with lnd version.
    if [[ $tag != "${lnd_version}" ]]; then
      red "lnd version $lnd_version does not match tag $tag"
      exit 1
    fi
  else
    red "malformed lnd version output"
    exit 1
  fi
}

# build_release builds the actual release binaries.
#   arguments: <version-tag> <build-system(s)> <build-tags> <ldflags>
function build_release() {
  local tag=$1
  local sys=$2
  local buildtags=$3
  local ldflags=$4

  green " - Packaging vendor"
  go mod vendor
  tar -czf vendor.tar.gz vendor

  maindir=$PACKAGE-$tag
  mkdir -p $maindir

  cp vendor.tar.gz $maindir/
  rm vendor.tar.gz
  rm -r vendor

  package_source="${maindir}/${PACKAGE}-source-${tag}.tar"
  git archive -o "${package_source}" HEAD
  gzip -f "${package_source}" >"${package_source}.gz"

  cd "${maindir}"

  for i in $sys; do
    os=$(echo $i | cut -f1 -d-)
    arch=$(echo $i | cut -f2 -d-)
    arm=

    if [[ $arch == "armv6" ]]; then
      arch=arm
      arm=6
    elif [[ $arch == "armv7" ]]; then
      arch=arm
      arm=7
    fi

    dir="${PACKAGE}-${i}-${tag}"
    mkdir "${dir}"
    pushd "${dir}"

    green " - Building: ${os} ${arch} ${arm} with build tags '${buildtags}'"
    env CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=$arm go build -v -trimpath -ldflags="${ldflags}" -tags="${buildtags}" ${PKG}/cmd/lnd
    env CGO_ENABLED=0 GOOS=$os GOARCH=$arch GOARM=$arm go build -v -trimpath -ldflags="${ldflags}" -tags="${buildtags}" ${PKG}/cmd/lncli
    popd

    if [[ $os == "windows" ]]; then
      zip -r "${dir}.zip" "${dir}"
    else
      tar -cvzf "${dir}.tar.gz" "${dir}"
    fi

    rm -r "${dir}"
  done

  shasum -a 256 * >manifest-$tag.txt
}

# usage prints the usage of the whole script.
function usage() {
  red "Usage: "
  red "release.sh check-tag <version-tag>"
  red "release.sh build-release <version-tag> <build-system(s)> <build-tags> <ldflags>"
}

# Whatever sub command is passed in, we need at least 2 arguments.
if [ "$#" -lt 2 ]; then
  usage
  exit 1
fi

# Extract the sub command and remove it from the list of parameters by shifting
# them to the left.
SUBCOMMAND=$1
shift

# Call the function corresponding to the specified sub command or print the
# usage if the sub command was not found.
case $SUBCOMMAND in
check-tag)
  green "Checking if version tag exists"
  check_tag_correct "$@"
  ;;
build-release)
  green "Building release"
  build_release "$@"
  ;;
*)
  usage
  exit 1
  ;;
esac
