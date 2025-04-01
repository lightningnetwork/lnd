#!/bin/bash

# Stop the script if an error is returned by any step.
set -e

# DIR is set to the directory of this script.
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "$DIR/compose.sh"
source "$DIR/network.sh"
source "$DIR/.env"

cd $DIR

# Spin up the network in detached mode.
compose_up

# Ensure that the cluster is shut down when the script exits
# regardless of success
trap compose_down EXIT

# Set up the network.
setup_network

# Print the initial version of each node.
do_for print_version alice bob charlie dave

# Test that Bob can send a multi-hop payment.
send_payment bob dave

# Test that Bob can receive a multi-hop payment.
send_payment dave bob

# Test that Bob can route a payment.
send_payment alice dave

# Upgrade the compose variables so that the Bob configuration
# is swapped out for the PR version.
upgrade_node bob

# Wait for Bob to start.
wait_for_node bob
wait_for_active_chans bob 2

# Show that Bob is now running the current branch.
do_for print_version bob

# Repeat the basic tests.
send_payment bob dave
send_payment dave bob
send_payment alice dave

# Upgrade the compose variables so that the Dave configuration
# is swapped out for the PR version.
upgrade_node dave

wait_for_node dave
wait_for_active_chans dave 1

# Show that Dave is now running the current branch.
do_for print_version dave

# Repeat the basic tests (after potential migraton).
send_payment bob dave
send_payment dave bob
send_payment alice dave

echo "🛡️⚔️🫡 Backwards compatibility test passed! 🫡⚔️🛡️"
