#!/bin/bash

# Validates PGP keys in the scripts/keys directory by checking their expiration
# status and signing capabilities. It iterates through all .asc files and uses
# GPG to parse key information.

set -euo pipefail
shopt -s nullglob

error() {
    RED='\033[0;31m'
    NC='\033[0m' # No Color
    echo -e "${RED}ERROR: $1${NC}"
    exit_code=1
}

# Check if a key has expired or is expiring soon
# Args: $1 = expiry timestamp, $2 = key_info
# Returns: 0 if key is valid or has no expiry, 1 if expired/expiring soon
check_key_expiry() {
    local expiry="$1"
    local key_info="$2"

    # If expiry is empty, the key does not expire.
    if [[ -z "$expiry" ]]; then
        echo "INFO: $key_info does not expire"
        return 0
    fi

    # Convert expiry timestamp to human readable date for logging.
    local expiry_date
    if ! expiry_date=$(date -d "@$expiry" "+%Y-%m-%d" 2>/dev/null \
        || date -r "$expiry" "+%Y-%m-%d" 2>/dev/null); then
        error "Invalid expiry timestamp for $key_info $expiry"
        return 1
    fi

    if (( expiry < $(date +%s) )); then
        echo "WARN: $key_info has already expired ($expiry_date)"
        return 1
    fi

    if (( expiry < EXPIRE_THRESHOLD )); then
        echo "WARN: $key_info expires soon ($expiry_date)"
        return 1
    fi

    echo "INFO: $key_info is valid until $expiry_date"
    return 0
}

echo
echo "Starting PGP key validation..."

KEY_DIR="./scripts/keys"
if [[ ! -d "$KEY_DIR" ]]; then
    error "Directory $KEY_DIR does not exist"
    exit $exit_code
fi

key_files=("$KEY_DIR"/*.asc)
if (( ${#key_files[@]} == 0 )); then
    error "No PGP keys found in $KEY_DIR"
    exit $exit_code
fi

# 2 weeks = 14 days * (24 hours * 60 minutes * 60 seconds).
EXPIRE_THRESHOLD=$(($(date +%s) + 14 * 86400))
exit_code=0

echo "Found ${#key_files[@]} key file(s) in $KEY_DIR"
echo

for key_file in "${key_files[@]}"; do
    echo "────────────────────────────────────────────────────────────────────"
    echo "Checking $(basename "$key_file")..."

    gpg_output=$(gpg --with-colons --import-options show-only \
      --import "$key_file" 2>&1 | grep -E '^(pub|sub):')

    key_name=$(basename "$key_file")

    # Parse GPG output line by line to find key type, id, expiry and
    # capabilities.
    valid_sign_key_found=false
    while IFS=: read -r type _ _ _ id _ expiry _ _ _ _ capabilities _; do
        if [[ "$valid_sign_key_found" == true ]]; then
            # If we already found a valid signing key, skip further checks for
            # this keychain.
            break
        fi

        key_info="$type:$id ($capabilities)"

        # Check primary key expiry.
        if [[ "$type" == "pub" ]] &&
            ! check_key_expiry "$expiry" "$key_info"; then
            error "$key_info primary key is invalid"
            break
        fi

        # Filter out keys that cannot sign releases.
        if ! [[ "$capabilities" =~ [sS] ]]; then
            continue
        fi

        # Check sub key expiry.
        if [[ "$type" == "sub" ]] &&
            ! check_key_expiry "$expiry" "$key_info"; then
            continue
        fi

        # If we reach here, we have a valid signing key.
        valid_sign_key_found=true
    done <<< "$gpg_output"

    if [[ "$valid_sign_key_found" == false ]]; then
        error "$key_name does not have any valid sign key"
    fi
done

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
if [[ $exit_code -eq 0 ]]; then
    echo "All PGP keys are valid and ready for use!"
else
    error "Some PGP keys have issues that need attention."
fi

exit $exit_code
