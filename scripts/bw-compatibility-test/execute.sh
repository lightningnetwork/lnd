#!/bin/bash

# The execute.sh file can be used to call any helper functions directly
# from the command line. For example:
#   $ ./execute.sh compose-up

# DIR is set to the directory of this script.
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "$DIR/.env"
source "$DIR/compose.sh"
source "$DIR/network.sh"

CMD=$1
shift
$CMD "$@"