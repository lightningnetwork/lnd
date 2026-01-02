#!/bin/bash
#
# Wallet Rescan Test Script for LND Esplora Backend
#
# This script tests wallet recovery/rescan functionality:
# 1. Start LND with a seed phrase (wallet creation)
# 2. Fund the wallet with on-chain funds
# 3. Record seed phrase and wallet birthday
# 4. Nuke the wallet data
# 5. Restore from seed phrase with wallet birthday
# 6. Verify on-chain funds are recovered via rescan
#
# Prerequisites:
# - Bitcoin Core running (native or in Docker)
# - Esplora API server (electrs/mempool-electrs) running
# - Go installed for building LND
#
# Usage:
#   ./scripts/test-esplora-wallet-rescan.sh [esplora_url]
#
# Example:
#   ./scripts/test-esplora-wallet-rescan.sh http://127.0.0.1:3002
#

set -e

# Configuration
ESPLORA_URL="${1:-http://127.0.0.1:3002}"
TEST_DIR="./test-esplora-wallet-rescan"
ALICE_DIR="$TEST_DIR/alice"
ALICE_PORT=10027
ALICE_REST=8097
ALICE_PEER=9752

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

    pkill -f "lnd-esplora.*test-esplora-wallet-rescan" 2>/dev/null || true

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

    if ! curl -s "${ESPLORA_URL}/blocks/tip/height" &>/dev/null; then
        log_error "Esplora API not reachable at $ESPLORA_URL"
        exit 1
    fi
    log_info "Esplora API reachable at $ESPLORA_URL"

    if [ ! -f "./lnd-esplora" ]; then
        log_info "Building lnd-esplora..."
        go build -o lnd-esplora ./cmd/lnd
    fi

    if [ ! -f "./lncli-esplora" ]; then
        log_info "Building lncli-esplora..."
        go build -o lncli-esplora ./cmd/lncli
    fi

    if ! command -v expect &> /dev/null; then
        log_error "expect not found. Please install expect (brew install expect or apt-get install expect)"
        exit 1
    fi
    log_info "expect found"

    log_info "All prerequisites met!"
}

setup_directory() {
    log_step "Setting up test directory..."

    rm -rf "$TEST_DIR"
    mkdir -p "$ALICE_DIR"

    # Create config WITHOUT noseedbackup - we want to use a real seed
    cat > "$ALICE_DIR/lnd.conf" << EOF
[Bitcoin]
bitcoin.regtest=true
bitcoin.node=esplora

[esplora]
esplora.url=$ESPLORA_URL

[Application Options]
debuglevel=debug,LNWL=trace,BTWL=trace,ESPN=trace
listen=127.0.0.1:$ALICE_PEER
rpclisten=127.0.0.1:$ALICE_PORT
restlisten=127.0.0.1:$ALICE_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

    log_info "Created config for Alice at $ALICE_DIR (with seed backup enabled)"
}

start_node_fresh() {
    log_info "Starting Alice (fresh wallet creation)..."

    ./lnd-esplora --lnddir="$ALICE_DIR" > "$ALICE_DIR/lnd.log" 2>&1 &
    echo $! > "$ALICE_DIR/lnd.pid"

    # Wait for LND to be ready for wallet creation
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        if ./lncli-esplora --lnddir="$ALICE_DIR" --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT state 2>/dev/null | grep -q "WAITING_TO_START\|NON_EXISTING"; then
            log_info "LND ready for wallet creation"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "LND failed to start. Check $ALICE_DIR/lnd.log"
    tail -50 "$ALICE_DIR/lnd.log"
    exit 1
}

start_node_unlocked() {
    log_info "Starting Alice (existing wallet)..."

    ./lnd-esplora --lnddir="$ALICE_DIR" > "$ALICE_DIR/lnd.log" 2>&1 &
    echo $! > "$ALICE_DIR/lnd.pid"

    # Wait for LND to be ready for unlock
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local state=$(./lncli-esplora --lnddir="$ALICE_DIR" --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT state 2>/dev/null | jq -r '.state // ""')
        if [ "$state" = "LOCKED" ] || [ "$state" = "WAITING_TO_START" ] || [ "$state" = "NON_EXISTING" ]; then
            log_info "LND ready (state: $state)"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "LND failed to start. Check $ALICE_DIR/lnd.log"
    tail -50 "$ALICE_DIR/lnd.log"
    exit 1
}

stop_node() {
    log_info "Stopping Alice..."
    if [ -f "$ALICE_DIR/lnd.pid" ]; then
        kill $(cat "$ALICE_DIR/lnd.pid") 2>/dev/null || true
        rm -f "$ALICE_DIR/lnd.pid"
        sleep 3
    fi
}

alice_cli() {
    ./lncli-esplora --lnddir="$ALICE_DIR" --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT "$@"
}

mine_blocks() {
    local count=${1:-1}
    local addr=$(btc getnewaddress)
    btc generatetoaddress $count $addr > /dev/null
    log_debug "Mined $count block(s)"
    sleep 3
}

wait_for_sync() {
    local max_attempts=${1:-60}
    local attempt=0

    log_info "Waiting for Alice to sync (timeout: ${max_attempts}s)..."
    while [ $attempt -lt $max_attempts ]; do
        local synced=$(alice_cli getinfo 2>/dev/null | jq -r '.synced_to_chain // "false"')
        if [ "$synced" = "true" ]; then
            log_info "Alice synced to chain"
            return 0
        fi
        if [ $((attempt % 30)) -eq 0 ] && [ $attempt -gt 0 ]; then
            log_debug "Still syncing... ($attempt/${max_attempts}s)"
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "Alice failed to sync after ${max_attempts}s"
    return 1
}

wait_for_balance() {
    local expected_min=$1
    local max_attempts=60
    local attempt=0

    log_info "Waiting for balance >= $expected_min sats..."
    while [ $attempt -lt $max_attempts ]; do
        local balance=$(alice_cli walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
        if [ "$balance" != "0" ] && [ "$balance" != "null" ] && [ "$balance" -ge "$expected_min" ] 2>/dev/null; then
            log_info "Balance confirmed: $balance sats"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "Balance not detected after $max_attempts attempts"
    return 1
}

create_wallet() {
    log_step "Creating new wallet with seed phrase..."

    local wallet_password="testpassword123"

    # Use lncli create with expect to handle interactive prompts
    expect << EOF > "$TEST_DIR/wallet_creation.log" 2>&1
set timeout 60
spawn ./lncli-esplora --lnddir=$ALICE_DIR --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT create

expect "Input wallet password:"
send "$wallet_password\r"

expect "Confirm password:"
send "$wallet_password\r"

expect "Do you have an existing cipher seed mnemonic"
send "n\r"

expect "Your cipher seed can optionally be encrypted"
send "\r"

expect "Input your passphrase if you wish to encrypt it"
send "\r"

expect -re "---------------BEGIN LND CIPHER SEED---------------(.*)---------------END LND CIPHER SEED---------------"
set seed \$expect_out(1,string)

expect "lnd successfully initialized"

puts "SEED_OUTPUT:\$seed"
EOF

    # Extract seed from output - parse lines between BEGIN/END markers
    # Use grep -oE to extract lowercase words (3+ chars) from seed lines
    SEED_PHRASE=$(sed -n '/BEGIN LND CIPHER SEED/,/END LND CIPHER SEED/p' "$TEST_DIR/wallet_creation.log" | \
        grep -E "^\s*[0-9]+\." | \
        grep -oE '[a-z]{3,}' | \
        head -24 | \
        tr '\n' ' ' | \
        sed 's/ $//')

    # Count words
    local word_count=$(echo "$SEED_PHRASE" | wc -w | tr -d ' ')

    if [ -z "$SEED_PHRASE" ] || [ "$word_count" -ne 24 ]; then
        log_error "Failed to extract seed phrase (got $word_count words: $SEED_PHRASE)"
        cat "$TEST_DIR/wallet_creation.log"
        exit 1
    fi

    log_info "Seed phrase captured (24 words)"
    echo "$SEED_PHRASE" > "$TEST_DIR/seed_phrase.txt"

    # Store password for later
    echo "$wallet_password" > "$TEST_DIR/wallet_password.txt"

    # Wait for wallet to be ready
    sleep 5

    # Get wallet birthday (current block height)
    WALLET_BIRTHDAY=$(btc getblockchaininfo | jq -r '.blocks')
    echo "$WALLET_BIRTHDAY" > "$TEST_DIR/wallet_birthday.txt"
    log_info "Wallet birthday (block height): $WALLET_BIRTHDAY"
}

unlock_wallet() {
    local password=$(cat "$TEST_DIR/wallet_password.txt")

    log_info "Unlocking wallet..."

    expect << EOF > /dev/null 2>&1
set timeout 30
spawn ./lncli-esplora --lnddir=$ALICE_DIR --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT unlock

expect "Input wallet password:"
send "$password\r"

expect eof
EOF

    sleep 3
    log_info "Wallet unlocked"
}

restore_wallet() {
    local seed_phrase=$(cat "$TEST_DIR/seed_phrase.txt")
    local password=$(cat "$TEST_DIR/wallet_password.txt")

    log_step "Restoring wallet from seed phrase..."
    log_info "Seed birthday is encoded in aezeed - no separate birthday needed"

    expect << EOF > "$TEST_DIR/wallet_restore.log" 2>&1
set timeout 120
spawn ./lncli-esplora --lnddir=$ALICE_DIR --network=regtest --rpcserver=127.0.0.1:$ALICE_PORT create

expect "Input wallet password:"
send "$password\r"

expect "Confirm password:"
send "$password\r"

expect "Do you have an existing cipher seed mnemonic"
send "y\r"

expect "Input your 24-word mnemonic separated by spaces:"
send "$seed_phrase\r"

expect "Input your cipher seed passphrase"
send "\r"

expect "Input an optional address look-ahead"
send "2500\r"

expect "lnd successfully initialized"
EOF

    log_info "Wallet restoration initiated"
    sleep 5
}

nuke_wallet() {
    log_step "Nuking wallet data..."

    # Stop node first
    stop_node

    # Remove wallet data but keep config
    log_info "Removing wallet data from $ALICE_DIR/data"
    rm -rf "$ALICE_DIR/data"

    # Also remove any macaroons
    rm -f "$ALICE_DIR/*.macaroon"

    log_info "Wallet data nuked!"
}

run_rescan_test() {
    log_step "Starting Wallet Rescan Test"

    # Phase 1: Create wallet and fund it
    log_step "Phase 1: Create and fund wallet"

    setup_directory
    start_node_fresh
    create_wallet
    wait_for_sync

    # Get addresses before funding
    local addr1=$(alice_cli newaddress p2wkh | jq -r '.address')
    local addr2=$(alice_cli newaddress p2tr | jq -r '.address')
    log_info "Generated addresses:"
    log_info "  P2WPKH: $addr1"
    log_info "  P2TR: $addr2"

    # Fund wallet with multiple UTXOs
    log_info "Sending funds to wallet..."
    btc sendtoaddress "$addr1" 0.5 > /dev/null
    btc sendtoaddress "$addr2" 0.3 > /dev/null
    btc sendtoaddress "$addr1" 0.2 > /dev/null

    # Mine to confirm
    mine_blocks 6

    # Wait for balance
    wait_for_balance 90000000  # ~1 BTC = 100M sats, expect at least 0.9 BTC

    # Record balance before nuking
    local balance_before=$(alice_cli walletbalance | jq -r '.confirmed_balance')
    local utxos_before=$(alice_cli listunspent | jq -r '.utxos | length')
    log_info "Balance before nuke: $balance_before sats"
    log_info "UTXOs before nuke: $utxos_before"

    echo "$balance_before" > "$TEST_DIR/balance_before.txt"
    echo "$utxos_before" > "$TEST_DIR/utxos_before.txt"

    # Mine more blocks to advance chain
    log_info "Mining additional blocks..."
    mine_blocks 10

    # Phase 2: Nuke wallet
    log_step "Phase 2: Nuke wallet data"
    nuke_wallet

    # Phase 3: Restore from seed
    log_step "Phase 3: Restore wallet from seed"
    start_node_fresh
    restore_wallet

    # Wait for rescan to complete - this takes longer due to address scanning
    log_step "Phase 4: Waiting for wallet rescan..."
    log_info "Recovery mode scans many addresses - this may take a few minutes..."
    wait_for_sync 300

    # Give extra time for rescan to find UTXOs
    log_info "Waiting for rescan to discover UTXOs..."
    local max_wait=180
    local waited=0
    while [ $waited -lt $max_wait ]; do
        local current_balance=$(alice_cli walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
        if [ "$current_balance" != "0" ] && [ "$current_balance" != "null" ]; then
            log_info "Balance detected: $current_balance sats"
            break
        fi
        sleep 10
        waited=$((waited + 10))
        if [ $((waited % 30)) -eq 0 ]; then
            log_debug "Still scanning for UTXOs... ($waited/$max_wait seconds)"
        fi
    done

    # Phase 5: Verify recovery
    log_step "Phase 5: Verify wallet recovery"

    local balance_after=$(alice_cli walletbalance | jq -r '.confirmed_balance')
    local utxos_after=$(alice_cli listunspent | jq -r '.utxos | length')
    local balance_before=$(cat "$TEST_DIR/balance_before.txt")
    local utxos_before=$(cat "$TEST_DIR/utxos_before.txt")

    log_info ""
    log_info "=== Recovery Results ==="
    log_info "Balance before nuke: $balance_before sats"
    log_info "Balance after restore: $balance_after sats"
    log_info "UTXOs before nuke: $utxos_before"
    log_info "UTXOs after restore: $utxos_after"
    log_info ""

    # Check results
    local success=true

    if [ "$balance_after" -eq "$balance_before" ] 2>/dev/null; then
        echo -e "${GREEN}✓ Balance fully recovered!${NC}"
    elif [ "$balance_after" -gt 0 ] 2>/dev/null; then
        echo -e "${YELLOW}⚠ Partial balance recovered: $balance_after / $balance_before sats${NC}"
        success=false
    else
        echo -e "${RED}✗ No balance recovered!${NC}"
        success=false
    fi

    if [ "$utxos_after" -eq "$utxos_before" ] 2>/dev/null; then
        echo -e "${GREEN}✓ All UTXOs recovered!${NC}"
    elif [ "$utxos_after" -gt 0 ] 2>/dev/null; then
        echo -e "${YELLOW}⚠ Partial UTXOs recovered: $utxos_after / $utxos_before${NC}"
    else
        echo -e "${RED}✗ No UTXOs recovered!${NC}"
        success=false
    fi

    echo ""

    # Show UTXO details
    log_debug "=== UTXOs After Recovery ==="
    alice_cli listunspent | jq '.utxos[] | {address, amount_sat, confirmations, address_type}'

    if [ "$success" = true ]; then
        log_step "Wallet Rescan Test PASSED! 🎉"
        echo ""
        echo "The Esplora backend successfully:"
        echo "  1. Created wallet with seed phrase"
        echo "  2. Funded wallet with on-chain funds"
        echo "  3. Restored wallet from seed phrase"
        echo "  4. Recovered all funds via blockchain rescan"
        echo ""
    else
        log_step "Wallet Rescan Test FAILED"
        echo ""
        echo "The wallet recovery did not fully succeed."
        echo "Check logs at: $ALICE_DIR/lnd.log"
        echo ""
        echo "Things to investigate:"
        echo "  - Wallet birthday may be incorrect"
        echo "  - Recovery window may be too small"
        echo "  - Esplora address/scripthash scanning may have issues"
        echo ""
        exit 1
    fi
}

# Main
main() {
    echo -e "${GREEN}"
    echo "============================================"
    echo "  LND Esplora Wallet Rescan Test Script"
    echo "============================================"
    echo -e "${NC}"
    echo ""
    echo "Esplora URL: $ESPLORA_URL"
    echo ""

    check_prerequisites
    run_rescan_test
}

main "$@"
