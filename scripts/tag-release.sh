#!/bin/bash
#
# tag-release.sh creates a signed annotated git tag for an lnd release after
# verifying (a) HEAD is in sync with the upstream lightningnetwork/lnd
# branch, and (b) build/version.go at HEAD matches the requested tag. Guards
# against tagging a commit that has not been merged upstream yet, or one
# whose embedded version disagrees with the tag.

set -euo pipefail

VERSION_FILE="build/version.go"

# Match the canonical upstream URL across https / git@ / ssh:// forms, with or
# without a `.git` suffix. We identify the remote by URL because `origin` is
# conventionally the fork in a `gh repo fork` setup. The URL is lower-cased
# before matching (see below), so this pattern stays lower-case: GitHub treats
# the org/repo as case-insensitive, and `origin` is often the mixed-case
# `LightningNetwork/lnd`.
UPSTREAM_URL_REGEX='[:/]lightningnetwork/lnd(\.git)?$'

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") <tag-name> [--branch <branch>]

  <tag-name>          Release tag, e.g. v0.21.0-beta.rc3. Must match the
                      constants defined in ${VERSION_FILE} at HEAD.
  --branch <branch>   Upstream branch to verify HEAD against. Defaults to
                      the currently checked-out branch (typically a release
                      branch such as v0.21.x-branch).
EOF
  exit 1
}

TAG=""
UPSTREAM_BRANCH=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)   usage ;;
    --branch)    [[ $# -ge 2 ]] || usage; UPSTREAM_BRANCH="$2"; shift 2 ;;
    --branch=*)  UPSTREAM_BRANCH="${1#--branch=}"; shift ;;
    -*)          echo "Unknown flag: $1" >&2; usage ;;
    *)           [[ -z "${TAG}" ]] || usage; TAG="$1"; shift ;;
  esac
done
[[ -n "${TAG}" ]] || usage

cd "$(git rev-parse --show-toplevel)"

if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  echo "Error: tag ${TAG} already exists locally." >&2
  exit 1
fi

if [[ -z "${UPSTREAM_BRANCH}" ]]; then
  UPSTREAM_BRANCH="$(git symbolic-ref --quiet --short HEAD || true)"
  [[ -n "${UPSTREAM_BRANCH}" ]] \
    || { echo "Error: detached HEAD; pass --branch <name>." >&2; exit 1; }
fi

# Discover the upstream remote by URL (see UPSTREAM_URL_REGEX).
UPSTREAM_REMOTES=()
while IFS= read -r line; do
  UPSTREAM_REMOTES+=("$line")
done < <(git remote -v | awk -v re="${UPSTREAM_URL_REGEX}" \
  '$3 == "(fetch)" && tolower($2) ~ re { print $1 }' | sort -u)

case "${#UPSTREAM_REMOTES[@]}" in
  0) echo "Error: no git remote points at lightningnetwork/lnd. Add one with" \
          "'git remote add upstream" \
          "https://github.com/lightningnetwork/lnd.git'." >&2
     exit 1 ;;
  1) UPSTREAM_REMOTE="${UPSTREAM_REMOTES[0]}" ;;
  *) echo "Error: multiple remotes match lightningnetwork/lnd:" >&2
     printf '  %s\n' "${UPSTREAM_REMOTES[@]}" >&2
     exit 1 ;;
esac

# Fetch first so every later check runs against confirmed-current upstream
# state. Without this, a stale local HEAD could pass the version-match check
# while still being out of sync with what's on the release branch.
echo "Fetching ${UPSTREAM_REMOTE} ${UPSTREAM_BRANCH}..."
git fetch --quiet "${UPSTREAM_REMOTE}" "${UPSTREAM_BRANCH}"

# Catch the race where another maintainer has already published this tag.
if git ls-remote --exit-code --tags "${UPSTREAM_REMOTE}" \
    "refs/tags/${TAG}" >/dev/null 2>&1; then
  echo "Error: tag ${TAG} already exists on ${UPSTREAM_REMOTE}." >&2
  exit 1
fi

# Compare against FETCH_HEAD rather than refs/remotes/<remote>/<branch>:
# FETCH_HEAD is always written by `git fetch <remote> <branch>`, while the
# remote-tracking ref depends on the user's refspec configuration.
HEAD_SHA="$(git rev-parse HEAD)"
UP_SHA="$(git rev-parse FETCH_HEAD)"
if [[ "${HEAD_SHA}" != "${UP_SHA}" ]]; then
  AHEAD="$(git rev-list --count FETCH_HEAD..HEAD)"
  BEHIND="$(git rev-list --count HEAD..FETCH_HEAD)"
  cat >&2 <<EOF

Error: HEAD does not match ${UPSTREAM_REMOTE}/${UPSTREAM_BRANCH}.
  HEAD:     ${HEAD_SHA}
  Upstream: ${UP_SHA}
  Ahead: ${AHEAD}, behind: ${BEHIND}.

Push (or wait for) the version bump on ${UPSTREAM_BRANCH}, then re-run.
EOF
  exit 1
fi

# Parse version.go from HEAD (now confirmed in sync with upstream) so the
# check reflects exactly what the tag will commit to. A single awk pass
# extracts all four constants in the order they appear in the file. The
# `|| :` keeps `set -e` from terminating the script if AppPreRelease is
# absent from version.go and the final read hits EOF.
{ read -r M && read -r m && read -r p && read -r pre || :; } < <(
  git show "HEAD:${VERSION_FILE}" 2>/dev/null | awk '
    /^[[:space:]]*AppMajor[[:space:]]+uint[[:space:]]*=/   { sub(/.*=[[:space:]]*/,""); sub(/[^0-9].*/,""); print }
    /^[[:space:]]*AppMinor[[:space:]]+uint[[:space:]]*=/   { sub(/.*=[[:space:]]*/,""); sub(/[^0-9].*/,""); print }
    /^[[:space:]]*AppPatch[[:space:]]+uint[[:space:]]*=/   { sub(/.*=[[:space:]]*/,""); sub(/[^0-9].*/,""); print }
    /^[[:space:]]*AppPreRelease[[:space:]]*=/              { match($0,/"[^"]*"/); print substr($0,RSTART+1,RLENGTH-2) }
  '
)

if [[ -z "${M}" || -z "${m}" || -z "${p}" ]]; then
  echo "Error: failed to parse version constants from HEAD:${VERSION_FILE}." \
       >&2
  exit 1
fi

# Go treats `01` as an octal literal but %d prints it as decimal; force
# base-10 here so we match build.Version()'s output.
EXPECTED="v$((10#$M)).$((10#$m)).$((10#$p))"
[[ -n "${pre}" ]] && EXPECTED="${EXPECTED}-${pre}"

echo "Requested: ${TAG}"
echo "Expected:  ${EXPECTED}  (from HEAD:${VERSION_FILE})"

if [[ "${TAG}" != "${EXPECTED}" ]]; then
  cat >&2 <<EOF

Error: requested tag does not match HEAD:${VERSION_FILE}.
  Requested: ${TAG}
  Expected:  ${EXPECTED}

Bump AppMajor/AppMinor/AppPatch/AppPreRelease and commit before tagging.
EOF
  exit 1
fi

echo "Creating signed tag ${TAG} at ${HEAD_SHA}."
git tag -s -m "${TAG}" "${TAG}"
echo "Done. Push with: git push ${UPSTREAM_REMOTE} ${TAG}"
