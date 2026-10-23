#!/bin/bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export SCHEDULES="${SCHEDULES:-20}"
export MAX_STEPS="${MAX_STEPS:-300}"

exec "$script_dir/check.sh"
