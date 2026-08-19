#!/bin/bash

# unit_race_part.sh runs one tranche of the unit tests in race detector
# mode. Packages are distributed over num_tranches tranches using a
# greedy longest-processing-time assignment driven by measured package
# durations, allowing the race detector tests to be spread across
# multiple machines with roughly equal runtimes.

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

# pkg_weight echoes the approximate race-mode test duration of a
# package in seconds, measured from CI logs (the per-package "ok"
# lines of the unit race jobs). Unlisted packages get a small default
# weight. The exact values only matter relative to each other; refresh
# them occasionally if the tranches drift out of balance.
pkg_weight() {
  case "${1#"${PKG_PREFIX}"/}" in
    invoices)                  echo 691 ;;
    lnwire)                    echo 328 ;;
    internal/musig2v040)       echo 246 ;;
    lnwallet/test/neutrino)    echo 171 ;;
    lnwallet/test/bitcoind)    echo 170 ;;
    lnwallet/btcwallet)        echo 103 ;;
    htlcswitch)                echo 84 ;;
    lnwallet/test/btcd)        echo 82 ;;
    routing/chainview)         echo 66 ;;
    watchtower/wtclient)       echo 54 ;;
    chainntnfs/test/bitcoind)  echo 46 ;;
    lnwallet)                  echo 45 ;;
    chainntnfs/test/btcd)      echo 45 ;;
    chainntnfs/test/neutrino)  echo 41 ;;
    contractcourt)             echo 38 ;;
    channeldb/migration30)     echo 26 ;;
    sqldb)                     echo 24 ;;
    chainntnfs)                echo 21 ;;
    discovery)                 echo 21 ;;
    funding)                   echo 17 ;;
    *)                         echo 3 ;;
  esac
}

all_pkgs=()
if [[ -n "${UNIT_RACE_PKGS:-}" ]]; then
  # An explicit package list (relative to PKG) restricts the run, e.g.
  # to the database-touching packages.
  for pkg in ${UNIT_RACE_PKGS}; do
    all_pkgs+=("${PKG_PREFIX}/${pkg}")
  done
else
  # Capture the package list via command substitution so a go list
  # failure aborts the script (set -e -o pipefail) instead of silently
  # yielding an empty list and a vacuously green test run.
  pkg_list=$(go list -tags="${DEV_TAGS}" -deps "${PKG_PREFIX}/..." | \
    grep "${PKG_PREFIX}" | grep -v "/vendor/")

  while IFS= read -r pkg; do
    all_pkgs+=("${pkg}")
  done <<< "${pkg_list}"

  if (( ${#all_pkgs[@]} == 0 )); then
    echo "go list produced no packages" >&2
    exit 1
  fi
fi

# Sort packages by descending weight (ties broken by name for
# determinism across tranche jobs) and greedily assign each to the
# currently lightest tranche. Every tranche job computes the same
# assignment and runs only its own bucket.
weighted=$(for pkg in "${all_pkgs[@]}"; do
  echo "$(pkg_weight "${pkg}") ${pkg}"
done | LC_ALL=C sort -k1,1rn -k2,2)

sums=()
for ((i=0; i<NUM_TRANCHES; i++)); do
  sums[i]=0
done

selected=()
while read -r weight pkg; do
  lightest=0
  for ((i=1; i<NUM_TRANCHES; i++)); do
    if (( sums[i] < sums[lightest] )); then
      lightest=$i
    fi
  done

  sums[lightest]=$(( sums[lightest] + weight ))
  if (( lightest == TRANCHE )); then
    selected+=("${pkg}")
  fi
done <<< "${weighted}"

if (( ${#selected[@]} == 0 )); then
  echo "No packages assigned to tranche ${TRANCHE} of ${NUM_TRANCHES}" >&2
  exit 0
fi

echo "Tranche ${TRANCHE} of ${NUM_TRANCHES}:" \
  "${#selected[@]} packages, estimated weight ${sums[TRANCHE]}s"

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
