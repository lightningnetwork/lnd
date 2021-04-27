#!/bin/bash

set -e

function build_fuzz() {
  PACKAGES=$1

  for pkg in $PACKAGES; do
    pushd fuzz/$pkg

    for file in *.go; do
      if [[ "$file" == "fuzz_utils.go" ]]; then
        continue
      fi

      NAME=$(echo $file | sed 's/\.go$//1')
      echo "Building zip file for $pkg/$NAME"
      go-fuzz-build -func "Fuzz_$NAME" -o "$pkg-$NAME-fuzz.zip" "github.com/lightningnetwork/lnd/fuzz/$pkg"
    done

    popd
  done
}

# timeout is a cross platform alternative to the GNU timeout command that
# unfortunately isn't available on macOS by default.
timeout() {
    time=$1
    $2 &
    pid=$!
    sleep $time
    kill -s SIGINT $pid    
}

function run_fuzz() {
  PACKAGES=$1
  RUN_TIME=$2
  TIMEOUT=$3
  PROCS=$4
  BASE_WORKDIR=$5

  for pkg in $PACKAGES; do
    pushd fuzz/$pkg

    for file in *.go; do
      if [[ "$file" == "fuzz_utils.go" ]]; then
        continue
      fi

      NAME=$(echo $file | sed 's/\.go$//1')
      WORKDIR=$BASE_WORKDIR/$pkg/$NAME
      mkdir -p $WORKDIR
      echo "Running fuzzer $pkg-$NAME-fuzz.zip with $PROCS processors for $RUN_TIME seconds"
      COMMAND="go-fuzz -bin=$pkg-$NAME-fuzz.zip -workdir=$WORKDIR -procs=$PROCS -timeout=$TIMEOUT"
      echo "$COMMAND"
      timeout "$RUN_TIME" "$COMMAND"
    done

    popd
  done
}

# usage prints the usage of the whole script.
function usage() {
  echo "Usage: "
  echo "fuzz.sh build <packages>"
  echo "fuzz.sh run <packages> <run_time> <timeout>"
}

# Extract the sub command and remove it from the list of parameters by shifting
# them to the left.
SUBCOMMAND=$1
shift

# Call the function corresponding to the specified sub command or print the
# usage if the sub command was not found.
case $SUBCOMMAND in
build)
  echo "Building fuzz packages"
  build_fuzz "$@"
  ;;
run)
  echo "Running fuzzer"
  run_fuzz "$@"
  ;;
*)
  usage
  exit 1
  ;;
esac
