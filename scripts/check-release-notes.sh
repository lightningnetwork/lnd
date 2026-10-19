#!/bin/bash

set -e

# Extract the PR number which is stored in the $GITHUB_REF env variable. The
# format of the string stored in the variable is: refs/pull/:PRNUMBER/merge.
PR_NUMBER=$(echo $GITHUB_REF | awk 'BEGIN { FS = "/" } ; { print $3 }')

# If this is a PR being merged into the main repo, then the PR number will
# actually be "master" here, so we'll ignore this case, and assume that in
# order for it to be merged it had to pass the check in its base branch. If
# we're trying to merge this with the Merge Queue, then the PR number will be
# "gh-readonly-queue" instead.
if [[ $PR_NUMBER == "master" ]] || [[ $PR_NUMBER == "gh-readonly-queue" ]]; then
    exit 0
fi

# Ensure that the PR is described by a release note fragment in
# docs/release-notes/unreleased, as documented in docs/release-notes/README.md.
# This is usually the PR's own fragment, but a PR may also extend the fragment
# of an earlier PR. Entries in the version-wide release note documents don't
# count, since those are only written when a release is prepared, by PRs that
# carry the no-changelog label. The PR number must not be followed by another
# digit, so that e.g. PR 1132 isn't satisfied by a link to PR 11326. If the
# PR's own fragment exists, the fragment checks below report a missing link.
NOTES_DIR=docs/release-notes
PR_LINK="lightningnetwork/lnd/pull/${PR_NUMBER}([^0-9]|$)"
if [[ ! -f "$NOTES_DIR/unreleased/$PR_NUMBER.md" ]] &&
    ! grep -r -q -E "$PR_LINK" "$NOTES_DIR/unreleased"; then
    echo "PR $PR_NUMBER didn't add a release note fragment"
    if grep -r -q -E "$PR_LINK" "$NOTES_DIR"; then
        echo "Release notes are no longer added to the version-wide documents,"
        echo "move the entry to a fragment"
    fi
    echo "Add $NOTES_DIR/unreleased/$PR_NUMBER.md"
    echo "See $NOTES_DIR/README.md for details"
    exit 1
fi

# The fragment checks below compare the PR against its base. On pull requests,
# the checked out commit is the merge commit, so its first parent is the base
# branch. RELEASE_NOTES_BASE allows overriding the base for local runs.
BASE=${RELEASE_NOTES_BASE:-HEAD^1}
if ! git rev-parse -q --verify "$BASE^{commit}" > /dev/null; then
    echo "Base commit $BASE not found, fetch at least two commits of history"
    exit 1
fi

# Print every heading of the given files as its path, i.e. "# Section" for
# top-level headings and "# Section > ## Subsection" for nested ones. Headings
# inside fenced code blocks are ignored.
heading_paths() {
    awk '
        { sub(/\r$/, "") }
        FNR == 1 { top = ""; fenced = 0 }
        /^```/ { fenced = !fenced; next }
        fenced { next }
        /^# / { top = $0; print top; next }
        /^#+ / { print top " > " $0 }
    ' "$@"
}

# Fragments may use the headings of the template and of the newest
# version-wide release note document, except for the document title and the
# generated contributors section.
TEMPLATE="$NOTES_DIR/release-notes-template.md"
NEWEST=$(ls "$NOTES_DIR"/release-notes-[0-9]*.md | sort -V | tail -n 1)
ALLOWED=$(heading_paths "$TEMPLATE" "$NEWEST" |
    grep -v -x -F '# Release Notes' | grep -v -i 'contributors')

FAILED=0
fail() {
    echo "$1: $2"
    FAILED=1
}

# The version-wide release note documents are only written when a release is
# prepared, by PRs that carry the no-changelog label and therefore skip this
# check. Any other PR touching them would keep conflicting with other PRs and
# could announce its change twice, once from its fragment.
while IFS= read -r document; do
    if [[ -n "$document" ]]; then
        fail "$document" "version-wide release notes are only edited when a \
release is prepared, move the entry to a fragment"
    fi
done <<< "$(git diff --name-only --no-renames "$BASE" HEAD -- \
    "$NOTES_DIR/release-notes-[0-9]*.md")"

# Validate every file that the PR adds or modifies in a subdirectory of the
# release notes. A new fragment must be named after the PR and link to it.
# Modifying an existing fragment, e.g. to extend the entry of an earlier PR, is
# allowed.
ADDED=$(git diff --name-only --no-renames --diff-filter=A "$BASE" HEAD -- \
    "$NOTES_DIR/*/*")
while IFS= read -r fragment; do
    if [[ -z "$fragment" ]]; then
        continue
    fi

    if [[ $(dirname "$fragment") != "$NOTES_DIR/unreleased" ]]; then
        fail "$fragment" "fragments must be added to $NOTES_DIR/unreleased"
        continue
    fi

    if grep -q -x -F -- "$fragment" <<< "$ADDED"; then
        if [[ $(basename "$fragment") != "$PR_NUMBER.md" ]]; then
            fail "$fragment" "fragment must be named $PR_NUMBER.md"
        fi

        if ! grep -q -E "$PR_LINK" "$fragment"; then
            fail "$fragment" "fragment must link to this PR (pull/$PR_NUMBER)"
        fi
    fi

    first_line=$(grep -v -m 1 -E '^[[:space:]]*$' "$fragment" || true)
    if [[ ! "$first_line" =~ ^#\  ]]; then
        fail "$fragment" "fragment must start with a top-level heading"
    fi

    while IFS= read -r heading; do
        if [[ -z "$heading" ]]; then
            continue
        fi

        if [[ "$heading" == *[Cc]ontributors* ]]; then
            fail "$fragment" "fragment must not contain a contributors section"
        elif ! grep -q -x -F -- "$heading" <<< "$ALLOWED"; then
            fail "$fragment" "unknown heading \"$heading\", see $TEMPLATE"
        fi
    done <<< "$(heading_paths "$fragment")"
done <<< "$(git diff --name-only --no-renames --diff-filter=AM "$BASE" HEAD -- \
    "$NOTES_DIR/*/*")"

if [[ $FAILED -ne 0 ]]; then
    echo "See $NOTES_DIR/README.md for the fragment format"
    exit 1
fi
