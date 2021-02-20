#!/bin/bash

REPO=lightningnetwork
PROJECT=lnd

RELEASE_URL=https://github.com/$REPO/$PROJECT/releases
API_URL=https://api.github.com/repos/$REPO/$PROJECT/releases
MANIFEST_SELECTOR=". | select(.name | test(\"manifest-v.*(\\\\.txt)$\")) | .name"
SIGNATURE_SELECTOR=". | select(.name | test(\"manifest-.*(\\\\.sig)$\")) | .name"
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
KEYS+=("7AB3D7F5911708842796513415BAADA29DA20D26 https://keybase.io/halseth/pgp_keys.asc")

function check_command() {
  echo -n "Checking if $1 is installed... "
  if ! command -v "$1"; then
    echo "ERROR: $1 is not installed or not in PATH!"
    exit 1
  fi
}

# If exactly three parameters are specified, we expect the first one to be lnd version and
# the following parameters are assets to be verified.
if [[ $# -ge 2 ]]; then
  LND_VERSION="$1"
  # discard this argument
  shift
else
  echo "ERROR: invalid number of parameters!"
  echo "Usage: verify-install.sh lnd-version asset-to-verify [assets-to-verify]"
  exit 1
fi

check_command curl
check_command jq
check_command gpg

# Make this script compatible with both linux and *nix.
SHA_CMD="sha256sum"
if ! command -v "$SHA_CMD"; then
  if command -v "shasum"; then
    SHA_CMD="shasum -a 256"
  else
    echo "ERROR: no SHA256 sum binary installed!"
    exit 1
  fi
fi

version_regex="^v[[:digit:]]+\.[[:digit:]]+\.[[:digit:]]"
if [[ ! "$LND_VERSION" =~ $version_regex ]]; then
  echo "ERROR: Invalid version of lnd specified: $LND_VERSION"
  exit 1
fi

# FIXME: how to fix this?
# If we're inside the docker image, there should be a shasums.txt file in the
# root directory. If that's the case, we first want to make sure we still have
# the same hash as we did when building the image.

# if [[ -f /shasums.txt ]]; then
#   if ! grep -q "$LND_SUM" /shasums.txt; then
#     echo "ERROR: Hash $LND_SUM for lnd not found in /shasums.txt: "
#     cat /shasums.txt
#     exit 1
#   fi
#   if ! grep -q "$LNCLI_SUM" /shasums.txt; then
#     echo "ERROR: Hash $LNCLI_SUM for lnd not found in /shasums.txt: "
#     cat /shasums.txt
#     exit 1
#   fi
# fi

TEMP_DIR=$(mktemp -d /tmp/lnd-sig-verification-XXXXXX)
# The GPG keys will be imported here to not surprisingly pollute GPG keyring of the user
mkdir -p "$TEMP_DIR/all_keys"
# Make GPG shut up about permissions
chmod 700 "$TEMP_DIR/all_keys"
# Import all the signing keys.
for key in "${KEYS[@]}"; do
  KEY_ID=$(echo $key | cut -d' ' -f1)
  IMPORT_URL=$(echo $key | cut -d' ' -f2)
  mkdir -p "$TEMP_DIR/$KEY_ID"
  # Make GPG shut up about permissions
  chmod 700 "$TEMP_DIR/$KEY_ID"
  echo "Downloading and importing key $KEY_ID from $IMPORT_URL"
  curl -L -s $IMPORT_URL | GNUPGHOME="$TEMP_DIR/all_keys" gpg --import

  # Make sure we have the correct key in target directory.
  GNUPGHOME="$TEMP_DIR/all_keys" gpg --export "$KEY_ID" | GNUPGHOME="$TEMP_DIR/$KEY_ID" gpg --import
done
# Defensively remove all_keys
rm -rf "$TEMP_DIR/all_keys"

echo ""

# Download the JSON of the release itself. That'll contain the release ID we need for the next call.
RELEASE_JSON=$(curl -L -s -H "$HEADER_JSON" "$RELEASE_URL/$LND_VERSION")

TAG_NAME=$(echo $RELEASE_JSON | jq -r '.tag_name')
RELEASE_ID=$(echo $RELEASE_JSON | jq -r '.id')
echo "Release $TAG_NAME found with ID $RELEASE_ID"

# Now download the asset list and filter by the manifest and the signatures.
ASSETS=$(curl -L -s -H "$HEADER_GH_JSON" "$API_URL/$RELEASE_ID" | jq -c '.assets[]')
MANIFEST=$(echo $ASSETS | jq -r "$MANIFEST_SELECTOR")
SIGNATURES=$(echo $ASSETS | jq -r "$SIGNATURE_SELECTOR")

# Download the main "manifest-*.txt" and all "manifest-*.sig" files containing
# the detached signatures.
echo "Downloading $MANIFEST"
curl -L -s -o "$TEMP_DIR/$MANIFEST" "$RELEASE_URL/download/$LND_VERSION/$MANIFEST"

for signature in $SIGNATURES; do
  echo "Downloading $signature"
  curl -L -s -o "$TEMP_DIR/$signature" "$RELEASE_URL/download/$LND_VERSION/$signature"
done

echo ""

# Before we even look at the content of the manifest, we first want to make sure
# the signatures actually sign that exact manifest.
NUM_CHECKS=0
for signature in $SIGNATURES; do
  echo "Verifying $signature"
  VALID=0
  for key in "${KEYS[@]}"; do
    KEY_ID=$(echo $key | cut -d' ' -f1)
    if GNUPGHOME="$TEMP_DIR/$KEY_ID" gpg --verify "$TEMP_DIR/$signature" "$TEMP_DIR/$MANIFEST" 2>&1 | grep -q "Good signature"; then
      # We remove the key and signature to NOT count them more than once
      # THIS IS CRUCIAL FOR VERIFICATION TO BE SECURE!!!
      rm -rf "$TEMP_DIR/$KEY_ID" "$signature" || exit 1

      echo "Verified $signature against $MANIFEST using $KEY_ID"
      VALID=1
      break
    elif GNUPGHOME="$TEMP_DIR/$KEY_ID" gpg --verify "$TEMP_DIR/$signature" "$TEMP_DIR/$MANIFEST" 2>&1 | grep -q "No public key"; then
      continue
    else
      echo "ERROR: Did not get valid signature for $MANIFEST in $signature!"
      echo "  The developer signature $signature disagrees on the expected"
      echo "  release binaries in $MANIFEST. The release may have been faulty or"
      echo "  was backdoored."
      echo "  The key used for verification: $KEY_ID"
      exit 1
    fi
  done
  if [ "$VALID" = "1" ];
  then
    ((NUM_CHECKS=NUM_CHECKS+1))
  else
    echo "Unable to verify signature $signature, no key available, skipping"
  fi
done

# We want at least five signatures (out of seven public keys) that sign the
# hashes of the binaries we have installed. If we arrive here without exiting,
# it means no signature manifests were uploaded (yet) with the correct naming
# pattern.
MIN_REQUIRED_SIGNATURES=5
if [[ $NUM_CHECKS -lt $MIN_REQUIRED_SIGNATURES ]]; then
  echo "ERROR: Not enough valid signatures found!"
  echo "  Valid signatures found: $NUM_CHECKS"
  echo "  Valid signatures required: $MIN_REQUIRED_SIGNATURES"
  echo
  echo "  Make sure the release $LND_VERSION contains the required "
  echo "  number of signatures on the manifest, or wait until more "
  echo "  signatures have been added to the release."
  exit 1
fi

VERSION_REGEX="`echo "-$LND_VERSION" | sed 's/\./\\./g'`"

# note that we discarded the version argument
for asset in "$@";
do
  # Then make sure that the hash of the installed binaries can be found in the
  # manifest that we now have verified the signatures for.
  SUM="`$SHA_CMD "$asset" | cut -d' ' -f1`" || exit 1
  if [[ ${#SUM} -ne 64 ]]; then
    echo "ERROR: invalid sha256sum for $asset"
    exit 1
  fi
  if ! grep -q "^$SUM " "$TEMP_DIR/$MANIFEST"; then
    echo "ERROR: Hash $SUM for $asset not found in $MANIFEST: "
    cat "$MANIFEST"
    echo "  The expected release binaries have been verified with the developer "
    echo "  signatures. Your binary's hash does not match the expected release "
    echo "  binary hashes. Make sure you're using an official binary."
    exit 1
  fi
  if ! grep "^$SUM " "$TEMP_DIR/$MANIFEST" | grep -q -- "$VERSION_REGEX"; then
    echo "ERROR: the version doesn't match"
    echo "This may be an attempt at downgrade attack or a bug"
    exit 1
  fi
done

echo ""
echo "SUCCESS! Verified lnd and lncli against $MANIFEST signed by $NUM_CHECKS developers."
