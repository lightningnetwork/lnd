#!/bin/bash

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
model_dir="$(dirname "$script_dir")"
project="$model_dir/trampoline.pproj"

if ! command -v p >/dev/null 2>&1; then
    echo "P compiler not found. Install it with: dotnet tool install --global P"
    exit 1
fi

cd "$model_dir"
p compile -pp "$project"
