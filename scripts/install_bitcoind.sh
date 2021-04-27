#!/usr/bin/env bash

set -ev

export BITCOIND_VERSION=0.20.0

if sudo cp ~/bitcoin/bitcoin-$BITCOIND_VERSION/bin/bitcoind /usr/local/bin/bitcoind
then
        echo "found cached bitcoind"
else
        mkdir -p ~/bitcoin && \
        pushd ~/bitcoin && \
        wget https://bitcoin.org/bin/bitcoin-core-$BITCOIND_VERSION/bitcoin-$BITCOIND_VERSION-x86_64-linux-gnu.tar.gz && \
        tar xvfz bitcoin-$BITCOIND_VERSION-x86_64-linux-gnu.tar.gz && \
        sudo cp ./bitcoin-$BITCOIND_VERSION/bin/bitcoind /usr/local/bin/bitcoind && \
        popd
fi

