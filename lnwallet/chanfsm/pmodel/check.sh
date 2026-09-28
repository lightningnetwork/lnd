#!/usr/bin/env bash
# Check the channel P models, record fresh bridge traces, and replay them
# against the Go state machine.
#
# Steps:
#   1. compile the P project;
#   2. run every green test case, each of which must find zero bugs;
#   3. run every counterexample, each of which must find the bug it exists
#      to catch, so a clean run there fails the script;
#   4. record seeded executions of the production cases as traces;
#   5. replay the fresh traces into the Go state machine with the bridge
#      test;
#   6. check that ../SPEC.md cites the current model, with valid citations.
#
# Environment:
#   SCHEDULES      schedules per test case (default 2000)
#   MAX_STEPS      step bound per schedule (default 5000)
#   TRACE_SEED     seed of the recorded runs (default 1)
#   TRACE_RUNS     schedules recorded per bridged case (default 200)
#   KEPT_RUNS      schedules per case KEEP_TRACES=1 commits (default 25)
#   KEEP_TRACES=1  write the recorded traces into traces/ for check-in

set -euo pipefail

MODEL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG_DIR="$(dirname "$MODEL_DIR")"
DLL="${MODEL_DIR}/PGenerated/PChecker/net8.0/ChanFSMModels.dll"

SCHEDULES="${SCHEDULES:-2000}"
MAX_STEPS="${MAX_STEPS:-5000}"
TRACE_SEED="${TRACE_SEED:-1}"
TRACE_RUNS="${TRACE_RUNS:-200}"
KEPT_RUNS="${KEPT_RUNS:-25}"

# Green cases hold every monitor they attach.
GREEN=(
	tcHonest
	tcReconnect
	tcByzantine
	tcPrematureRevocation
)

# Counterexamples each remove one rule, and must find the bug it prevents.
# Each names the error it must report, so a run that fails for another
# reason, such as a runtime error in the model, does not count.
MUST_FAIL=(
	"tcLegacyRevocationCounterexample:changed its state refusing a revocation"
	"tcForwardOnCommitCounterexample:before it was irrevocably committed"
	"tcNoFreshnessCounterexample:forwarded peer update"
	"tcEarlyLocalRemovalCounterexample:removed HTLC"
	"tcSignAllRemoteCounterexample:for a commitment it doesn't build"
	"tcNoResendCounterexample:EventuallyLockedInAndForwarded detected liveness bug"
	"tcRevokeFirstCounterexample:failed the channel in an honest run"
	"tcUnpersistedPeerAckedCounterexample:failed the channel in an honest run"
)

# Only cases that run the production node are bridged.
BRIDGED=(
	tcHonest
	tcReconnect
	tcByzantine
	tcPrematureRevocation
)

if ! command -v p >/dev/null 2>&1; then
	echo "P not found: dotnet tool install --global P --version 3.0.4"
	exit 1
fi
P_VERSION="$(p --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' |
	head -1)"
echo "P ${P_VERSION}, schedules ${SCHEDULES}, max steps ${MAX_STEPS}," \
	"trace seed ${TRACE_SEED}"

cd "$MODEL_DIR"
rm -rf PGenerated PCheckerOutput
p compile -pp chanfsm.pproj >/dev/null

for tc in "${GREEN[@]}"; do
	echo "=== green: ${tc} (expect 0 bugs)"
	out="$(p check "$DLL" -tc "$tc" -s "$SCHEDULES" \
		--max-steps "$MAX_STEPS")"
	echo "$out" | grep -E "Found [0-9]+ bug"
	if ! echo "$out" | grep -q "Found 0 bugs"; then
		echo "ERROR: ${tc} found a bug"
		exit 1
	fi
done

for entry in "${MUST_FAIL[@]}"; do
	tc="${entry%%:*}"
	want="${entry#*:}"
	echo "=== must fail: ${tc} (expect: ${want})"
	if out="$(p check "$DLL" -tc "$tc" -s "$SCHEDULES" \
		--max-steps "$MAX_STEPS" -v)"; then

		echo "ERROR: ${tc} found no bug, but a bug was expected"
		exit 1
	fi

	err="$(echo "$out" | grep -m1 -E "<ErrorLog>" || true)"
	echo "$err"
	if [[ "$err" != *"$want"* ]]; then
		echo "ERROR: ${tc} failed, but not with the expected bug"
		exit 1
	fi
done

# Record a seeded run of each bridged case. Every schedule prints its trace
# lines through the checker's verbose log.
OUT_DIR="$(mktemp -d)"
trap 'rm -rf "$OUT_DIR" "${MODEL_DIR}/PCheckerOutput"' EXIT
record() {
	local tc="$1" runs="$2" out="$3"
	{ p check "$DLL" -tc "$tc" -s "$runs" \
		--max-steps "$MAX_STEPS" --seed "$TRACE_SEED" -v || true; } \
		| sed -n "s/^<PrintLog> CTRACE //p" > "$out"
}
for tc in "${BRIDGED[@]}"; do
	record "$tc" "$TRACE_RUNS" "${OUT_DIR}/channel_${tc}.trace"
done

# The committed traces are a smaller sample, which keeps the repository
# lean while plain go test still replays every bridged case.
if [[ "${KEEP_TRACES:-0}" == "1" ]]; then
	rm -f "${MODEL_DIR}"/traces/*.trace
	for tc in "${BRIDGED[@]}"; do
		record "$tc" "$KEPT_RUNS" \
			"${MODEL_DIR}/traces/channel_${tc}.trace"
	done
fi

echo "=== bridge: replaying traces into the Go state machine"
cd "$PKG_DIR"
CHANFSM_PMODEL_TRACES="$OUT_DIR" go test -count=1 -v \
	-run 'TestPModelBridge' . | grep -E "replayed|^(ok|FAIL|--- )"

echo "=== spec: validating ../SPEC.md against the model"
python3 "${MODEL_DIR}/scripts/extract_p_model.py" "$MODEL_DIR" \
	--output "${OUT_DIR}/inventory.json"
python3 "${MODEL_DIR}/scripts/validate_spec.py" --model-dir "$MODEL_DIR" \
	--inventory "${OUT_DIR}/inventory.json" --spec "${PKG_DIR}/SPEC.md"
