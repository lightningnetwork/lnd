#!/bin/bash

# Let's work with absolute paths only, we run in the itest directory itself.
WORKDIR=$(pwd)/itest

TRANCHE=$1
NUM_TRANCHES=$2

# Shift the passed parameters by two, giving us all remaining testing flags in
# the $@ special variable.
shift
shift

# Windows insists on having the .exe suffix for an executable, we need to add
# that here if necessary.
EXEC="$WORKDIR"/itest.test"$EXEC_SUFFIX"
LND_EXEC="$WORKDIR"/lnd-itest"$EXEC_SUFFIX"
BTCD_EXEC="$WORKDIR"/btcd-itest"$EXEC_SUFFIX"
echo $EXEC -test.v "$@" -logoutput -logdir=.logs-tranche$TRANCHE -lndexec=$LND_EXEC -btcdexec=$BTCD_EXEC -splittranches=$NUM_TRANCHES -runtranche=$TRANCHE

# Exit code 255 causes the parallel jobs to abort, so if one part fails the
# other is aborted too.
cd "$WORKDIR" || exit 255
$EXEC -test.v "$@" -logoutput -logdir=.logs-tranche$TRANCHE -lndexec=$LND_EXEC -btcdexec=$BTCD_EXEC -splittranches=$NUM_TRANCHES -runtranche=$TRANCHE || exit 255
