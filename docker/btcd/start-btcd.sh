#!/usr/bin/env bash

# Check env variable and in case of empty value and default value specified
# returns default value, in case of non-empty value returns value.
set_env() {
    # docker initialized env variables with blank string and we can't just
    # use -z flag as usually.
    BLANK_STRING='""'

    VARIABLE="$1"
    NAME="$2"
    DEFAULT="$3"

    if [[ -z "$VARIABLE" || "$VARIABLE" == "$BLANK_STRING" ]]; then

        if [ -z "$DEFAULT" ]; then
            echo "You should specify '$NAME' env variable"
            exit 0
        else
            VARIABLE="$DEFAULT"
        fi
    fi

    # echo is used as return in case if string values
    echo "$VARIABLE"
}

RPCUSER=$(set_env "$RPCUSER" "RPCUSER")
RPCPASS=$(set_env "$RPCPASS" "RPCPASS")

DEBUG=$(set_env "$DEBUG" "DEBUG")

MINING_ADDRESS=$(set_env "$MINING_ADDRESS" "MINING_ADDRESS" "ScoDuqH7kYA9nvxuRg4Xk7E31AhsSc5zxp")

btcd \
    --debuglevel="$DEBUG" \
    --datadir="/data" \
    --logdir="/data" \
    --simnet \
    --rpccert="/rpc/rpc.cert" \
    --rpckey="/rpc/rpc.key" \
    --rpcuser="$RPCUSER" \
    --rpcpass="$RPCPASS" \
    --miningaddr="$MINING_ADDRESS" \
    --rpclisten="0.0.0.0" \
    "$@"

