#!/bin/bash

set -e

function run_fuzz() {
  PACKAGES=$1
  RUN_TIME=$2
  NUM_WORKERS=$3

  for pkg in $PACKAGES; do
    pushd $pkg

    go test -list=Fuzz.* | grep Fuzz | while read line; do
        echo ----- Fuzz testing $pkg:$line for $2 with $3 workers -----
        go test -fuzz=^$line\$ -fuzztime=$2 -parallel=$3
    done

    popd
  done
}

# usage prints the usage of the whole script.
function usage() {
  echo "Usage: "
  echo "fuzz.sh run <packages> <run_time>"
}

# Extract the sub command and remove it from the list of parameters by shifting
# them to the left.
SUBCOMMAND=$1
shift

# Call the function corresponding to the specified sub command or print the
# usage if the sub command was not found.
case $SUBCOMMAND in
run)
  echo "Running fuzzer"
  run_fuzz "$@"
  ;;
*)
  usage
  exit 1
  ;;
esac
