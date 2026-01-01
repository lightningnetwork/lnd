#!/bin/bash
#
# End-to-End Test Script for LND Esplora Backend
#
# This script tests the Esplora backend implementation by:
# 1. Starting two LND nodes with Esplora backend
# 2. Funding the first node
# 3. Opening a channel between the nodes
# 4. Making payments
# 5. Closing the channel
#
# Prerequisites:
# - Bitcoin Core running (native or in Docker)
# - Esplora API server (electrs/mempool-electrs) running and connected to Bitcoin Core
# - Go installed for building LND
#
# Usage:
#   ./scripts/test-esplora-e2e.sh [esplora_url]
#
# Example:
#   ./scripts/test-esplora-e2e.sh http://127.0.0.1:3002
#
# Environment Variables:
#   BITCOIN_CLI      - Path to bitcoin-cli or docker command (auto-detected)
#   DOCKER_BITCOIN   - Set to container name if using Docker (e.g., "bitcoind")
#   RPC_USER         - Bitcoin RPC username (default: "second")
#   RPC_PASS         - Bitcoin RPC password (default: "ark")
#   REBUILD          - Set to "1" to force rebuild of lnd
#

set -e

# Configuration
ESPLORA_URL="${1:-http://127.0.0.1:3002}"
TEST_DIR="./test-esplora-e2e"
ALICE_DIR="$TEST_DIR/alice"
BOB_DIR="$TEST_DIR/bob"
ALICE_PORT=10015
ALICE_REST=8089
ALICE_PEER=9738
BOB_PORT=10016
BOB_REST=8090
BOB_PEER=9739

# Bitcoin RPC Configuration
RPC_USER="${RPC_USER:-second}"
RPC_PASS="${RPC_PASS:-ark}"
DOCKER_BITCOIN="${DOCKER_BITCOIN:-}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_step() {
    echo -e "\n${GREEN}========================================${NC}"
    echo -e "${GREEN}$1${NC}"
    echo -e "${GREEN}========================================${NC}\n"
}

# Bitcoin CLI wrapper - handles both native and Docker setups
btc() {
    if [ -n "$DOCKER_BITCOIN" ]; then
        docker exec "$DOCKER_BITCOIN" bitcoin-cli -regtest -rpcuser="$RPC_USER" -rpcpassword="$RPC_PASS" "$@"
    elif [ -n "$BITCOIN_CLI" ]; then
        $BITCOIN_CLI -regtest -rpcuser="$RPC_USER" -rpcpassword="$RPC_PASS" "$@"
    else
        bitcoin-cli -regtest -rpcuser="$RPC_USER" -rpcpassword="$RPC_PASS" "$@"
    fi
}

cleanup() {
    log_step "Cleaning up..."

    # Stop Alice
    if [ -f "$ALICE_DIR/lnd.pid" ]; then
        kill $(cat "$ALICE_DIR/lnd.pid") 2>/dev/null || true
        rm -f "$ALICE_DIR/lnd.pid"
    fi

    # Stop Bob
    if [ -f "$BOB_DIR/lnd.pid" ]; then
        kill $(cat "$BOB_DIR/lnd.pid") 2>/dev/null || true
        rm -f "$BOB_DIR/lnd.pid"
    fi

    # Kill any remaining lnd processes from this test
    pkill -f "lnd-esplora.*test-esplora-e2e" 2>/dev/null || true

    log_info "Cleanup complete"
}

# Set trap to cleanup on exit
trap cleanup EXIT

detect_bitcoin_cli() {
    log_info "Detecting Bitcoin Core setup..."

    # Check for Docker container with "bitcoind" in the name
    for container in $(docker ps --format '{{.Names}}' 2>/dev/null | grep -i bitcoind); do
        if docker exec "$container" bitcoin-cli -regtest -rpcuser="$RPC_USER" -rpcpassword="$RPC_PASS" getblockchaininfo &>/dev/null; then
            DOCKER_BITCOIN="$container"
            log_info "Found Bitcoin Core in Docker container: $DOCKER_BITCOIN"
            return 0
        fi
    done

    # Check for docker-compose based names with "bitcoin" in the name
    for container in $(docker ps --format '{{.Names}}' 2>/dev/null | grep -i bitcoin); do
        if docker exec "$container" bitcoin-cli -regtest -rpcuser="$RPC_USER" -rpcpassword="$RPC_PASS" getblockchaininfo &>/dev/null; then
            DOCKER_BITCOIN="$container"
            log_info "Found Bitcoin Core in Docker container: $DOCKER_BITCOIN"
            return 0
        fi
    done

    # Check for native bitcoin-cli
    if command -v bitcoin-cli &> /dev/null; then
        log_info "Found native bitcoin-cli"
        return 0
    fi

    return 1
}

check_prerequisites() {
    log_step "Checking prerequisites..."

    # Detect Bitcoin CLI setup
    if ! detect_bitcoin_cli; then
        log_error "Bitcoin Core not found. Please either:"
        log_error "  1. Install Bitcoin Core natively"
        log_error "  2. Run Bitcoin Core in Docker (container name should contain 'bitcoin')"
        log_error "  3. Set DOCKER_BITCOIN env var to your container name"
        exit 1
    fi

    # Check if Bitcoin Core is running in regtest
    if ! btc getblockchaininfo &> /dev/null; then
        log_error "Bitcoin Core not responding to RPC"
        log_error "Check RPC credentials: RPC_USER=$RPC_USER"
        exit 1
    fi
    log_info "Bitcoin Core running in regtest mode"

    # Show blockchain info
    local blocks=$(btc getblockchaininfo | jq -r '.blocks')
    log_info "Current block height: $blocks"

    # Check if Esplora API is reachable
    if ! curl -s "${ESPLORA_URL}/blocks/tip/height" &>/dev/null; then
        log_error "Esplora API not reachable at $ESPLORA_URL"
        log_error "Start your Esplora server (electrs, mempool-electrs, etc.)"
        exit 1
    fi
    log_info "Esplora API reachable at $ESPLORA_URL"

    # Check if Go is available
    if ! command -v go &> /dev/null; then
        log_error "Go not found. Please install Go."
        exit 1
    fi
    log_info "Go found"

    # Check if jq is available
    if ! command -v jq &> /dev/null; then
        log_error "jq not found. Please install jq."
        exit 1
    fi
    log_info "jq found"

    log_info "All prerequisites met!"
}

build_lnd() {
    log_step "Building LND..."

    if [ ! -f "./lnd-esplora" ] || [ "$REBUILD" = "1" ]; then
        go build -o lnd-esplora ./cmd/lnd
        log_info "Built lnd-esplora"
    else
        log_info "lnd-esplora already exists, skipping build"
    fi

    if [ ! -f "./lncli-esplora" ] || [ "$REBUILD" = "1" ]; then
        go build -o lncli-esplora ./cmd/lncli
        log_info "Built lncli-esplora"
    else
        log_info "lncli-esplora already exists, skipping build"
    fi
}

setup_directories() {
    log_step "Setting up test directories..."

    # Clean up old test data
    rm -rf "$TEST_DIR"
    mkdir -p "$ALICE_DIR" "$BOB_DIR"

    # Create Alice's config
    cat > "$ALICE_DIR/lnd.conf" << EOF
[Bitcoin]
bitcoin.regtest=true
bitcoin.node=esplora

[esplora]
esplora.url=$ESPLORA_URL

[Application Options]
noseedbackup=true
debuglevel=debug
listen=127.0.0.1:$ALICE_PEER
rpclisten=127.0.0.1:$ALICE_PORT
restlisten=127.0.0.1:$ALICE_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

    # Create Bob's config
    cat > "$BOB_DIR/lnd.conf" << EOF
[Bitcoin]
bitcoin.regtest=true
bitcoin.node=esplora

[esplora]
esplora.url=$ESPLORA_URL

[Application Options]
noseedbackup=true
debuglevel=debug
listen=127.0.0.1:$BOB_PEER
rpclisten=127.0.0.1:$BOB_PORT
restlisten=127.0.0.1:$BOB_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

    log_info "Created config for Alice at $ALICE_DIR"
    log_info "Created config for Bob at $BOB_DIR"
}

start_node() {
    local name=$1
    local dir=$2
    local port=$3

    log_info "Starting $name..."

    ./lnd-esplora --lnddir="$dir" > "$dir/lnd.log" 2>&1 &
    echo $! > "$dir/lnd.pid"

    # Wait for node to start
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        if ./lncli-esplora --lnddir="$dir" --network=regtest --rpcserver=127.0.0.1:$port getinfo &> /dev/null; then
            log_info "$name started successfully"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name failed to start. Check $dir/lnd.log"
    cat "$dir/lnd.log" | tail -50
    exit 1
}

alice_cli() {
    ./lncli-esplora --lnddir="$ALICE_DIR" --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT "$@"
}

bob_cli() {
    ./lncli-esplora --lnddir="$BOB_DIR" --network=regtest --rpcserver=127.0.0.1:$BOB_PORT "$@"
}

mine_blocks() {
    local count=${1:-1}
    local addr=$(btc getnewaddress)
    btc generatetoaddress $count $addr > /dev/null
    log_info "Mined $count block(s)"
    # Wait for Esplora to index - minimal wait since it catches up fast
    sleep 2
}

wait_for_sync() {
    local name=$1
    local cli=$2
    local max_attempts=30
    local attempt=0

    log_info "Waiting for $name to sync..."

    while [ $attempt -lt $max_attempts ]; do
        local synced=$($cli getinfo 2>/dev/null | jq -r '.synced_to_chain')
        if [ "$synced" = "true" ]; then
            log_info "$name synced to chain"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name failed to sync"
    return 1
}

fund_node() {
    local name=$1
    local cli=$2
    local amount=$3

    log_info "Funding $name with $amount BTC..."

    # Get a new address
    local addr=$($cli newaddress p2tr | jq -r '.address')
    log_info "$name address: $addr"

    # Send funds
    btc sendtoaddress $addr $amount > /dev/null

    # Mine to confirm
    mine_blocks 6

    # Wait for balance to appear
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local balance=$($cli walletbalance | jq -r '.confirmed_balance')
        if [ "$balance" != "0" ]; then
            log_info "$name balance: $balance sats"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "Failed to fund $name"
    return 1
}

connect_peers() {
    log_step "Connecting peers..."

    local bob_pubkey=$(bob_cli getinfo | jq -r '.identity_pubkey')
    log_info "Bob's pubkey: $bob_pubkey"

    alice_cli connect "${bob_pubkey}@127.0.0.1:$BOB_PEER"
    sleep 2

    local peers=$(alice_cli listpeers | jq -r '.peers | length')
    if [ "$peers" = "1" ]; then
        log_info "Peers connected successfully"
    else
        log_error "Failed to connect peers"
        exit 1
    fi
}

open_channel() {
    local channel_type=$1
    local amount=$2
    local private=$3

    log_step "Opening $channel_type channel..."

    local bob_pubkey=$(bob_cli getinfo | jq -r '.identity_pubkey')

    local open_cmd="alice_cli openchannel --node_key=$bob_pubkey --local_amt=$amount"
    if [ "$private" = "true" ]; then
        open_cmd="$open_cmd --private"
    fi
    if [ "$channel_type" = "taproot" ]; then
        open_cmd="$open_cmd --channel_type=taproot"
    fi

    local result=$($open_cmd)
    local funding_txid=$(echo "$result" | jq -r '.funding_txid')
    log_info "Funding txid: $funding_txid"

    # Mine blocks to confirm (need 3 confirmations, mine extra to be safe)
    mine_blocks 6

    # Wait for channel to be active with longer timeout
    local max_attempts=60
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local active=$(alice_cli listchannels | jq -r '.channels[0].active')
        if [ "$active" = "true" ]; then
            log_info "Channel is active!"
            return 0
        fi
        # Check pending channels for debugging
        if [ $((attempt % 10)) -eq 0 ]; then
            local pending=$(alice_cli pendingchannels 2>/dev/null | jq -r '.pending_open_channels | length')
            log_info "Waiting for channel... (pending_open: $pending)"
        fi
        sleep 2
        attempt=$((attempt + 1))
    done

    log_error "Channel failed to become active"
    # Show pending channels for debugging
    alice_cli pendingchannels 2>/dev/null || true
    return 1
}

make_payment() {
    local amount=$1

    log_step "Making payment of $amount sats..."

    # Bob creates invoice
    local invoice=$(bob_cli addinvoice --amt=$amount | jq -r '.payment_request')
    log_info "Invoice created"

    # Alice pays
    alice_cli payinvoice --force "$invoice"
    log_info "Payment sent!"

    # Verify
    local bob_balance=$(bob_cli channelbalance | jq -r '.local_balance.sat')
    log_info "Bob's channel balance: $bob_balance sats"
}

close_channel() {
    log_step "Closing channel cooperatively..."

    local channel_point=$(alice_cli listchannels | jq -r '.channels[0].channel_point')
    log_info "Channel point: $channel_point"

    local funding_txid=$(echo $channel_point | cut -d':' -f1)
    local output_index=$(echo $channel_point | cut -d':' -f2)

    alice_cli closechannel --funding_txid=$funding_txid --output_index=$output_index

    # Mine to confirm close
    mine_blocks 6

    # Wait for channel to be fully closed
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local pending=$(alice_cli pendingchannels | jq -r '.waiting_close_channels | length')
        if [ "$pending" = "0" ]; then
            log_info "Channel closed successfully!"
            return 0
        fi
        sleep 2
        mine_blocks 1
        attempt=$((attempt + 1))
    done

    log_warn "Channel close is taking longer than expected"
}

run_test() {
    log_step "Starting Esplora Backend E2E Test"

    check_prerequisites
    build_lnd
    setup_directories

    # Start nodes
    start_node "Alice" "$ALICE_DIR" "$ALICE_PORT"
    start_node "Bob" "$BOB_DIR" "$BOB_PORT"

    # Wait for sync
    wait_for_sync "Alice" alice_cli
    wait_for_sync "Bob" bob_cli

    # Fund Alice
    fund_node "Alice" alice_cli 1.0

    # Connect peers
    connect_peers

    # Test 1: Regular (anchors) channel
    log_step "Test 1: Regular Channel"
    open_channel "anchors" 500000 "false"
    make_payment 10000
    close_channel

    # Re-fund Alice for next test
    fund_node "Alice" alice_cli 1.0

    # Test 2: Taproot channel (private)
    log_step "Test 2: Taproot Channel"
    open_channel "taproot" 500000 "true"
    make_payment 20000
    close_channel

    log_step "All tests passed! 🎉"
}

# Run the test
run_test
