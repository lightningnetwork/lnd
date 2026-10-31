#!/bin/bash
if [[ "$1" = "" ]]; then
	echo "USAGE: $0 remote/head_branch"
	echo "eg $0 upstream/master"
	exit 1
fi

set -e
set -x

if [[ "$(git log --pretty="%H %D" | grep "^[0-9a-f]*.* $1")" = "" ]]; then
	echo "It seems like the current checked-out commit is not based on $1"
	exit 1
fi

# Keep build cache ephemeral for this run to avoid long-term disk growth.
# On GitHub runners we put it under RUNNER_TEMP rather than /tmp: newer
# runner images mount /tmp as a small tmpfs, which a cold build of every
# package overflows. GOTMPDIR moves the compiler's work directory along with
# it for the same reason.
TMP_BASE="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
TMP_GOCACHE=$(mktemp -d "$TMP_BASE/lnd-check-commit-gocache.XXXXXX")
TMP_GOTMPDIR=$(mktemp -d "$TMP_BASE/lnd-check-commit-gotmp.XXXXXX")
cleanup() {
	rm -rf "$TMP_GOCACHE" "$TMP_GOTMPDIR"
}
trap cleanup EXIT
export GOCACHE="$TMP_GOCACHE"
export GOTMPDIR="$TMP_GOTMPDIR"

git rebase --exec scripts/check-commit.sh $1
