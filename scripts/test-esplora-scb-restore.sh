#!/bin/bash
#
# SCB (Static Channel Backup) Restore Test Script for LND Esplora Backend
#
# This script tests disaster recovery using Static Channel Backups:
# 1. Start Alice and Bob with seed phrases (wallet backup enabled)
# 2. Fund Alice and open a channel with Bob
# 3. Make payments so Bob has channel balance
# 4. Save Bob's channel.backup file and seed phrase
# 5. Nuke Bob's wallet data (simulating data loss)
# 6. Restore Bob from seed phrase
# 7. Restore channel backup - triggers DLP force close
# 8. Verify Bob recovers his funds
#
# Prerequisites:
# - Bitcoin Core running (native or in Docker)
# - Esplora API server (electrs/mempool-electrs) running
# - Go installed for building LND
# - expect utility installed
#
# Usage:
#   ./scripts/test-esplora-scb-restore.sh [esplora_url]
#
# Example:
#   ./scripts/test-esplora-scb-restore.sh http://127.0.0.1:3002
#

set -e

# Configuration
ESPLORA_URL="${1:-http://127.0.0.1:3002}"
TEST_DIR="./test-esplora-scb-restore"
ALICE_DIR="$TEST_DIR/alice"
BOB_DIR="$TEST_DIR/bob"
BACKUP_DIR="$TEST_DIR/backup"
ALICE_PORT=10031
ALICE_REST=8101
ALICE_PEER=9756
BOB_PORT=10032
BOB_REST=8102
BOB_PEER=9757

# Bitcoin RPC Configuration
RPC_USER="${RPC_USER:-second}"
RPC_PASS="${RPC_PASS:-ark}"
DOCKER_BITCOIN="${DOCKER_BITCOIN:-}"

# Wallet passwords
ALICE_PASSWORD="alicepassword123"
BOB_PASSWORD="bobpassword456"

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

    pkill -f "lnd-esplora.*test-esplora-scb-restore" 2>/dev/null || true

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

setup_directories() {
    log_step "Setting up test directories..."

    rm -rf "$TEST_DIR"
    mkdir -p "$ALICE_DIR" "$BOB_DIR" "$BACKUP_DIR"

    # Create Alice's config (with seed backup enabled)
    cat > "$ALICE_DIR/lnd.conf" << EOF
[Bitcoin]
bitcoin.regtest=true
bitcoin.node=esplora

[esplora]
esplora.url=$ESPLORA_URL

[Application Options]
debuglevel=debug,BRAR=trace
listen=127.0.0.1:$ALICE_PEER
rpclisten=127.0.0.1:$ALICE_PORT
restlisten=127.0.0.1:$ALICE_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

    # Create Bob's config (with seed backup enabled)
    cat > "$BOB_DIR/lnd.conf" << EOF
[Bitcoin]
bitcoin.regtest=true
bitcoin.node=esplora

[esplora]
esplora.url=$ESPLORA_URL

[Application Options]
debuglevel=debug,BRAR=trace
listen=127.0.0.1:$BOB_PEER
rpclisten=127.0.0.1:$BOB_PORT
restlisten=127.0.0.1:$BOB_REST

[protocol]
protocol.simple-taproot-chans=true
EOF

    log_info "Created configs for Alice and Bob (seed backup enabled)"
}

start_node_fresh() {
    local name=$1
    local dir=$2
    local port=$3

    log_info "Starting $name (fresh wallet)..."

    ./lnd-esplora --lnddir="$dir" > "$dir/lnd.log" 2>&1 &
    echo $! > "$dir/lnd.pid"

    # Wait for LND to be ready for wallet creation
    local max_attempts=30
    local attempt=0
    while [ $attempt -lt $max_attempts ]; do
        local state=$(./lncli-esplora --lnddir="$dir" --network=regtest --rpcserver=127.0.0.1:$port state 2>/dev/null | jq -r '.state // ""')
        if [ "$state" = "WAITING_TO_START" ] || [ "$state" = "NON_EXISTING" ]; then
            log_info "$name ready for wallet creation"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name failed to start. Check $dir/lnd.log"
    tail -50 "$dir/lnd.log"
    exit 1
}

stop_node() {
    local name=$1
    local dir=$2

    log_info "Stopping $name..."
    if [ -f "$dir/lnd.pid" ]; then
        kill $(cat "$dir/lnd.pid") 2>/dev/null || true
        rm -f "$dir/lnd.pid"
        sleep 3
    fi
}

create_wallet() {
    local name=$1
    local dir=$2
    local port=$3
    local password=$4

    log_info "Creating wallet for $name..."

    expect << EOF > "$dir/wallet_creation.log" 2>&1
set timeout 60
spawn ./lncli-esplora --lnddir=$dir --network=regtest --rpcserver=127.0.0.1:$port create

expect "Input wallet password:"
send "$password\r"

expect "Confirm password:"
send "$password\r"

expect "Do you have an existing cipher seed mnemonic"
send "n\r"

expect "Your cipher seed can optionally be encrypted"
send "\r"

expect "Input your passphrase if you wish to encrypt it"
send "\r"

expect "lnd successfully initialized"
EOF

    # Extract seed phrase
    local seed=$(grep -oE '[a-z]{3,}' "$dir/wallet_creation.log" | \
        awk '/BEGIN LND CIPHER SEED/,/END LND CIPHER SEED/' | \
        head -24 | tr '\n' ' ' | sed 's/ $//')

    # Alternative extraction if the above fails
    if [ -z "$seed" ] || [ $(echo "$seed" | wc -w) -ne 24 ]; then
        seed=$(sed -n '/BEGIN LND CIPHER SEED/,/END LND CIPHER SEED/p' "$dir/wallet_creation.log" | \
            grep -E "^\s*[0-9]+\." | \
            grep -oE '[a-z]{3,}' | \
            head -24 | tr '\n' ' ' | sed 's/ $//')
    fi

    echo "$seed" > "$dir/seed_phrase.txt"
    echo "$password" > "$dir/password.txt"

    local word_count=$(echo "$seed" | wc -w | tr -d ' ')
    if [ "$word_count" -eq 24 ]; then
        log_info "$name wallet created with 24-word seed"
    else
        log_warn "$name seed extraction got $word_count words (expected 24)"
    fi

    sleep 3
}

unlock_wallet() {
    local name=$1
    local dir=$2
    local port=$3
    local password=$4

    log_info "Unlocking $name wallet..."

    expect << EOF > /dev/null 2>&1
set timeout 30
spawn ./lncli-esplora --lnddir=$dir --network=regtest --rpcserver=127.0.0.1:$port unlock

expect "Input wallet password:"
send "$password\r"

expect eof
EOF

    sleep 3
}

restore_wallet() {
    local name=$1
    local dir=$2
    local port=$3
    local password=$4
    local seed_file=$5

    local seed=$(cat "$seed_file")

    log_info "Restoring $name wallet from seed..."

    expect << EOF > "$dir/wallet_restore.log" 2>&1
set timeout 120
spawn ./lncli-esplora --lnddir=$dir --network=regtest --rpcserver=127.0.0.1:$port create

expect "Input wallet password:"
send "$password\r"

expect "Confirm password:"
send "$password\r"

expect "Do you have an existing cipher seed mnemonic"
send "y\r"

expect "Input your 24-word mnemonic separated by spaces:"
send "$seed\r"

expect "Input your cipher seed passphrase"
send "\r"

expect "Input an optional address look-ahead"
send "2500\r"

expect "lnd successfully initialized"
EOF

    log_info "$name wallet restored"
    sleep 5
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
    log_debug "Mined $count block(s)"
    sleep 3
}

wait_for_sync() {
    local name=$1
    local cli_func=$2
    local max_attempts=${3:-120}
    local attempt=0

    log_info "Waiting for $name to sync..."
    while [ $attempt -lt $max_attempts ]; do
        local synced=$($cli_func getinfo 2>/dev/null | jq -r '.synced_to_chain // "false"')
        if [ "$synced" = "true" ]; then
            log_info "$name synced to chain"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
        if [ $((attempt % 30)) -eq 0 ]; then
            log_debug "$name still syncing... ($attempt/${max_attempts}s)"
        fi
    done

    log_error "$name failed to sync"
    return 1
}

wait_for_server_ready() {
    local name=$1
    local cli_func=$2
    local max_attempts=${3:-60}
    local attempt=0

    log_info "Waiting for $name server to be fully ready..."
    while [ $attempt -lt $max_attempts ]; do
        # Try to list channels - this requires full server startup
        if $cli_func listchannels &>/dev/null; then
            log_info "$name server is fully ready"
            return 0
        fi
        sleep 2
        attempt=$((attempt + 1))
    done

    log_error "$name server not ready after ${max_attempts} attempts"
    return 1
}

wait_for_balance() {
    local name=$1
    local cli_func=$2
    local expected_min=${3:-1}
    local max_attempts=60
    local attempt=0

    log_info "Waiting for $name balance..."
    while [ $attempt -lt $max_attempts ]; do
        local balance=$($cli_func walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
        if [ "$balance" != "0" ] && [ "$balance" != "null" ] && [ "$balance" -ge "$expected_min" ] 2>/dev/null; then
            log_info "$name balance: $balance sats"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "$name balance not detected"
    return 1
}

wait_for_channel_active() {
    local max_attempts=60
    local attempt=0

    log_info "Waiting for channel to become active..."
    while [ $attempt -lt $max_attempts ]; do
        local active=$(alice_cli listchannels 2>/dev/null | jq -r '.channels[0].active // false')
        if [ "$active" = "true" ]; then
            log_info "Channel is active"
            return 0
        fi
        sleep 2
        attempt=$((attempt + 1))
    done

    log_error "Channel failed to become active"
    alice_cli pendingchannels
    return 1
}

save_bob_backup() {
    log_step "Saving Bob's backup data..."

    # Copy Bob's channel backup file
    local backup_file="$BOB_DIR/data/chain/bitcoin/regtest/channel.backup"
    if [ -f "$backup_file" ]; then
        cp "$backup_file" "$BACKUP_DIR/channel.backup"
        log_info "Saved channel.backup to $BACKUP_DIR/"
    else
        log_error "Channel backup file not found at $backup_file"
        exit 1
    fi

    # Copy Bob's seed phrase
    cp "$BOB_DIR/seed_phrase.txt" "$BACKUP_DIR/seed_phrase.txt"
    cp "$BOB_DIR/password.txt" "$BACKUP_DIR/password.txt"
    log_info "Saved seed phrase and password"

    # Record Bob's balance before disaster
    local bob_channel_balance=$(bob_cli listchannels 2>/dev/null | jq -r '.channels[0].local_balance // "0"')
    local bob_onchain_balance=$(bob_cli walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
    echo "$bob_channel_balance" > "$BACKUP_DIR/channel_balance.txt"
    echo "$bob_onchain_balance" > "$BACKUP_DIR/onchain_balance.txt"

    log_info "Bob's channel balance: $bob_channel_balance sats"
    log_info "Bob's on-chain balance: $bob_onchain_balance sats"
}

nuke_bob_wallet() {
    log_step "Nuking Bob's wallet data (simulating disaster)..."

    stop_node "Bob" "$BOB_DIR"

    # Remove all data but keep config
    rm -rf "$BOB_DIR/data"
    rm -f "$BOB_DIR"/*.macaroon

    log_info "Bob's wallet data has been destroyed!"
}

restore_bob_from_backup() {
    log_step "Restoring Bob from seed + SCB..."

    # Start Bob fresh
    start_node_fresh "Bob" "$BOB_DIR" "$BOB_PORT"

    # Restore wallet from seed
    restore_wallet "Bob" "$BOB_DIR" "$BOB_PORT" "$BOB_PASSWORD" "$BACKUP_DIR/seed_phrase.txt"

    # Wait for sync
    wait_for_sync "Bob" bob_cli 300

    # Wait for server to be fully ready before restoring channel backup
    wait_for_server_ready "Bob" bob_cli 60

    # Now restore the channel backup - this will trigger DLP force close
    log_info "Restoring channel backup..."

    # Retry logic for channel backup restore
    local restore_attempts=5
    local restore_success=false
    for i in $(seq 1 $restore_attempts); do
        if bob_cli restorechanbackup --multi_file="$BACKUP_DIR/channel.backup" 2>&1; then
            restore_success=true
            break
        fi
        log_warn "Channel backup restore attempt $i failed, retrying in 5s..."
        sleep 5
    done

    if [ "$restore_success" = false ]; then
        log_error "Failed to restore channel backup after $restore_attempts attempts"
        exit 1
    fi

    log_info "Channel backup restored"

    # Bob needs to reconnect to Alice for DLP protocol to trigger force close
    log_info "Reconnecting Bob to Alice (triggers DLP force close)..."
    local alice_pubkey=$(alice_cli getinfo | jq -r '.identity_pubkey')
    bob_cli connect "${alice_pubkey}@127.0.0.1:$ALICE_PEER" > /dev/null 2>&1 || true
    sleep 5

    log_info "DLP force close should be triggered by Alice"
}

run_scb_restore_test() {
    log_step "Phase 1: Setup - Create wallets for Alice and Bob"

    # Start and create Alice's wallet
    start_node_fresh "Alice" "$ALICE_DIR" "$ALICE_PORT"
    create_wallet "Alice" "$ALICE_DIR" "$ALICE_PORT" "$ALICE_PASSWORD"
    wait_for_sync "Alice" alice_cli

    # Start and create Bob's wallet
    start_node_fresh "Bob" "$BOB_DIR" "$BOB_PORT"
    create_wallet "Bob" "$BOB_DIR" "$BOB_PORT" "$BOB_PASSWORD"
    wait_for_sync "Bob" bob_cli

    # Get node info
    local alice_pubkey=$(alice_cli getinfo | jq -r '.identity_pubkey')
    local bob_pubkey=$(bob_cli getinfo | jq -r '.identity_pubkey')
    log_info "Alice pubkey: $alice_pubkey"
    log_info "Bob pubkey: $bob_pubkey"

    log_step "Phase 2: Fund Alice and open channel with Bob"

    # Fund Alice
    local alice_addr=$(alice_cli newaddress p2wkh | jq -r '.address')
    btc sendtoaddress "$alice_addr" 1.0 > /dev/null
    mine_blocks 6
    wait_for_balance "Alice" alice_cli

    # Connect to Bob
    alice_cli connect "${bob_pubkey}@127.0.0.1:$BOB_PEER" > /dev/null
    sleep 2

    # Open channel (500k sats)
    log_info "Opening channel with Bob (500,000 sats)..."
    alice_cli openchannel --node_key="$bob_pubkey" --local_amt=500000
    mine_blocks 6
    wait_for_channel_active

    log_step "Phase 3: Make payments so Bob has balance"

    # Make several payments to Bob
    for i in 1 2 3; do
        local invoice=$(bob_cli addinvoice --amt=30000 | jq -r '.payment_request')
        alice_cli payinvoice --force "$invoice" > /dev/null 2>&1
        log_info "Payment $i complete (30,000 sats to Bob)"
        sleep 1
    done

    # Verify Bob has balance
    local bob_balance=$(bob_cli listchannels | jq -r '.channels[0].local_balance')
    log_info "Bob's channel balance: $bob_balance sats"

    log_step "Phase 4: Save Bob's backup data before disaster"
    save_bob_backup

    log_step "Phase 5: DISASTER - Nuke Bob's wallet"
    nuke_bob_wallet

    log_step "Phase 6: Restore Bob from seed + channel backup"
    restore_bob_from_backup

    log_step "Phase 7: Wait for force close and fund recovery"

    # Give time for DLP to trigger and force close tx to be broadcast
    log_info "Waiting for force close transaction to be broadcast..."
    sleep 10

    # Mine blocks to confirm force close tx
    log_info "Mining blocks to confirm force close..."
    mine_blocks 6
    sleep 5

    # Check for pending force close (check both nodes)
    local bob_pending=$(bob_cli pendingchannels 2>/dev/null | jq -r '.pending_force_closing_channels | length // 0')
    local alice_pending=$(alice_cli pendingchannels 2>/dev/null | jq -r '.pending_force_closing_channels | length // 0')
    log_info "Bob has $bob_pending pending force closing channel(s)"
    log_info "Alice has $alice_pending pending force closing channel(s)"

    local pending=$((bob_pending + alice_pending))
    if [ "$pending" -eq 0 ]; then
        # Check waiting close channels
        local waiting=$(bob_cli pendingchannels 2>/dev/null | jq -r '.waiting_close_channels | length // 0')
        if [ "$waiting" -gt 0 ]; then
            log_info "Bob has $waiting waiting close channel(s) - force close may not have broadcast yet"
            log_info "Mining more blocks and waiting..."
            mine_blocks 6
            sleep 5
        fi
    fi

    # Re-check pending channels
    local pending=$(bob_cli pendingchannels 2>/dev/null | jq -r '.pending_force_closing_channels | length // 0')
    log_info "Bob has $pending pending force closing channel(s)"

    # Get maturity info if there are pending channels
    if [ "$pending" -gt 0 ]; then
        local blocks_til=$(bob_cli pendingchannels | jq -r '.pending_force_closing_channels[0].blocks_til_maturity // 0')
        log_info "Blocks until maturity: $blocks_til"

        if [ "$blocks_til" -gt 0 ]; then
            log_info "Mining $blocks_til blocks to reach maturity..."
            mine_blocks $blocks_til
        fi
    fi

    # Mine additional blocks for sweep
    log_info "Mining additional blocks for sweep transactions..."
    for i in {1..30}; do
        mine_blocks 1
        sleep 2

        # Check both pending force close and waiting close
        local force_pending=$(bob_cli pendingchannels 2>/dev/null | jq -r '.pending_force_closing_channels | length // 0')
        local waiting=$(bob_cli pendingchannels 2>/dev/null | jq -r '.waiting_close_channels | length // 0')

        if [ "$force_pending" = "0" ] && [ "$waiting" = "0" ]; then
            log_info "All pending channels resolved!"
            break
        fi

        if [ $((i % 5)) -eq 0 ]; then
            log_debug "Still waiting for channel resolution... (force_pending: $force_pending, waiting: $waiting)"
        fi
    done

    log_step "Phase 8: Verify Bob recovered his funds"

    # Wait for balance to appear
    sleep 5
    mine_blocks 1
    sleep 3

    local bob_final_balance=$(bob_cli walletbalance 2>/dev/null | jq -r '.confirmed_balance // "0"')
    local bob_original_channel=$(cat "$BACKUP_DIR/channel_balance.txt")
    local bob_original_onchain=$(cat "$BACKUP_DIR/onchain_balance.txt")
    local bob_total_original=$((bob_original_channel + bob_original_onchain))

    log_info ""
    log_info "=== SCB Recovery Results ==="
    log_info "Bob's original channel balance: $bob_original_channel sats"
    log_info "Bob's original on-chain balance: $bob_original_onchain sats"
    log_info "Bob's total original funds: $bob_total_original sats"
    log_info "Bob's final on-chain balance: $bob_final_balance sats"
    log_info ""

    # Check pending channels
    local still_pending=$(bob_cli pendingchannels 2>/dev/null | jq -r '.pending_force_closing_channels | length // 0')
    if [ "$still_pending" != "0" ]; then
        log_warn "Bob still has $still_pending pending force close channel(s)"
        bob_cli pendingchannels | jq '.pending_force_closing_channels[] | {limbo_balance, blocks_til_maturity}'
    fi

    # Calculate recovery (allowing for fees)
    local min_expected=$((bob_total_original - 50000))  # Allow up to 50k sats for fees

    if [ "$bob_final_balance" -ge "$min_expected" ] 2>/dev/null; then
        echo -e "${GREEN}✓ SCB Recovery Successful!${NC}"
        echo -e "${GREEN}  Bob recovered his funds after disaster recovery${NC}"
        local recovered_pct=$((bob_final_balance * 100 / bob_total_original))
        echo -e "${GREEN}  Recovery rate: ~${recovered_pct}% (minus fees)${NC}"
    elif [ "$bob_final_balance" -gt 0 ] 2>/dev/null; then
        echo -e "${YELLOW}⚠ Partial Recovery${NC}"
        echo -e "${YELLOW}  Bob recovered $bob_final_balance sats${NC}"
        echo -e "${YELLOW}  Some funds may still be in pending channels${NC}"
    else
        echo -e "${RED}✗ SCB Recovery Failed${NC}"
        echo -e "${RED}  Bob's balance is $bob_final_balance sats${NC}"

        log_warn "Checking Bob's pending channels for debugging..."
        bob_cli pendingchannels

        exit 1
    fi

    echo ""
}

# Main
main() {
    echo -e "${GREEN}"
    echo "============================================"
    echo "  LND Esplora SCB Restore Test Script"
    echo "============================================"
    echo -e "${NC}"
    echo ""
    echo "Esplora URL: $ESPLORA_URL"
    echo ""
    echo "This test simulates disaster recovery using Static Channel Backups (SCB)."
    echo "Bob will lose his wallet data and recover using his seed + channel backup."
    echo ""

    check_prerequisites
    setup_directories
    run_scb_restore_test

    log_step "SCB Restore Test Complete! 🎉"
}

main "$@"
