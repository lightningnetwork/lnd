#!/bin/bash
#
# End-to-End Test Script for LND Electrum Backend
#
# This script tests the Electrum backend implementation by:
# 1. Starting two LND nodes with Electrum backend
# 2. Funding the first node
# 3. Opening a channel between the nodes
# 4. Making payments
# 5. Closing the channel
#
# Prerequisites:
# - Bitcoin Core running (native or in Docker)
# - Electrum server (electrs/mempool-electrs) running and connected to Bitcoin Core
# - Go installed for building LND
#
# Usage:
#   ./scripts/test-electrum-e2e.sh [electrum_server:port]
#
# Example:
#   ./scripts/test-electrum-e2e.sh 127.0.0.1:50001
#
# Environment Variables:
#   BITCOIN_CLI      - Path to bitcoin-cli or docker command (auto-detected)
#   DOCKER_BITCOIN   - Set to container name if using Docker (e.g., "bitcoind")
#   RPC_USER         - Bitcoin RPC username (default: "second")
#   RPC_PASS         - Bitcoin RPC password (default: "ark")
#   REBUILD          - Set to "1" to force rebuild of lnd-electrum
#

set -e

# Configuration
ELECTRUM_SERVER="${1:-127.0.0.1:50001}"
ELECTRUM_REST="${2:-http://127.0.0.1:3002}"
TEST_DIR="./test-electrum-e2e"
ALICE_DIR="$TEST_DIR/alice"
BOB_DIR="$TEST_DIR/bob"
ALICE_PORT=10011
ALICE_REST=8081
ALICE_PEER=9736
BOB_PORT=10012
BOB_REST=8082
BOB_PEER=9737

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

    # Kill any remaining lnd-electrum processes from this test
    pkill -f "lnd-electrum.*test-electrum-e2e" 2>/dev/null || true

    log_info "Cleanup complete"
}

# Set trap to cleanup on exit
trap cleanup EXIT

detect_bitcoin_cli() {
    log_info "Detecting Bitcoin Core setup..."

    # Check for Docker container with "bitcoind" in the name (handles prefixes like scripts-bitcoind-1)
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

    # Check if Electrum server is reachable
    if ! nc -z ${ELECTRUM_SERVER%:*} ${ELECTRUM_SERVER#*:} 2>/dev/null; then
        log_error "Electrum server not reachable at $ELECTRUM_SERVER"
        log_error "Start your Electrum server (electrs, mempool-electrs, etc.)"
        exit 1
    fi
    log_info "Electrum server reachable at $ELECTRUM_SERVER"

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
    log_step "Building LND with Electrum support..."

    if [ ! -f "./lnd-electrum" ] || [ "$REBUILD" = "1" ]; then
        go build -o lnd-electrum -tags="electrum" ./cmd/lnd
        log_info "Built lnd-electrum"
    else
        log_info "lnd-electrum already exists, skipping build"
    fi

    if [ ! -f "./lncli-electrum" ] || [ "$REBUILD" = "1" ]; then
        go build -o lncli-electrum -tags="electrum" ./cmd/lncli
        log_info "Built lncli-electrum"
    else
        log_info "lncli-electrum already exists, skipping build"
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
bitcoin.node=electrum

[electrum]
electrum.server=$ELECTRUM_SERVER
electrum.ssl=false
electrum.resturl=$ELECTRUM_REST

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
bitcoin.node=electrum

[electrum]
electrum.server=$ELECTRUM_SERVER
electrum.ssl=false
electrum.resturl=$ELECTRUM_REST

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

    ./lnd-electrum --lnddir="$dir" > "$dir/lnd.log" 2>&1 &
    echo $! > "$dir/lnd.pid"

    # Wait for node to start
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        if ./lncli-electrum --lnddir="$dir" --network=regtest --rpcserver=127.0.0.1:$port getinfo &> /dev/null; then
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
    ./lncli-electrum --lnddir="$ALICE_DIR" --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT "$@"
}

bob_cli() {
    ./lncli-electrum --lnddir="$BOB_DIR" --network=regtest --rpcserver=127.0.0.1:$BOB_PORT "$@"
}

mine_blocks() {
    local count=${1:-1}
    local addr=$(btc getnewaddress)
    btc generatetoaddress $count $addr > /dev/null
    log_info "Mined $count block(s)"
    sleep 3  # Give Electrum time to index
}

wait_for_balance() {
    local name=$1
    local cli_func=$2
    local min_balance=${3:-1}

    log_info "Waiting for $name to detect balance..."
    local max_attempts=60
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local balance=$($cli_func walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
        if [ "$balance" != "0" ] && [ "$balance" != "null" ] && [ "$balance" -ge "$min_balance" ]; then
            log_info "$name balance detected: $balance sats"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name balance not detected after $max_attempts attempts"
    return 1
}

wait_for_sync() {
    local name=$1
    local cli_func=$2

    log_info "Waiting for $name to sync..."
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local synced=$($cli_func getinfo 2>/dev/null | jq -r '.synced_to_chain')
        if [ "$synced" = "true" ]; then
            log_info "$name synced to chain"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name failed to sync"
    exit 1
}

wait_for_channel_open() {
    local expected=${1:-1}
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local active=$(alice_cli listchannels 2>/dev/null | jq -r '.channels | length // 0')
        if [ "$active" != "" ] && [ "$active" != "null" ] && [ "$active" -ge "$expected" ] 2>/dev/null; then
            log_info "Channel opened successfully (active channels: $active)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "Channel failed to open after $max_attempts attempts"
    alice_cli pendingchannels 2>/dev/null || true
    exit 1
}

wait_for_channel_close() {
    local expected=${1:-1}
    local max_attempts=20
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local closed=$(alice_cli closedchannels 2>/dev/null | jq '.channels | length // 0')
        if [ "$closed" != "" ] && [ "$closed" != "null" ] && [ "$closed" -ge "$expected" ] 2>/dev/null; then
            log_info "Channel closed successfully (closed channels: $closed)"
            return 0
        fi

        # Mine a block to help detection
        mine_blocks 1
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "Channel failed to close after $max_attempts attempts"
    alice_cli pendingchannels 2>/dev/null || true
    alice_cli closedchannels 2>/dev/null || true
    exit 1
}

run_tests() {
    log_step "Starting LND nodes..."
    start_node "Alice" "$ALICE_DIR" "$ALICE_PORT"
    start_node "Bob" "$BOB_DIR" "$BOB_PORT"

    # Wait for both nodes to sync
    wait_for_sync "Alice" alice_cli
    wait_for_sync "Bob" bob_cli

    log_step "Getting node info..."
    local alice_pubkey=$(alice_cli getinfo | jq -r '.identity_pubkey')
    local bob_pubkey=$(bob_cli getinfo | jq -r '.identity_pubkey')
    log_info "Alice pubkey: $alice_pubkey"
    log_info "Bob pubkey: $bob_pubkey"

    log_step "Funding Alice's wallet (taproot + segwit addresses)..."

    # Fund taproot address
    local alice_tr_addr=$(alice_cli newaddress p2tr | jq -r '.address')
    log_info "Alice's taproot address: $alice_tr_addr"
    local txid1=$(btc sendtoaddress "$alice_tr_addr" 0.5)
    log_info "Sent 0.5 BTC to taproot address, txid: $txid1"

    # Fund segwit address
    local alice_sw_addr=$(alice_cli newaddress p2wkh | jq -r '.address')
    log_info "Alice's segwit address: $alice_sw_addr"
    local txid2=$(btc sendtoaddress "$alice_sw_addr" 0.5)
    log_info "Sent 0.5 BTC to segwit address, txid: $txid2"

    # Mine blocks and wait for balance
    mine_blocks 6
    sleep 3

    if ! wait_for_balance "Alice" alice_cli 1000; then
        log_error "Alice's funding failed"
        exit 1
    fi

    local balance=$(alice_cli walletbalance | jq -r '.confirmed_balance')
    log_info "Alice's confirmed balance: $balance sats"

    log_step "Connecting Alice to Bob..."
    alice_cli connect "${bob_pubkey}@127.0.0.1:$BOB_PEER"
    sleep 2

    local peers=$(alice_cli listpeers | jq '.peers | length')
    if [ "$peers" = "0" ]; then
        log_error "Failed to connect Alice to Bob"
        exit 1
    fi
    log_info "Alice connected to Bob"

    # ==================== TEST 1: Regular anchors channel ====================
    log_step "Opening regular (anchors) channel from Alice to Bob..."
    alice_cli openchannel --node_key="$bob_pubkey" --local_amt=250000

    mine_blocks 6
    wait_for_channel_open 1

    log_info "Regular channel info:"
    alice_cli listchannels | jq '.channels[0] | {channel_point, capacity, commitment_type}'

    log_step "Payment over regular channel..."
    local invoice1=$(bob_cli addinvoice --amt=5000 | jq -r '.payment_request')
    alice_cli payinvoice --force "$invoice1"
    log_info "Payment 1 succeeded"

    log_step "Closing regular channel..."
    local chan1=$(alice_cli listchannels | jq -r '.channels[0].channel_point')
    alice_cli closechannel --funding_txid="${chan1%:*}" --output_index="${chan1#*:}"

    mine_blocks 6
    wait_for_channel_close 1

    # ==================== TEST 2: Taproot channel ====================
    log_step "Opening taproot channel from Alice to Bob..."
    alice_cli openchannel --node_key="$bob_pubkey" --local_amt=250000 --channel_type=taproot --private

    mine_blocks 6
    wait_for_channel_open 1

    log_info "Taproot channel info:"
    alice_cli listchannels | jq '.channels[0] | {channel_point, capacity, commitment_type, private}'

    log_step "Payment over taproot channel..."
    local invoice2=$(bob_cli addinvoice --amt=5000 | jq -r '.payment_request')
    alice_cli payinvoice --force "$invoice2"
    log_info "Payment 2 succeeded"

    log_step "Closing taproot channel..."
    local chan2=$(alice_cli listchannels | jq -r '.channels[0].channel_point')
    alice_cli closechannel --funding_txid="${chan2%:*}" --output_index="${chan2#*:}"

    mine_blocks 6
    wait_for_channel_close 2

    log_step "Final wallet balances..."
    log_info "Alice's final balance: $(alice_cli walletbalance | jq -r '.confirmed_balance') sats"
    log_info "Bob's final balance: $(bob_cli walletbalance | jq -r '.confirmed_balance') sats"

    log_step "TEST COMPLETED SUCCESSFULLY!"
    echo -e "${GREEN}"
    echo "============================================"
    echo "  All Electrum backend tests passed!  "
    echo "============================================"
    echo -e "${NC}"
    echo ""
    echo "Summary:"
    echo "  ✓ Two LND nodes started with Electrum backend"
    echo "  ✓ Chain synchronization working"
    echo "  ✓ Taproot + SegWit wallet addresses funded"
    echo "  ✓ Regular (anchors) channel: open, pay, close"
    echo "  ✓ Taproot channel: open, pay, close"
    echo ""
}

# Main
main() {
    echo -e "${GREEN}"
    echo "============================================"
    echo "  LND Electrum Backend E2E Test Script"
    echo "============================================"
    echo -e "${NC}"
    echo ""
    echo "Electrum Server: $ELECTRUM_SERVER"
    echo "Electrum REST:   $ELECTRUM_REST"
    echo ""

    check_prerequisites
    build_lnd
    setup_directories
    run_tests
}

main "$@"
