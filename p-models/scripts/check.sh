#!/bin/bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
model_dir="$(dirname "$script_dir")"
dll="$model_dir/PGenerated/PChecker/net8.0/TrampolineModels.dll"
schedules="${SCHEDULES:-100}"
max_steps="${MAX_STEPS:-1000}"

"$script_dir/build.sh"

test_cases=(
    tcTrampolineHappy
    tcTrampolineRetry
    tcTrampolineFailure
    tcTrampolineCancelPending
    tcTrampolineRestartTimeout
    tcTrampolineCancelSettles
    tcTrampolineReorgTimeout
    tcTrampolineSenderFlow
    tcTrampolineReadyReplay
)

for test_case in "${test_cases[@]}"; do
    p check "$dll" \
        --testcase "$test_case" \
        --schedules "$schedules" \
        --max-steps "$max_steps"
done
