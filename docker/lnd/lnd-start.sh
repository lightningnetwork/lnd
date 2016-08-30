#!/bin/bash

/go/bin/lnd --datadir=/data --logdir=/data --simnet  \
            --btcdhost=btcd --rpccert=/rpc/rpc.cert  \
            --rpcuser=${RPCUSER} --rpcpass=${RPCPASS} --debuglevel=debug
