#!/usr/bin/env bash

# exit from script if error was raised.
set -e

WORKDIR=/root

function waitnoerror() {
        for i in {1..30}; do $@ && return; sleep 1; done
        echo "timeout"
        exit 1
}

function waitpredicate() {
        cmd=$1
        echo $cmd
        shift
        for i in {1..30}; do [ `eval $cmd` $@ ] && return; sleep 1; done
        echo "timeout"
        exit 1
}

echo "Starting btcd"
btcd --simnet --rpcuser=devuser --rpcpass=devpass --miningaddr=SVv2gHztnPJa2YU7J4SjkiBMXcTvnDWxgM &
BTCD_PID=$!

BTCCTL="btcctl --simnet --rpcuser=devuser --rpcpass=devpass"

# Wait for btcd startup
waitnoerror $BTCCTL getinfo

echo "Mining initial blocks"
$BTCCTL generate 400

echo "Starting lnd"

run_alice() {
  # Keep restarting lnd after random kill happened.
  while true
  do
    $WORKDIR/lnd --bitcoin.active --bitcoin.simnet --btcd.rpcuser=devuser \
      --btcd.rpcpass=devpass --noseedbackup --lnddir=$WORKDIR/lnd-server --accept-keysend --trickledelay=1000 --debuglevel=debug | awk '{ print "[alice] " $0; }' 
  done
}

run_bob() {
  while true
  do
    $WORKDIR/lnd --bitcoin.active --bitcoin.simnet --btcd.rpcuser=devuser \
      --btcd.rpcpass=devpass --noseedbackup --rpclisten=localhost:10002 \
      --listen=localhost:10012 --restlisten=localhost:8002 \
      --lnddir=$WORKDIR/lnd-client --accept-keysend --trickledelay=1000 --debuglevel=debug | awk '{ print "[bob] " $0; }'
  done
}

run_alice &
run_bob &

LNCLI_ALICE="$WORKDIR/lncli --network simnet --lnddir $WORKDIR/lnd-server"
LNCLI_BOB="$WORKDIR/lncli --network simnet --lnddir $WORKDIR/lnd-client --rpcserver=localhost:10002"

waitnoerror $LNCLI_ALICE getinfo
waitnoerror $LNCLI_BOB getinfo

# Get pubkeys
ALICE_PUBKEY=`$LNCLI_ALICE getinfo | jq .identity_pubkey -r`
BOB_PUBKEY=`$LNCLI_BOB getinfo | jq .identity_pubkey -r`

# Create mining address
MINING_ADDR=`$LNCLI_ALICE newaddress np2wkh | jq .address -r`
echo $MINING_ADDR

# Kill btcd
kill -INT $BTCD_PID
wait $BTCD_PID

# Restart btcd with mining address
echo "Restart btcd with mining address $MINING_ADDR"
btcd --simnet --rpcuser=devuser --rpcpass=devpass --miningaddr=$MINING_ADDR --txindex \
    | awk '{ print "[btcd] " $0; }' &
BTCD_PID=$!

waitnoerror $BTCCTL getinfo

# Mine some blocks
$BTCCTL generate 100

waitpredicate "$LNCLI_ALICE walletbalance | jq .confirmed_balance -r" -ne 0

# Connect and open channel

$LNCLI_ALICE connect $BOB_PUBKEY@localhost:10012

$LNCLI_ALICE openchannel $BOB_PUBKEY 10000000 5000000

$BTCCTL generate 6

waitpredicate "$LNCLI_ALICE listchannels | jq '.channels | length'" -ne 0

echo "STARTING TEST"
echo "Alice PK: $ALICE_PUBKEY"
echo "Bob PK: $BOB_PUBKEY"
sleep 3

# Start test

crossfire() {
  while true
  do
        $LNCLI_ALICE sendpayment -d $BOB_PUBKEY -a 10000 --keysend || true
        $LNCLI_BOB sendpayment -d $ALICE_PUBKEY -a 10000 --keysend || true
  done
}

# Set up multiple cross-fire threads
crossfire &
crossfire &
crossfire &
crossfire &
crossfire &

# Kill an lnd instance every 10 seconds until the channel dies.
while [[ $($LNCLI_ALICE listchannels --active_only | jq '.channels | length') -ne 0 ]]
do
  LND_PID=$(ps | grep lnd | head -1 | awk '{ printf $1 }')
  echo "Killing $LND_PID"
  kill -9 $LND_PID

  sleep 10
done
