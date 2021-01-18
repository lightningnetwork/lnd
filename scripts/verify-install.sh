#!/bin/bash

REPO=lightningnetwork
PROJECT=lnd

RELEASE_URL=https://github.com/$REPO/$PROJECT/releases
API_URL=https://api.github.com/repos/$REPO/$PROJECT/releases
SIGNATURE_SELECTOR=". | select(.name | test(\"manifest-.*(\\\\.txt\\\\.asc)$\")) | .name"
HEADER_JSON="Accept: application/json"
HEADER_GH_JSON="Accept: application/vnd.github.v3+json"

# All keys that can sign lnd releases. The key must be downloadable/importable
# from the URL given after the space.
KEYS=()
KEYS+=("F4FC70F07310028424EFC20A8E4256593F177720 https://keybase.io/guggero/pgp_keys.asc")
KEYS+=("15E7ECF257098A4EF91655EB4CA7FE54A6213C91 https://keybase.io/carlakirkcohen/pgp_keys.asc")
KEYS+=("9C8D61868A7C492003B2744EE7D737B67FA592C7 https://keybase.io/bitconner/pgp_keys.asc")
KEYS+=("E4D85299674B2D31FAA1892E372CBD7633C61696 https://keybase.io/roasbeef/pgp_keys.asc")
KEYS+=("729E9D9D92C75A5FBFEEE057B5DD717BEF7CA5B1 https://keybase.io/wpaulino/pgp_keys.asc")
KEYS+=("7E81EF6B9989A9CC93884803118759E83439A9B1 https://keybase.io/eugene_/pgp_keys.asc")

function check_command() {
  echo -n "Checking if $1 is installed... "
  if ! command -v "$1"; then
    echo "ERROR: $1 is not installed or not in PATH!"
    exit 1
  fi
}

check_command curl
check_command jq
check_command gpg
check_command lnd
check_command lncli

LND_BIN=$(which lnd)
LNCLI_BIN=$(which lncli)

LND_VERSION=$(lnd --version | cut -d'=' -f2)
LNCLI_VERSION=$(lncli --version | cut -d'=' -f2)
LND_SUM=$(sha256sum $LND_BIN | cut -d' ' -f1)
LNCLI_SUM=$(sha256sum $LNCLI_BIN | cut -d' ' -f1)

echo "Detected lnd version $LND_VERSION with SHA256 sum $LND_SUM"
echo "Detected lncli version $LNCLI_VERSION with SHA256 sum $LNCLI_SUM"

# Make sure lnd and lncli are installed with the same version.
if [[ "$LNCLI_VERSION" != "$LND_VERSION" ]]; then
  echo "ERROR: Version $LNCLI_VERSION of lncli does not match $LND_VERSION of lnd!"
  exit 1
fi

# If we're inside the docker image, there should be a shasums.txt file in the
# root directory. If that's the case, we first want to make sure we still have
# the same hash as we did when building the image.
if [[ -f /shasums.txt ]]; then
  if ! grep -q "$LND_SUM" /shasums.txt; then
    echo "ERROR: Hash $LND_SUM for lnd not found in /shasums.txt: "
    cat /shasums.txt
    exit 1
  fi
  if ! grep -q "$LNCLI_SUM" /shasums.txt; then
    echo "ERROR: Hash $LNCLI_SUM for lnd not found in /shasums.txt: "
    cat /shasums.txt
    exit 1
  fi
fi

# Import all the signing keys.
for key in "${KEYS[@]}"; do
  KEY_ID=$(echo $key | cut -d' ' -f1)
  IMPORT_URL=$(echo $key | cut -d' ' -f2)
  echo "Downloading and importing key $KEY_ID from $IMPORT_URL"
  curl -L -s $IMPORT_URL | gpg --import

  # Make sure we actually imported the correct key.
  if ! gpg --list-key "$KEY_ID"; then
    echo "ERROR: Imported key from $IMPORT_URL doesn't match ID $KEY_ID."
  fi
done

echo ""

# Download the JSON of the release itself. That'll contain the release ID we need for the next call.
RELEASE_JSON=$(curl -L -s -H "$HEADER_JSON" "$RELEASE_URL/$LND_VERSION")

TAG_NAME=$(echo $RELEASE_JSON | jq -r '.tag_name')
RELEASE_ID=$(echo $RELEASE_JSON | jq -r '.id')
echo "Release $TAG_NAME found with ID $RELEASE_ID"

# Now download the asset list and filter by manifests and signatures.
ASSETS=$(curl -L -s -H "$HEADER_GH_JSON" "$API_URL/$RELEASE_ID" | jq -c '.assets[]')
SIGNATURES=$(echo $ASSETS | jq -r "$SIGNATURE_SELECTOR")

# Download all "manifest-*.txt.asc" as those contain both the hashes that were
# signed and the signature itself (=detached sig).
TEMP_DIR=$(mktemp -d /tmp/lnd-sig-verification-XXXXXX)
for signature in $SIGNATURES; do
  echo "Downloading $signature"
  curl -L -s -o "$TEMP_DIR/$signature" "$RELEASE_URL/download/$LND_VERSION/$signature"
done

echo ""
cd $TEMP_DIR || exit 1

NUM_CHECKS=0
for signature in $SIGNATURES; do
  # First make sure the downloaded signature file is valid.
  echo "Verifying $signature"
  if ! gpg --verify "$signature" 2>&1 | grep -q "Good signature"; then
    echo "ERROR: Did not get valid signature for $signature!"
    exit 1
  fi

  echo "Signature for $signature checks out: "
  gpg --verify "$signature" 2>&1 | grep "using"

  echo ""

  # Then make sure that the hash of the installed binaries can be found in the
  # signed list of hashes.
  if ! grep -q "$LND_SUM" "$signature"; then
    echo "ERROR: Hash $LND_SUM for lnd not found in $signature: "
    cat "$signature"
    exit 1
  fi

  if ! grep -q "$LNCLI_SUM" "$signature"; then
    echo "ERROR: Hash $LNCLI_SUM for lncli not found in $signature: "
    cat "$signature"
    exit 1
  fi

  echo "Verified lnd and lncli hashes against $signature"
  ((NUM_CHECKS=NUM_CHECKS+1))
done

# We want at least one signature that signs the hashes of the binaries we have
# installed. If we arrive here without exiting, it means no signature manifests
# were uploaded (yet) with the correct naming pattern.
if [[ $NUM_CHECKS -lt 1 ]]; then
  echo "ERROR: No valid signatures found!"
  echo "Make sure the release $LND_VERSION contains any signed manifests."
  exit 1
fi

echo ""
echo "SUCCESS! Verified lnd and lncli against $NUM_CHECKS signature(s)."
