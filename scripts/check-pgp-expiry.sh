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

for key_file in "${key_files[@]}"; do
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

        key_info="$key_name $type:$id ($capabilities)"

        # Filter out keys that cannot sign releases.
        if ! [[ "$capabilities" =~ [sS] ]]; then
            continue
        fi

        # If expiry is empty, the key does not expire.
        if [[ -z "$expiry" ]]; then
            echo "INFO: $key_info does not expire"
            valid_sign_key_found=true
            continue
        fi

        # Convert expiry timestamp to human readable date for logging.
        if ! expiry_date=$(date -d "@$expiry" "+%Y-%m-%d" 2>/dev/null \
            || date -r "$expiry" "+%Y-%m-%d" 2>/dev/null); then
            error "Invalid expiry timestamp for $key_info $expiry"
            continue
        fi

        if (( expiry < $(date +%s) )); then
            echo "WARN: $key_info has already expired ($expiry_date)"
            continue
        fi

        if (( expiry < EXPIRE_THRESHOLD )); then
            echo "WARN: $key_info expires soon ($expiry_date)"
            continue
        fi

        valid_sign_key_found=true
        echo "INFO: $key_info is valid until $expiry_date"
    done <<< "$gpg_output"

    if [[ "$valid_sign_key_found" == false ]]; then
        error "$key_name does not have any valid sign key"
    fi
done

exit $exit_code
