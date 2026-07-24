#!/usr/bin/env bash
# Refresh the command-center dashboard from the latest GEPA run artifacts
# and republish to Litbucket. Deterministic part of the periodic refresh;
# a scheduled agent runs this and layers content edits on top.
#
# Usage: refresh_dashboard.sh <run-name> <scratch-dir>
set -euo pipefail

RUN_NAME="${1:?usage: refresh_dashboard.sh <run-name> <scratch-dir>}"
SCRATCH="${2:?usage: refresh_dashboard.sh <run-name> <scratch-dir>}"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
SITE="$REPO/simulation/command-center"

# Export the latest run lineage into the site's data dir (local serve at
# :8777 picks this up on reload automatically).
python3 "$REPO/simulation/export_run.py" \
    --run-dir "$SCRATCH/runs/$RUN_NAME" \
    --output-dir "$SCRATCH/outputs/$RUN_NAME" \
    --out "$SITE/data/run.json" \
    --run-id "$RUN_NAME"

# Bundle and publish a new version to Litbucket.
BUNDLE="$(mktemp -d)/command-center.zip"
(cd "$SITE" && zip -qr "$BUNDLE" index.html findings.html style.css app.js data/)

export LITBUCKET_ENDPOINT="${LITBUCKET_ENDPOINT:-https://litbucket-api.staging.lightningcluster.com}"
litbucket --json publish "$BUNDLE" \
    --name "LN Routing Evolution Command Center" \
    --slug lnd-routing-command-center \
    --team labs \
    --description "GEPA-driven evolution of lnd pathfinding: the findings write-up (mainnet validation, paradigm-over-parameters, anatomy of the evolved routers) plus live run telemetry, candidate lineage and the corpus explorer."

rm -f "$BUNDLE"
