#!/usr/bin/env bash
# Regression test for bump_go.py: replay past Go bump commits and require the
# script to reproduce them byte for byte.
#
# Usage: replay_history.sh [<commit>...]
#
# For each commit, a scratch worktree is checked out at its parent, bump_go.py
# is run with the commit's target toolchain (Makefile GO_VERSION) and minimum
# (root go.mod), and the result is diffed against the real commit. Run it after
# changing RULES in bump_go.py.
#
# The default commits are every Go bump since the pins became consistent:
# toolchain and minimum together, a major toolchain bump (1.26 -> 1.27, which
# also changes the major-only setup-go comment), and a minimum-only bump. Older
# bumps (before df48bd51c6) carried pins that had drifted and were fixed by
# hand, so they don't replay byte for byte.
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/bump_go.py"
commits=("$@")
if [[ ${#commits[@]} -eq 0 ]]; then
	commits=(df48bd51c6 b1f9ca66d d67c9edf8 aab1e4661)
fi

wt=$(mktemp -d)
trap 'git worktree remove --force "$wt" >/dev/null 2>&1 || true' EXIT
git worktree add -q --detach "$wt" "${commits[0]}^"

fail=0
for c in "${commits[@]}"; do
	(cd "$wt" && git checkout -q -f --detach "$c^" && git clean -qfd)
	tn=$(git show "$c:Makefile" | sed -n 's/^GO_VERSION = //p')
	to=$(git show "$c^:Makefile" | sed -n 's/^GO_VERSION = //p')
	mn=$(git show "$c:go.mod" | sed -n 's/^go //p')
	mo=$(git show "$c^:go.mod" | sed -n 's/^go //p')
	args=()
	[[ "$tn" != "$to" ]] && args+=(--toolchain "$tn")
	[[ "$mn" != "$mo" ]] && args+=(--min "$mn")

	if ! (cd "$wt" && python3 -I "$script" set "${args[@]}" >/dev/null); then
		echo "FAIL $c: bump_go.py set ${args[*]} failed"
		fail=1
		continue
	fi
	# Compare only files the bump touches; the real commit may also carry
	# unrelated edits.
	files=$( (cd "$wt" && git diff --name-only; git diff --name-only "$c^" "$c") |
		sort -u)
	if (cd "$wt" && git diff --quiet "$c" -- $files); then
		echo "ok   $c: ${args[*]} reproduces $(echo "$files" | wc -l | tr -d ' ') files"
	else
		echo "FAIL $c: ${args[*]} differs from the real commit:"
		(cd "$wt" && git diff --stat "$c" -- $files)
		fail=1
	fi
done
exit $fail
