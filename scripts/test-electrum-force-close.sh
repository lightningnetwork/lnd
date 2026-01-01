#!/bin/bash
#
# Force Close E2E Test Script for LND Electrum Backend
#
# This script specifically tests force close scenarios to debug sweep
# transaction creation for time-locked outputs.
#
# Known Issue: After force close, the commitSweepResolver launches but
# doesn't create sweep requests for CommitmentTimeLock outputs after
# the CSV delay expires.
#
# Prerequisites:
# - Bitcoin Core running (native or in Docker)
# - Electrum server (electrs/mempool-electrs) running
# - LND built with electrum tag
#
# Usage:
#   ./scripts/test-electrum-force-close.sh [electrum_server:port] [rest_url]
#
# Example:
#   ./scripts/test-electrum-force-close.sh 127.0.0.1:50001 http://127.0.0.1:3002
#

set -e

# Configuration
ELECTRUM_SERVER="${1:-127.0.0.1:50001}"
ELECTRUM_REST="${2:-http://127.0.0.1:3002}"
TEST_DIR="./test-electrum-force-close"
ALICE_DIR="$TEST_DIR/alice"
BOB_DIR="$TEST_DIR/bob"
ALICE_PORT=10021
ALICE_REST=8091
ALICE_PEER=9746
BOB_PORT=10022
BOB_REST=8092
BOB_PEER=9747

# Bitcoin RPC Configuration
RPC_USER="${RPC_USER:-second}"
RPC_PASS="${RPC_PASS:-ark}"
DOCKER_BITCOIN="${DOCKER_BITCOIN:-}"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() {
    echo -e "${GREEN}[INFO]${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

log_debug() {
    echo -e "${CYAN}[DEBUG]${NC} $1"
}

log_step() {
    echo -e "\n${GREEN}========================================${NC}"
    echo -e "${GREEN}$1${NC}"
    echo -e "${GREEN}========================================${NC}\n"
}

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

    if [ -f "$ALICE_DIR/lnd.pid" ]; then
        kill $(cat "$ALICE_DIR/lnd.pid") 2>/dev/null || true
        rm -f "$ALICE_DIR/lnd.pid"
    fi

    if [ -f "$BOB_DIR/lnd.pid" ]; then
        kill $(cat "$BOB_DIR/lnd.pid") 2>/dev/null || true
        rm -f "$BOB_DIR/lnd.pid"
    fi

    pkill -f "lnd-electrum.*test-electrum-force-close" 2>/dev/null || true

    log_info "Cleanup complete"
}

trap cleanup EXIT

detect_bitcoin_cli() {
    for container in $(docker ps --format '{{.Names}}' 2>/dev/null | grep -i bitcoin); do
        if docker exec "$container" bitcoin-cli -regtest -rpcuser="$RPC_USER" -rpcpassword="$RPC_PASS" getblockchaininfo &>/dev/null; then
            DOCKER_BITCOIN="$container"
            log_info "Found Bitcoin Core in Docker container: $DOCKER_BITCOIN"
            return 0
        fi
    done

    if command -v bitcoin-cli &> /dev/null; then
        log_info "Found native bitcoin-cli"
        return 0
    fi

    return 1
}

check_prerequisites() {
    log_step "Checking prerequisites..."

    if ! detect_bitcoin_cli; then
        log_error "Bitcoin Core not found"
        exit 1
    fi

    if ! btc getblockchaininfo &> /dev/null; then
        log_error "Bitcoin Core not responding"
        exit 1
    fi

    local blocks=$(btc getblockchaininfo | jq -r '.blocks')
    log_info "Current block height: $blocks"

    if ! nc -z ${ELECTRUM_SERVER%:*} ${ELECTRUM_SERVER#*:} 2>/dev/null; then
        log_error "Electrum server not reachable at $ELECTRUM_SERVER"
        exit 1
    fi
    log_info "Electrum server reachable at $ELECTRUM_SERVER"

    if [ ! -f "./lnd-electrum" ]; then
        log_error "lnd-electrum binary not found. Build with: go build -o lnd-electrum -tags=electrum ./cmd/lnd"
        exit 1
    fi

    log_info "All prerequisites met!"
}

setup_directories() {
    log_step "Setting up test directories..."

    rm -rf "$TEST_DIR"
    mkdir -p "$ALICE_DIR" "$BOB_DIR"

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
debuglevel=debug,SWPR=trace,CNCT=trace,NTFN=trace
listen=127.0.0.1:$ALICE_PEER
rpclisten=127.0.0.1:$ALICE_PORT
restlisten=127.0.0.1:$ALICE_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

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
debuglevel=debug,SWPR=trace,CNCT=trace,NTFN=trace
listen=127.0.0.1:$BOB_PEER
rpclisten=127.0.0.1:$BOB_PORT
restlisten=127.0.0.1:$BOB_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

    log_info "Created configs with trace logging for SWPR, CNCT, NTFN"
}

start_node() {
    local name=$1
    local dir=$2
    local port=$3

    log_info "Starting $name..."

    ./lnd-electrum --lnddir="$dir" > "$dir/lnd.log" 2>&1 &
    echo $! > "$dir/lnd.pid"

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
    tail -50 "$dir/lnd.log"
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
    log_debug "Mined $count block(s)"
    sleep 2
}

wait_for_sync() {
    local name=$1
    local cli_func=$2

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

wait_for_balance() {
    local name=$1
    local cli_func=$2

    local max_attempts=60
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local balance=$($cli_func walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
        if [ "$balance" != "0" ] && [ "$balance" != "null" ]; then
            log_info "$name balance: $balance sats"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name balance not detected"
    return 1
}

wait_for_channel() {
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local active=$(alice_cli listchannels 2>/dev/null | jq -r '.channels | length // 0')
        if [ "$active" -gt 0 ] 2>/dev/null; then
            log_info "Channel active"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "Channel failed to open"
    alice_cli pendingchannels
    exit 1
}

show_pending_channels() {
    echo ""
    log_debug "=== Alice Pending Channels ==="
    alice_cli pendingchannels | jq '{
        pending_force_closing: .pending_force_closing_channels | map({
            channel_point: .channel.channel_point,
            local_balance: .channel.local_balance,
            remote_balance: .channel.remote_balance,
            limbo_balance: .limbo_balance,
            maturity_height: .maturity_height,
            blocks_til_maturity: .blocks_til_maturity,
            recovered_balance: .recovered_balance
        }),
        waiting_close: .waiting_close_channels | length
    }'

    echo ""
    log_debug "=== Bob Pending Channels ==="
    bob_cli pendingchannels | jq '{
        pending_force_closing: .pending_force_closing_channels | map({
            channel_point: .channel.channel_point,
            local_balance: .channel.local_balance,
            limbo_balance: .limbo_balance,
            blocks_til_maturity: .blocks_til_maturity
        }),
        waiting_close: .waiting_close_channels | length
    }'
}

show_closed_channels() {
    echo ""
    log_debug "=== Alice Closed Channels ==="
    alice_cli closedchannels | jq '.channels | map({
        channel_point,
        close_type,
        settled_balance,
        time_locked_balance
    })'
}

check_sweep_logs() {
    local name=$1
    local dir=$2

    echo ""
    log_debug "=== $name Sweep-related logs (last 50 lines) ==="
    grep -i "sweep\|SWPR\|CommitmentTimeLock\|resolver\|mature" "$dir/lnd.log" 2>/dev/null | tail -50 || echo "No sweep logs found"
}

run_force_close_test() {
    log_step "Starting LND nodes..."
    start_node "Alice" "$ALICE_DIR" "$ALICE_PORT"
    start_node "Bob" "$BOB_DIR" "$BOB_PORT"

    wait_for_sync "Alice" alice_cli
    wait_for_sync "Bob" bob_cli

    log_step "Getting node info..."
    local alice_pubkey=$(alice_cli getinfo | jq -r '.identity_pubkey')
    local bob_pubkey=$(bob_cli getinfo | jq -r '.identity_pubkey')
    log_info "Alice pubkey: $alice_pubkey"
    log_info "Bob pubkey: $bob_pubkey"

    log_step "Funding Alice's wallet..."
    local alice_addr=$(alice_cli newaddress p2wkh | jq -r '.address')
    log_info "Alice's address: $alice_addr"

    btc sendtoaddress "$alice_addr" 1.0 > /dev/null
    mine_blocks 6
    sleep 3

    if ! wait_for_balance "Alice" alice_cli; then
        exit 1
    fi

    log_step "Connecting Alice to Bob..."
    alice_cli connect "${bob_pubkey}@127.0.0.1:$BOB_PEER" > /dev/null
    sleep 2

    log_step "Opening small channel (25k sats) for force close test..."
    alice_cli openchannel --node_key="$bob_pubkey" --local_amt=25000
    mine_blocks 6
    sleep 3
    wait_for_channel

    log_step "Making payment so Bob has balance..."
    local invoice=$(bob_cli addinvoice --amt=5000 | jq -r '.payment_request')
    alice_cli payinvoice --force "$invoice" > /dev/null 2>&1
    log_info "Payment complete - Bob now has 5000 sats in channel"

    local chan_point=$(alice_cli listchannels | jq -r '.channels[0].channel_point')
    log_info "Channel point: $chan_point"

    log_step "Recording balances before force close..."
    local alice_balance_before=$(alice_cli walletbalance | jq -r '.confirmed_balance')
    local bob_balance_before=$(bob_cli walletbalance | jq -r '.confirmed_balance')
    log_info "Alice on-chain balance: $alice_balance_before sats"
    log_info "Bob on-chain balance: $bob_balance_before sats"

    log_step "FORCE CLOSING CHANNEL (Alice initiates)..."
    local funding_txid="${chan_point%:*}"
    local output_index="${chan_point#*:}"
    alice_cli closechannel --force --funding_txid="$funding_txid" --output_index="$output_index"

    log_step "Mining 1 block to confirm force close TX..."
    mine_blocks 1
    sleep 3

    show_pending_channels

    local blocks_til=$(alice_cli pendingchannels | jq -r '.pending_force_closing_channels[0].blocks_til_maturity // 0')
    local maturity_height=$(alice_cli pendingchannels | jq -r '.pending_force_closing_channels[0].maturity_height // 0')
    log_info "Blocks until maturity: $blocks_til"
    log_info "Maturity height: $maturity_height"

    log_step "Mining 6 more blocks for Bob to receive funds..."
    mine_blocks 6
        sleep 5

        local bob_balance_after=$(bob_cli walletbalance | jq -r '.confirmed_balance')
    log_info "Bob on-chain balance after confirmations: $bob_balance_after sats"

    if [ "$bob_balance_after" -gt "$bob_balance_before" ]; then
        log_info "✓ Bob received funds immediately (no timelock for remote party)"
    else
        log_warn "✗ Bob has NOT received funds yet"
        check_sweep_logs "Bob" "$BOB_DIR"
    fi

    log_step "Mining blocks to pass Alice's timelock..."
    blocks_til=$(alice_cli pendingchannels | jq -r '.pending_force_closing_channels[0].blocks_til_maturity // 0')

    if [ "$blocks_til" -gt 0 ]; then
        log_info "Mining $blocks_til blocks to reach maturity..."

        # Mine in batches to show progress
        local mined=0
        while [ $mined -lt $blocks_til ]; do
            local batch=$((blocks_til - mined))
            if [ $batch -gt 20 ]; then
                batch=20
            fi
            mine_blocks $batch
            mined=$((mined + batch))

            local remaining=$(alice_cli pendingchannels | jq -r '.pending_force_closing_channels[0].blocks_til_maturity // 0')
            log_debug "Mined $mined blocks, $remaining remaining until maturity"
        done
    fi

    log_step "Timelock should now be expired. Mining additional blocks..."
    mine_blocks 10
    sleep 8

    show_pending_channels

    log_step "Checking sweep transaction creation..."
    check_sweep_logs "Alice" "$ALICE_DIR"

    log_step "Mining more blocks and waiting for sweep..."
    for i in {1..30}; do
        mine_blocks 1
        sleep 3

        local pending=$(alice_cli pendingchannels | jq '.pending_force_closing_channels | length')
        if [ "$pending" = "0" ]; then
            log_info "✓ Force close channel fully resolved!"
            break
        fi

        if [ $((i % 10)) -eq 0 ]; then
            log_debug "Still waiting for sweep (attempt $i/30)..."
            show_pending_channels
        fi
    done

    log_step "Final state..."

    local alice_balance_final=$(alice_cli walletbalance | jq -r '.confirmed_balance')
    local bob_balance_final=$(bob_cli walletbalance | jq -r '.confirmed_balance')

    log_info "Alice final balance: $alice_balance_final sats (was: $alice_balance_before)"
    log_info "Bob final balance: $bob_balance_final sats (was: $bob_balance_before)"

    show_pending_channels
    show_closed_channels

    log_step "Summary"
    echo ""

    local pending_force=$(alice_cli pendingchannels | jq '.pending_force_closing_channels | length')
    if [ "$pending_force" = "0" ]; then
        echo -e "${GREEN}✓ Force close completed successfully${NC}"
    else
        echo -e "${RED}✗ Force close still pending${NC}"
        echo ""
        log_warn "The time-locked output sweep is not working correctly."
        log_warn "Check the logs above for SWPR (sweeper) and CNCT (contract court) messages."
        echo ""
        log_info "Log files for further investigation:"
        log_info "  Alice: $ALICE_DIR/lnd.log"
        log_info "  Bob: $BOB_DIR/lnd.log"
        echo ""
        log_info "Key things to look for in logs:"
        log_info "  - 'commitSweepResolver' launching"
        log_info "  - 'CommitmentTimeLock' sweep requests"
        log_info "  - 'Registered sweep request' messages"
        log_info "  - Any errors from SWPR or CNCT"
    fi
    echo ""
}

# Main
main() {
    echo -e "${GREEN}"
    echo "============================================"
    echo "  LND Electrum Force Close Test Script"
    echo "============================================"
    echo -e "${NC}"
    echo ""
    echo "Electrum Server: $ELECTRUM_SERVER"
    echo "Electrum REST:   $ELECTRUM_REST"
    echo ""

    check_prerequisites
    setup_directories
    run_force_close_test
}

main "$@"
