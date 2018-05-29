#!/bin/bash

# Simple bash script to build basic lnd tools for all the platforms
# we support with the golang cross-compiler.
#
# Copyright (c) 2016 Company 0, LLC.
# Use of this source code is governed by the ISC
# license.

# If no tag specified, use date + version otherwise use tag.
if [[ $1x = x ]]; then
    DATE=`date +%Y%m%d`
    VERSION="01"
    TAG=$DATE-$VERSION
else
    TAG=$1
fi

PACKAGE=lnd
MAINDIR=$PACKAGE-$TAG
mkdir -p $MAINDIR
cd $MAINDIR

SYS="windows-386 windows-amd64 openbsd-386 openbsd-amd64 linux-386 linux-amd64 linux-arm linux-arm64 darwin-386 darwin-amd64 dragonfly-amd64 freebsd-386 freebsd-amd64 freebsd-arm netbsd-386 netbsd-amd64 linux-mips64 linux-mips64le linux-ppc64 linux-arm64"

# Use the first element of $GOPATH in the case where GOPATH is a list
# (something that is totally allowed).
GPATH=$(echo $GOPATH | cut -f1 -d:)

for i in $SYS; do
    OS=$(echo $i | cut -f1 -d-)
    ARCH=$(echo $i | cut -f2 -d-)
    mkdir $PACKAGE-$i-$TAG
    cd $PACKAGE-$i-$TAG
    echo "Building:" $OS $ARCH
    env GOOS=$OS GOARCH=$ARCH go build -v github.com/lightningnetwork/lnd
    env GOOS=$OS GOARCH=$ARCH go build -v github.com/lightningnetwork/lnd/cmd/lncli
    env GOOS=$OS GOARCH=$ARCH go build -v github.com/lightningnetwork/lnd/cmd/lnneo4j
    cd ..
    if [[ $OS = "windows" ]]; then
	zip -r $PACKAGE-$i-$TAG.zip $PACKAGE-$i-$TAG
    else
	tar -cvzf $PACKAGE-$i-$TAG.tar.gz $PACKAGE-$i-$TAG
    fi
    rm -r $PACKAGE-$i-$TAG
done

shasum -a 256 * > manifest-$TAG.txt
