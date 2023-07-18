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

# Ensure that the PR number at least shows up in the release notes folder under
# one of the contained milestones.
if ! grep -r -q "lightningnetwork/lnd/pull/$PR_NUMBER" docs/release-notes; then
    echo "PR $PR_NUMBER didn't update release notes"
    exit 1
fi
