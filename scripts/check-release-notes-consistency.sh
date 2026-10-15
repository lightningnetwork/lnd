#!/bin/bash

set -e

# This script keeps the release note fragments in docs/release-notes/unreleased
# consistent with the version-wide release note documents, as described in
# docs/release-notes/README.md. Unlike check-release-notes.sh, it must not be
# skipped by the no-changelog label, since the pull requests that fold
# fragments into a release note document or bump the version carry that label.
#
# Usage: check-release-notes-consistency.sh [pr|release]
#
#   pr       Run on pull requests. Fails if a fragment is already announced in
#            a version-wide release note document. If the pull request changes
#            the version to a release version, the release notes of that
#            version must be ready as well.
#   release  Run before building a release. The release notes of the version
#            in build/version.go must be ready.
MODE=${1:-pr}

NOTES_DIR=docs/release-notes
UNRELEASED_DIR="$NOTES_DIR/unreleased"
VERSION_FILE=build/version.go

FAILED=0
error() {
    echo "::error::$*"
    FAILED=1
}

# Print the release note fragments, one per line.
fragments() {
    if [[ -d "$UNRELEASED_DIR" ]]; then
        find "$UNRELEASED_DIR" -maxdepth 1 -type f -name '*.md' | sort
    fi
}

# Print the value of the given constant in build/version.go.
version_const() {
    sed -n -E "s/^[[:space:]]*$1( uint)? = \"?([^\"]*)\"?$/\2/p" \
        "$VERSION_FILE"
}

# Every fragment belongs to a change that hasn't been released yet. Once a
# release note document announces the PR, its fragment must be deleted, either
# in the PR that folds the fragments into the document or in the PR that ports
# a patch release's notes to master.
check_released_fragments() {
    local fragment pr announced

    while IFS= read -r fragment; do
        if [[ -z "$fragment" ]]; then
            continue
        fi

        pr=$(basename "$fragment" .md)
        if [[ ! $pr =~ ^[0-9]+$ ]]; then
            continue
        fi

        announced=$(grep -l -E "lightningnetwork/lnd/pull/${pr}([^0-9]|$)" \
            "$NOTES_DIR"/release-notes-[0-9]*.md || true)
        if [[ -n "$announced" ]]; then
            error "$fragment: PR $pr is already announced in" \
                "$(echo $announced), delete the fragment"
        fi
    done <<< "$(fragments)"
}

# Check that the release notes of the version in build/version.go are ready:
# the document must exist and all fragments must be folded into it, for
# release candidates as well as final releases. A final release also needs a
# contributor list.
check_release_ready() {
    local major minor patch pre_release version notes remaining contributors

    major=$(version_const AppMajor)
    minor=$(version_const AppMinor)
    patch=$((10#$(version_const AppPatch)))
    pre_release=$(version_const AppPreRelease)

    if [[ $patch -eq 99 ]]; then
        error "$VERSION_FILE contains the development version" \
            "$major.$minor.$patch, not a release version"
        return
    fi

    version="$major.$minor.$patch"
    notes="$NOTES_DIR/release-notes-$version.md"
    echo "Checking release notes for $version-$pre_release"

    if [[ ! -f "$notes" ]]; then
        error "$notes does not exist, create it from the template and" \
            "fold the fragments in $UNRELEASED_DIR into it"
        return
    fi

    remaining=$(fragments | wc -l | tr -d ' ')
    if [[ $remaining -ne 0 ]]; then
        error "$remaining fragment(s) in $UNRELEASED_DIR must be folded" \
            "into $notes before the release"
    fi

    if [[ $pre_release != *rc* ]]; then
        contributors=$(awk '
            /^# / { in_section = /^# Contributors/; next }
            in_section && /^[*-] / { count++ }
            END { print count + 0 }
        ' "$notes")
        if [[ $contributors -eq 0 ]]; then
            error "$notes has an empty contributors section"
        fi
    fi
}

case $MODE in
pr)
    check_released_fragments

    # Release branches keep their release version between releases, so the
    # release notes are only checked by the pull request that changes it. On
    # pull requests, the checked out commit is the merge commit, so its first
    # parent is the base branch. RELEASE_NOTES_BASE allows overriding the base
    # for local runs.
    base=${RELEASE_NOTES_BASE:-HEAD^1}
    if ! git rev-parse -q --verify "$base^{commit}" > /dev/null; then
        error "base commit $base not found, fetch at least two commits of" \
            "history"
    elif ! git diff --quiet "$base" HEAD -- "$VERSION_FILE"; then
        if [[ $((10#$(version_const AppPatch))) -ne 99 ]]; then
            check_release_ready
        fi
    fi
    ;;

release)
    check_release_ready
    ;;

*)
    echo "Usage: $0 [pr|release]"
    exit 1
    ;;
esac

if [[ $FAILED -ne 0 ]]; then
    echo "See $NOTES_DIR/README.md for the release note process"
    exit 1
fi
