#!/usr/bin/env bash
# Build the Lean model of the range reply logic, check every proof, and run
# the differential test that compares the model with the Go code.
#
# Steps:
#   1. build the library, which checks every theorem, and the executable;
#   2. refuse any sorry, admit or unexpected axiom;
#   3. run TestLeanDiffAccumulator and TestLeanDiffChunker in the parent
#      package against the freshly built executable.
#
# Environment:
#   CHECKS   rapid checks per differential test (default 100000)
#   CLEAN=1  delete the build first, to time a build from scratch

set -euo pipefail

LEAN_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG_DIR="$(dirname "$LEAN_DIR")"
CHECKS="${CHECKS:-100000}"

export PATH="${HOME}/.elan/bin:${PATH}"
if ! command -v lake >/dev/null 2>&1; then
	echo "lake not found: install elan from https://github.com/leanprover/elan"
	exit 1
fi

cd "$LEAN_DIR"
if [[ "${CLEAN:-0}" == "1" ]]; then
	rm -rf .lake/build
fi

echo "=== build: $(lean --version)"
start=$(date +%s)
lake build 2>&1 | tail -1
echo "built in $(($(date +%s) - start))s"

echo "=== proofs: no sorry, no admit, standard axioms only"
if grep -rnwE 'sorry|admit' --include='*.lean' GossipSync GossipSync.lean Main.lean; then
	echo "ERROR: unfinished proof"
	exit 1
fi
axioms="$(lake env lean scripts/Axioms.lean)"
echo "$axioms" | sed 's/^/  /'
if echo "$axioms" | grep -vE "depends on axioms: \[(propext|Classical\.choice|Quot\.sound|, )*\]$|does not depend on any axioms$" | grep -q .; then
	echo "ERROR: a theorem depends on a nonstandard axiom"
	exit 1
fi

echo "=== differential test: ${CHECKS} checks each"
cd "$PKG_DIR"
GOSSIPSYNC_LEAN_BIN="${LEAN_DIR}/.lake/build/bin/gsync-lean" \
	go test -count=1 -v -run 'TestLeanDiff' . -rapid.checks="${CHECKS}" |
	grep -E "rapid|^(ok|FAIL|--- )"
