#!/bin/bash

# Verify the quorum-signed OCI digest for an lnd release image, then pull the
# image by that immutable digest. This script must be run from a trusted lnd
# checkout; running a verifier contained in the image would make the image its
# own trust anchor.

set -euo pipefail

REPO=lightningnetwork
PROJECT=lnd
DOCKER_IMAGE=lightninglabs/lnd
API_URL=https://api.github.com/repos/$REPO/$PROJECT/releases/tags
# Match verify-install.sh: require three independently controlled release keys.
MIN_REQUIRED_SIGNATURES=3

# Keep this list in sync with scripts/verify-install.sh. The key files are
# imported into separate keyrings so each release signer can only be counted
# once.
KEYS=()
KEYS+=("A5B61896952D9FDA83BC054CDC42612E89237182 roasbeef")
KEYS+=("9FC6B0BFD597A94DBF09708280E5375C094198D8 bhandras")
KEYS+=("4DC235556B18694E08518DBB671103D881A5F0E4 sputn1ck")
KEYS+=("E85497D2DBA0EB9ADB0024279BCD95C4FF296868 yyforyongyu")
KEYS+=("32F7EA1E7A0339F7D37164B9F82D456EA023C9BF hieblmi")
KEYS+=("3E9BD4436C288039CA827A9200C9E2BC2E45666F suheb")
KEYS+=("5F75437E11695F86D50C11BB1AFF9C4DCED6D666 ziggie1984")
KEYS+=("C20A78516A0944900EBFCA29961CC8259AE675D4 ViktorT-11")
KEYS+=("1583B601BB57CC7CD2DF8A87E08DEA9B12B66AF6 georgetsagk")
KEYS+=("7530B54D5E45A68760E68926019A44857735FD20 gijswijs")
KEYS+=("BCEE34B0F9CD832214CE53005EA98470361ACB4F boris")

usage() {
  echo "Usage: verify-docker-image.sh [--no-pull] version"
}

fail() {
  echo "ERROR: $*" >&2
  exit 1
}

log() {
  echo "$*" >&2
}

check_command() {
  command -v "$1" >/dev/null 2>&1 || fail "$1 is not installed or not in PATH"
}

PULL_IMAGE=true
if [[ ${1:-} == "--no-pull" ]]; then
  PULL_IMAGE=false
  shift
fi

if [[ $# -ne 1 ]]; then
  usage
  exit 1
fi

VERSION=$1
VERSION_REGEX='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$'
if [[ ! $VERSION =~ $VERSION_REGEX ]]; then
  fail "invalid release version: $VERSION"
fi

check_command gpg
check_command jq
check_command wget
if [[ $PULL_IMAGE == true ]]; then
  check_command docker
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
KEY_DIR="$SCRIPT_DIR/keys"
[[ -d $KEY_DIR ]] || fail "release key directory not found: $KEY_DIR"

TEMP_DIR=$(mktemp -d /tmp/lnd-docker-verification-XXXXXX)
trap 'rm -rf "$TEMP_DIR"' EXIT

log "Downloading release metadata for $VERSION"
RELEASE_JSON="$TEMP_DIR/release.json"
wget -q --header="Accept: application/vnd.github+json" \
  -O "$RELEASE_JSON" "$API_URL/$VERSION" || \
  fail "could not download release metadata for $VERSION"

RELEASE_TAG=$(jq -er '.tag_name' "$RELEASE_JSON")
[[ $RELEASE_TAG == "$VERSION" ]] || \
  fail "release tag $RELEASE_TAG does not match expected version $VERSION"

MANIFEST_NAME="docker-image-digest-$VERSION.txt"
MANIFEST_COUNT=$(jq --arg name "$MANIFEST_NAME" \
  '[.assets[] | select(.name == $name)] | length' "$RELEASE_JSON")
[[ $MANIFEST_COUNT -eq 1 ]] || \
  fail "expected one $MANIFEST_NAME release asset, found $MANIFEST_COUNT"

MANIFEST_URL=$(jq -er --arg name "$MANIFEST_NAME" \
  '.assets[] | select(.name == $name) | .browser_download_url' \
  "$RELEASE_JSON")
MANIFEST_FILE="$TEMP_DIR/$MANIFEST_NAME"
wget -q -O "$MANIFEST_FILE" "$MANIFEST_URL" || \
  fail "could not download $MANIFEST_NAME"

NUM_CHECKS=0
for key in "${KEYS[@]}"; do
  KEY_ID=${key%% *}
  USERNAME=${key#* }
  KEY_FILE="$KEY_DIR/$USERNAME.asc"
  [[ -f $KEY_FILE ]] || fail "release key not found: $KEY_FILE"

  SIGNATURE_NAME="docker-image-digest-$USERNAME-$VERSION.sig"
  SIGNATURE_COUNT=$(jq --arg name "$SIGNATURE_NAME" \
    '[.assets[] | select(.name == $name)] | length' "$RELEASE_JSON")
  if [[ $SIGNATURE_COUNT -eq 0 ]]; then
    continue
  fi
  [[ $SIGNATURE_COUNT -eq 1 ]] || \
    fail "found multiple $SIGNATURE_NAME release assets"

  SIGNATURE_URL=$(jq -er --arg name "$SIGNATURE_NAME" \
    '.assets[] | select(.name == $name) | .browser_download_url' \
    "$RELEASE_JSON")
  SIGNATURE_FILE="$TEMP_DIR/$SIGNATURE_NAME"
  wget -q -O "$SIGNATURE_FILE" "$SIGNATURE_URL" || \
    fail "could not download $SIGNATURE_NAME"

  UNTRUSTED_KEYRING="$USERNAME.pgp-untrusted"
  TRUSTED_KEYRING="$USERNAME.pgp"
  gpg --homedir "$TEMP_DIR" --batch --no-default-keyring \
    --keyring "$UNTRUSTED_KEYRING" --import "$KEY_FILE" >/dev/null 2>&1
  gpg --homedir "$TEMP_DIR" --batch --no-default-keyring \
    --keyring "$UNTRUSTED_KEYRING" --export "$KEY_ID" |
    gpg --homedir "$TEMP_DIR" --batch --no-default-keyring \
      --keyring "$TRUSTED_KEYRING" --import >/dev/null 2>&1

  STATUS_FILE="$TEMP_DIR/$USERNAME.status"
  if ! gpg --homedir "$TEMP_DIR" --batch --no-default-keyring \
    --keyring "$TRUSTED_KEYRING" --status-fd=1 \
    --verify "$SIGNATURE_FILE" "$MANIFEST_FILE" \
    >"$STATUS_FILE" 2>&1; then
    sed 's/^/  /' "$STATUS_FILE" >&2
    fail "invalid signature from $USERNAME"
  fi
  grep -q '^\[GNUPG:\] VALIDSIG ' "$STATUS_FILE" || \
    fail "gpg did not report a valid signature from $USERNAME"

  log "Verified Docker manifest signature from $USERNAME"
  NUM_CHECKS=$((NUM_CHECKS + 1))
done

if [[ $NUM_CHECKS -lt $MIN_REQUIRED_SIGNATURES ]]; then
  fail "found $NUM_CHECKS valid signatures; $MIN_REQUIRED_SIGNATURES required"
fi

LINE_COUNT=$(awk 'END { print NR }' "$MANIFEST_FILE")
[[ $LINE_COUNT -eq 1 ]] || fail "$MANIFEST_NAME must contain exactly one line"

IMAGE_REF=$(sed -n '1p' "$MANIFEST_FILE")
EXPECTED_PREFIX="$DOCKER_IMAGE:$VERSION@sha256:"
[[ $IMAGE_REF == "$EXPECTED_PREFIX"* ]] || \
  fail "manifest does not identify $DOCKER_IMAGE:$VERSION by digest"

DIGEST=${IMAGE_REF##*@}
if [[ ! $DIGEST =~ ^sha256:[0-9a-f]{64}$ ]]; then
  fail "invalid OCI digest in $MANIFEST_NAME: $DIGEST"
fi

log "Verified $IMAGE_REF with $NUM_CHECKS developer signatures"
if [[ $PULL_IMAGE == true ]]; then
  log "Pulling the verified image by immutable digest"
  docker pull "$IMAGE_REF" >&2
fi

echo "$IMAGE_REF"
