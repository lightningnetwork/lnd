#!/bin/bash


/go/bin/btcd --datadir=/data --logdir=/data --simnet  \
             --rpccert=/rpc/rpc.cert --rpckey=/rpc/rpc.key  \
             --rpcuser=${RPCUSER} --rpcpass=${RPCPASS} --rpclisten=0.0.0.0 \
             --debuglevel=debug
