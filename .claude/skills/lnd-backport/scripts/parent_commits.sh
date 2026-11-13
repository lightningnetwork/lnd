#!/usr/bin/env bash
# Print the parent PR's commits, oldest first, as "<full-sha> <subject>".
#
# Usage: parent_commits.sh <parent-pr-number> [repo]
#
# Uses the merge commit's second-parent range when the PR was merged with a
# merge commit, and falls back to the PR's commit list otherwise (squash or
# rebase merges, or an unmerged PR). It also checks the commit count against
# the PR so a range that picked up extra commits gets caught.
set -euo pipefail

pr=${1:?parent PR number}
repo=${2:-lightningnetwork/lnd}

json=$(gh pr view "$pr" --repo "$repo" --json state,mergeCommit,commits)
merge=$(jq -r '.mergeCommit.oid // empty' <<<"$json")
count=$(jq '.commits | length' <<<"$json")

if [[ -n "$merge" ]]; then
	git fetch -q origin "$merge" 2>/dev/null || true
	parents=$(git rev-list --parents -n1 "$merge" | wc -w | tr -d ' ')
	if [[ "$parents" -eq 3 ]]; then
		out=$(git log --reverse --format='%H %s' "$merge^1..$merge^2")
		n=$(wc -l <<<"$out" | tr -d ' ')
		if [[ "$n" -eq "$count" ]]; then
			echo "$out"
			exit 0
		fi
		echo "warn: merge range has $n commits, PR lists $count;" \
			"using PR commit list" >&2
	fi
fi

# Fallback: the PR's own commit list. Make sure every object exists locally.
git fetch -q origin "pull/$pr/head" 2>/dev/null || true
jq -r '.commits[] | "\(.oid) \(.messageHeadline)"' <<<"$json"
