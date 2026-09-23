#!/usr/bin/env bash
# Check the gossip sync TLA+ specs exhaustively with TLC.
#
# Steps:
#   1. translate the PlusCal in SyncerPairing.tla, rewriting its TLA+
#      translation in place, so an edited algorithm is never checked stale;
#   2. run every green case, each of which must find no error;
#   3. run every must-fail case, each of which must fail with the property
#      it exists to break, so a clean run, or a failure of another kind such
#      as a TLC error in the spec, fails the script;
#   4. print a summary of states, distinct states and time per case.
#
# Environment:
#   JAVA        Java 11 or later (default: Homebrew's openjdk@17, then java)
#   TLA2TOOLS   path to tla2tools.jar (default ~/tools/tla2tools.jar)
#   WORKERS     TLC worker threads (default auto)
#   CASES       only run the cases whose name matches this regex

set -euo pipefail

SPEC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TLA2TOOLS="${TLA2TOOLS:-${HOME}/tools/tla2tools.jar}"
WORKERS="${WORKERS:-auto}"
CASES="${CASES:-.}"

if [[ -z "${JAVA:-}" ]]; then
	JAVA=java
	if [[ -x /opt/homebrew/opt/openjdk@17/bin/java ]]; then
		JAVA=/opt/homebrew/opt/openjdk@17/bin/java
	fi
fi

# Green cases hold every property their .cfg lists. Each case is
# <spec>:<cfg>, with the .cfg under cases/.
GREEN=(
	SyncerPairing:SyncerPrompt
	SyncerPairing:SyncerADrain
	SyncerPairing:SyncerLegacyPrompt
	SyncerPairing:SyncerLegacyADrain
	SyncerPairing:SyncerVerySlow
	SyncerPairing:SyncerByzantine
	ManagerLiveness:ManagerSafety
	ManagerLiveness:ManagerFair
	ManagerLiveness:ManagerNoTickLiveness
	ManagerLiveness:ManagerNoSettleTicks
)

# Must-fail cases each remove one rule or one fairness assumption, or run
# the production spec under an assumption the design doesn't meet (a
# finding). Each names the error TLC must report.
MUST_FAIL=(
	"SyncerPairing:SyncerNoDraining:Invariant NoCrossAttemptCredit is violated"
	"SyncerPairing:SyncerNoFirstReplyCheck:Invariant WholeStreamCredit is violated"
	"SyncerPairing:SyncerFixedDrainDeadline:Invariant NoCrossAttemptCredit is violated"
	"SyncerPairing:SyncerLegacyDrain:Invariant NoCrossAttemptCredit is violated"
	"SyncerPairing:SyncerTwoPausesFinding:Invariant NoCrossAttemptCredit is violated"
	"SyncerPairing:SyncerCompleteFlagFinding:Invariant OneOutstandingQuery is violated"
	"SyncerPairing:SyncerLegacyVerySlowFinding:Invariant WholeStreamCredit is violated"
	"SyncerPairing:SyncerOverBudgetFinding:Invariant OneOutstandingQuery is violated"
	"ManagerLiveness:ManagerNoSettleSafety:Invariant GoalA is violated"
	"ManagerLiveness:ManagerNoLocalBackoff:Action property GoalH is violated"
	"ManagerLiveness:ManagerNoSettleStarves:Temporal properties were violated"
	"ManagerLiveness:ManagerNoPinnedRetry:Temporal properties were violated"
	"ManagerLiveness:ManagerNoTickFairness:Temporal properties were violated"
	"ManagerLiveness:ManagerWeakCompletion:Temporal properties were violated"
	"ManagerLiveness:ManagerNoTakeFairness:Temporal properties were violated"
	"ManagerLiveness:ManagerNoDrainFairness:Temporal properties were violated"
	"ManagerLiveness:ManagerNoDeliveryFairness:Temporal properties were violated"
)

if [[ ! -f "$TLA2TOOLS" ]]; then
	echo "tla2tools.jar not found at ${TLA2TOOLS}: download it from"
	echo "https://github.com/tlaplus/tlaplus/releases, or set TLA2TOOLS"
	exit 1
fi
TLC_VERSION="$("$JAVA" -cp "$TLA2TOOLS" tlc2.TLC 2>&1 |
	grep -oE 'TLC2 Version [0-9.]+ of [0-9]+ [A-Za-z]+ [0-9]+' | head -1 || true)"
echo "${TLC_VERSION:-TLC}, $("$JAVA" -version 2>&1 | head -1), workers ${WORKERS}"

OUT_DIR="$(mktemp -d)"
trap 'rm -rf "$OUT_DIR"' EXIT

cd "$SPEC_DIR"
echo "=== translate: SyncerPairing.tla"
"$JAVA" -cp "$TLA2TOOLS" pcal.trans -nocfg SyncerPairing.tla \
	| grep -E "Translation completed|error" || {
	echo "ERROR: pcal.trans failed"
	exit 1
}
rm -f ./*.old

SUMMARY=()

# run_tlc runs one case and leaves its output in ${OUT_DIR}/<cfg>.out.
run_tlc() {
	local spec="$1" cfg="$2"
	"$JAVA" -XX:+UseParallelGC -cp "$TLA2TOOLS" tlc2.TLC \
		-workers "$WORKERS" -metadir "${OUT_DIR}/${cfg}.meta" \
		-config "cases/${cfg}.cfg" "${spec}.tla" \
		> "${OUT_DIR}/${cfg}.out" 2>&1 || true
}

# record adds one line to the summary from the case's TLC output.
record() {
	local cfg="$1" kind="$2" out="${OUT_DIR}/$1.out"
	local counts time
	counts="$(grep -oE '^[0-9]+ states generated, [0-9]+ distinct' "$out" |
		tail -1 | sed -E 's/ states generated, / /; s/ distinct//' || true)"
	time="$(grep -oE 'Finished in [^ ]+( [^ ]+)?' "$out" | tail -1 |
		sed -E 's/Finished in //; s/ at$//' || true)"
	SUMMARY+=("$(printf '%-30s %-9s %12s %12s  %s' "$cfg" "$kind" \
		${counts:-? ?} "$time")")
}

for entry in "${GREEN[@]}"; do
	spec="${entry%%:*}"
	cfg="${entry#*:}"
	[[ "$cfg" =~ $CASES ]] || continue

	echo "=== green: ${cfg} (expect no error)"
	run_tlc "$spec" "$cfg"
	if ! grep -q "Model checking completed. No error has been found." \
		"${OUT_DIR}/${cfg}.out"; then

		grep -E "^Error" "${OUT_DIR}/${cfg}.out" | head -5 || true
		echo "ERROR: ${cfg} found an error, but none was expected"
		exit 1
	fi
	grep -E "^[0-9]+ states generated" "${OUT_DIR}/${cfg}.out" || true
	record "$cfg" green
done

for entry in "${MUST_FAIL[@]}"; do
	spec="${entry%%:*}"
	rest="${entry#*:}"
	cfg="${rest%%:*}"
	want="${rest#*:}"
	[[ "$cfg" =~ $CASES ]] || continue

	echo "=== must fail: ${cfg} (expect: ${want})"
	run_tlc "$spec" "$cfg"
	if grep -q "No error has been found" "${OUT_DIR}/${cfg}.out"; then
		echo "ERROR: ${cfg} found no error, but one was expected"
		exit 1
	fi

	err="$(grep -m1 -E "^Error: " "${OUT_DIR}/${cfg}.out" || true)"
	echo "$err"
	if [[ "$err" != *"$want"* ]]; then
		echo "ERROR: ${cfg} failed, but not with the expected error"
		exit 1
	fi
	record "$cfg" must-fail
done

echo "=== summary"
printf '%-30s %-9s %12s %12s  %s\n' case kind states distinct time
printf '%s\n' "${SUMMARY[@]}"
