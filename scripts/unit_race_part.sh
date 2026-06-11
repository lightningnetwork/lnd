#!/bin/bash

# unit_race_part.sh runs one tranche of the unit tests in race detector
# mode. The full package list is split round-robin into num_tranches
# tranches, allowing the race detector tests to be spread across
# multiple machines.

set -euo pipefail

TRANCHE=${1:-}
NUM_TRANCHES=${2:-}

if [[ -z "${TRANCHE}" || -z "${NUM_TRANCHES}" ]]; then
  echo "Usage: $0 <tranche> <num_tranches> [go test flags...]" >&2
  exit 1
fi

if ! [[ "${TRANCHE}" =~ ^[0-9]+$ && "${NUM_TRANCHES}" =~ ^[0-9]+$ ]]; then
  echo "tranche and num_tranches must be non-negative integers" >&2
  exit 1
fi

if (( NUM_TRANCHES <= 0 )); then
  echo "num_tranches must be greater than 0" >&2
  exit 1
fi

if (( TRANCHE < 0 || TRANCHE >= NUM_TRANCHES )); then
  echo "tranche must be in range [0, num_tranches)" >&2
  exit 1
fi

shift 2

PKG_PREFIX=${PKG:-github.com/lightningnetwork/lnd}
DEV_TAGS=${DEV_TAGS:-dev}

# Heavy packages listed first so the round-robin split distributes them
# across different tranches. Ordered by approximate descending test
# duration. Update periodically if the profile shifts.
HEAVY_PKGS=(
  "${PKG_PREFIX}/lnwallet"
  "${PKG_PREFIX}/htlcswitch"
  "${PKG_PREFIX}/chainntnfs"
  "${PKG_PREFIX}/channeldb"
  "${PKG_PREFIX}/contractcourt"
  "${PKG_PREFIX}/routing"
  "${PKG_PREFIX}/graph/db"
  "${PKG_PREFIX}/invoices"
  "${PKG_PREFIX}/watchtower/wtclient"
  "${PKG_PREFIX}/peer"
)

# Capture the package list via command substitution so a go list
# failure aborts the script (set -e -o pipefail) instead of silently
# yielding an empty list and a vacuously green test run.
pkg_list=$(go list -tags="${DEV_TAGS}" -deps "${PKG_PREFIX}/..." | \
  grep "${PKG_PREFIX}" | grep -v "/vendor/")

all_pkgs=()
while IFS= read -r pkg; do
  all_pkgs+=("${pkg}")
done <<< "${pkg_list}"

if (( ${#all_pkgs[@]} == 0 )); then
  echo "go list produced no packages" >&2
  exit 1
fi

# Only treat heavy packages that actually appear in the package list as
# heavy, so a stale entry above cannot select a nonexistent package.
heavy=()
for pkg in "${HEAVY_PKGS[@]}"; do
  if printf '%s\n' "${all_pkgs[@]}" | grep -qxF "${pkg}"; then
    heavy+=("${pkg}")
  fi
done

remaining=()
while IFS= read -r pkg; do
  remaining+=("${pkg}")
done < <(printf '%s\n' "${all_pkgs[@]}" | \
  grep -vxF "$(printf '%s\n' "${heavy[@]}")")

ordered=("${heavy[@]}" "${remaining[@]}")

selected=()
for i in "${!ordered[@]}"; do
  if (( (i % NUM_TRANCHES) == TRANCHE )); then
    selected+=("${ordered[$i]}")
  fi
done

if (( ${#selected[@]} == 0 )); then
  echo "No packages assigned to tranche ${TRANCHE} of ${NUM_TRANCHES}" >&2
  exit 0
fi

exit_code=0
for pkg in "${selected[@]}"; do
  echo "Running race unit tests for ${pkg}"
  if ! env CGO_ENABLED=1 GORACE="history_size=7 halt_on_errors=1" \
    go test -race "$@" "${pkg}"; then
    exit_code=1
  fi
done

if (( exit_code != 0 )); then
  echo "One or more packages failed in tranche ${TRANCHE} of" \
    "${NUM_TRANCHES}" >&2
fi

exit ${exit_code}
