#!/usr/bin/env bash
# Check the gossip sync P models, record fresh bridge traces, and replay them
# against the Go implementation.
#
# Steps:
#   1. compile the P project;
#   2. run every green test case, each of which must find zero bugs;
#   3. run every counterexample and finding test case, each of which must
#      find the bug it exists to catch, so a clean run there fails the
#      script;
#   4. record seeded executions of the production cases as traces;
#   5. replay the fresh traces, and the checked-in ones, into the Go manager
#      and syncer with the bridge tests, and run the reference model test.
#
# Environment:
#   SCHEDULES      schedules per green test case (default 2000)
#   MAX_STEPS      step bound per schedule (default 3000)
#   TRACE_SEED     seed of the recorded runs (default 1)
#   TRACE_RUNS     schedules recorded per bridged case (default 40)
#   KEEP_TRACES=1  write the recorded traces into traces/ for check-in

set -euo pipefail

MODEL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PKG_DIR="$(dirname "$MODEL_DIR")"
DLL="${MODEL_DIR}/PGenerated/PChecker/net8.0/GossipSyncModels.dll"

SCHEDULES="${SCHEDULES:-2000}"
MAX_STEPS="${MAX_STEPS:-3000}"
TRACE_SEED="${TRACE_SEED:-1}"
TRACE_RUNS="${TRACE_RUNS:-40}"

# Green cases hold every monitor they attach.
GREEN=(
	tcManagerProduction
	tcManagerOnePeer
	tcManagerLegacyTick
	tcManagerSinglePeerLegacyTick
	tcManagerLiveness
	tcManagerNoTickLiveness
	tcManagerPinnedOnly
	tcSyncerHonestPrompt
	tcSyncerHonestLossy
	tcSyncerVerySlowPeer
	tcSyncerByzantine
	tcSyncerLegacySlow
	tcSyncerLegacyPrompt
)

# Counterexamples each remove one rule, and must find the bug it prevents.
# Findings run the production profile under an assumption the design does
# not meet, and must find the bug that documents the limit. Each case names
# the error it must report, so a run that fails for another reason, such as
# a runtime error in the model, does not count.
MUST_FAIL=(
	"tcManagerNoSettleCounterexample:(a) unsynced"
	"tcManagerNoSettleStarvesCounterexample:GraphEventuallySynced detected"
	"tcManagerNoLocalBackoffCounterexample:(h) attempt"
	"tcManagerNoSessionCheckCounterexample:differ from the contract's"
	"tcSyncerNoDrainingCounterexample:credited a reply to query"
	"tcSyncerNoFirstReplyCheckCounterexample:completed its range phase on"
	"tcSyncerLegacyDrainCounterexample:credited a reply to query"
	"tcSyncerFixedDrainDeadlineCounterexample:credited a reply to query"
	"tcManagerNoPinnedRetryCounterexample:GraphEventuallySyncedWithPinned"
	"tcSyncerCrossCreditBeyondDrainFinding:credited a reply to query"
)

# Only cases that run the production algorithm are bridged: a variant
# profile's traces describe different behavior than the Go code's. The
# syncer finding runs the production algorithm but is not bridged: its
# assertion fires in the middle of a transition, so the last step's recorded
# outbox is partial.
BRIDGED=(
	tcManagerProduction
	tcManagerOnePeer
	tcManagerLiveness
	tcManagerNoTickLiveness
	tcManagerPinnedOnly
	tcSyncerHonestPrompt
	tcSyncerHonestLossy
	tcSyncerVerySlowPeer
	tcSyncerByzantine
	tcSyncerLegacySlow
	tcSyncerLegacyPrompt
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
p compile -pp gossipsync.pproj >/dev/null

for tc in "${GREEN[@]}"; do
	echo "=== green: ${tc} (expect 0 bugs)"
	p check "$DLL" -tc "$tc" -s "$SCHEDULES" --max-steps "$MAX_STEPS" \
		| grep -E "Found [0-9]+ bug"
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
# lines through the checker's verbose log. A finding stops at its first bug,
# so p check exits non-zero for it.
OUT_DIR="$(mktemp -d)"
trap 'rm -rf "$OUT_DIR" "${MODEL_DIR}/PCheckerOutput"' EXIT
for tc in "${BRIDGED[@]}"; do
	prefix=syncer
	tag=STRACE
	if [[ "$tc" == tcManager* ]]; then
		prefix=manager
		tag=MTRACE
	fi

	{ p check "$DLL" -tc "$tc" -s "$TRACE_RUNS" \
		--max-steps "$MAX_STEPS" --seed "$TRACE_SEED" -v || true; } \
		| sed -n "s/^<PrintLog> ${tag} //p" \
		> "${OUT_DIR}/${prefix}_${tc}.trace"
done

if [[ "${KEEP_TRACES:-0}" == "1" ]]; then
	rm -f "${MODEL_DIR}"/traces/*.trace
	cp "${OUT_DIR}"/*.trace "${MODEL_DIR}/traces/"
fi

echo "=== bridge: replaying traces into the Go manager and syncer"
cd "$PKG_DIR"
TESTS='TestPModelManagerBridge|TestPModelSyncerBridge|TestRefModelManager'
GOSSIPSYNC_PMODEL_TRACES="$OUT_DIR" go test -count=1 -v -run "$TESTS" . |
	grep -E "replayed|^(ok|FAIL|--- )"

