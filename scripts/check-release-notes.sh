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

# Ensure that the PR number at least shows up in the release notes folder.
# New entries are PR-specific fragments in docs/release-notes/unreleased, as
# documented in docs/release-notes/README.md. The recursive check keeps the
# version-wide files of older release lines compatible. The PR number must not
# be followed by another digit, so that e.g. PR 1132 isn't satisfied by a link
# to PR 11326.
NOTES_DIR=docs/release-notes
PR_LINK="lightningnetwork/lnd/pull/${PR_NUMBER}([^0-9]|$)"
if ! grep -r -q -E "$PR_LINK" "$NOTES_DIR"; then
    echo "PR $PR_NUMBER didn't update the release notes"
    echo "Add $NOTES_DIR/unreleased/$PR_NUMBER.md"
    echo "See $NOTES_DIR/README.md for details"
    exit 1
fi
