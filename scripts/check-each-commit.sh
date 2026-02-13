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
TMP_GOCACHE=$(mktemp -d -t lnd-check-commit-gocache.XXXXXX)
cleanup() {
	rm -rf "$TMP_GOCACHE"
}
trap cleanup EXIT
export GOCACHE="$TMP_GOCACHE"

git rebase --exec scripts/check-commit.sh $1
