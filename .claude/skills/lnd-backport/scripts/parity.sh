#!/usr/bin/env bash
# Compare a rebuilt backport against its parent PR, commit by commit.
#
# Usage: parity.sh <commits-file> <target-base-ref> [backport-head]
#
#   commits-file     output of parent_commits.sh (oldest first)
#   target-base-ref  e.g. origin/v0.21.x-branch
#   backport-head    defaults to HEAD
#
# Prints:
#   1. A markdown table: #, parent sha, backport sha, subject, SAME/DIFF,
#      parent shortstat, backport shortstat.
#   2. For each DIFF commit, the added/removed lines that differ from the
#      parent (context-free), so each difference can be explained.
#   3. The aggregate changed-line difference between the whole parent PR and
#      the whole backport, plus both diffstats.
#
# The table's "Difference" column has to be written by hand from section 2.
# The script reports what differs; you explain why the branch forced it.
set -euo pipefail

commits=${1:?commits file}
base=${2:?target base ref}
head=${3:-HEAD}

# bash 3.2 (macOS) has no mapfile.
parents=()
while read -r sha _; do parents+=("$sha"); done <"$commits"
backs=()
while read -r sha; do backs+=("$sha"); done < <(git rev-list --reverse "$base..$head")

if [[ ${#parents[@]} -ne ${#backs[@]} ]]; then
	echo "error: ${#parents[@]} parent commits but ${#backs[@]}" \
		"backport commits on $base..$head" >&2
	exit 1
fi

changed() {
	git show --format= -U0 "$1" | grep -E '^[-+]' |
		grep -vE '^(\+\+\+|---) ' || true
}
stat() {
	git show --shortstat --format= "$1" | sed 's/^ *//; s/ changed//;
		s/ insertions\{0,1\}(+)/+/; s/ deletions\{0,1\}(-)/-/' |
		tr -d '\n'
}

echo "| # | Parent | Backport | Subject | Patch | Parent stat | Backport stat |"
echo "|---|--------|----------|---------|-------|-------------|---------------|"
diffs=()
for i in "${!parents[@]}"; do
	p=${parents[$i]}
	b=${backs[$i]}
	pid=$(git show "$p" | git patch-id --stable | cut -d' ' -f1)
	bid=$(git show "$b" | git patch-id --stable | cut -d' ' -f1)
	same=DIFF
	[[ "$pid" == "$bid" ]] && same=SAME
	[[ "$same" == DIFF ]] && diffs+=("$i")
	printf '| %d | %s | %s | %s | %s | %s | %s |\n' "$((i + 1))" \
		"${p:0:10}" "${b:0:9}" "$(git log -1 --format=%s "$b")" \
		"$same" "$(stat "$p")" "$(stat "$b")"
done

echo
for i in ${diffs[@]+"${diffs[@]}"}; do
	p=${parents[$i]}
	b=${backs[$i]}
	echo "### $((i + 1)) ${p:0:10} vs ${b:0:9} (< parent only, > backport only)"
	d=$(diff <(changed "$p") <(changed "$b") || true)
	if [[ -z "$d" ]]; then
		echo "(same changed lines; only hunk context differs)"
	else
		echo "$d"
	fi
	echo
done

first=$(git rev-parse "${parents[0]}^")
last=${parents[$((${#parents[@]} - 1))]}
echo "### Aggregate: parent ${first:0:10}..${last:0:10} vs $base..$head"
d=$(diff <(git diff -U0 "$first" "$last" | grep -E '^[-+]' |
	grep -vE '^(\+\+\+|---) ' | sort) \
	<(git diff -U0 "$base" "$head" | grep -E '^[-+]' |
	grep -vE '^(\+\+\+|---) ' | sort) || true)
echo "${d:-(identical changed lines)}"
echo "parent:   $(git diff --shortstat "$first" "$last")"
echo "backport: $(git diff --shortstat "$base" "$head")"
