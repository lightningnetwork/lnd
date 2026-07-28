#!/usr/bin/env python3
"""The routesim evaluator: candidate params JSON in, (score, feedback) out.

The score composes the three signals a Lightning sender cares about, in
strict priority order:

  1. success rate (dominant term),
  2. attempts per payment (latency proxy, small penalty),
  3. fees paid in ppm on delivered value (small penalty).

The feedback dict carries the aggregate plus compact traces of the failed
payments (failure code and position per attempt), which is what a reflective
optimizer needs to reason about *why* a parameter set underperforms.
"""

import json
import os
import subprocess
import tempfile

ROUTESIM = os.environ.get("ROUTESIM_BIN", "routesim")

# Score weights: one extra attempt costs 1% success rate; 1000 ppm in fees
# costs 2%. Both penalties saturate so that success rate always dominates:
# the worst possible score is success_rate - 0.25, keeping deltas in
# success rate visible to the optimizer even on pathological runs.
#
# THE 1/N RULE, and do not raise the fee weight without reading it. A scored
# file holds between 6 and 10 payments (gen_scenarios.py draws randint(6, 10)),
# so abandoning one payment in the smallest file costs 1/6 = 0.167 of
# objective. The entire fee term is worth at most FEE_PPM_CAP * FEE_WEIGHT =
# 0.100. The fee term is therefore structurally incapable of paying for
# abandonment: even dropping the most expensive payment in a file and taking
# the fee penalty to zero loses money. That factor of 1.67 is the only thing
# standing between the fee term and the exp-013 give-up attractor.
#
#   The fee term's maximum value must stay strictly below 1/N, where N is the
#   payment count of the smallest scored file.
#
# Doubling FEE_WEIGHT breaks it. Removing the cap breaks it unconditionally.
# The safe way to make fees matter more is not a bigger weight on this metric,
# it is a DIFFERENT metric: fee_ppm_attempted keeps the abandoned amount in its
# denominator, so abandonment cannot improve it and the rule stops binding.
# That substitution is FEE_METRIC_ATTEMPTED below, and it runs as a
# pre-registered side-by-side arm scored offline, not as a change to what the
# optimizer maximizes.
ATTEMPT_WEIGHT = 0.01
ATTEMPT_CAP = 15
FEE_WEIGHT = 0.00002
FEE_PPM_CAP = 5_000

# The aggregate field the fee penalty is charged against. FEE_METRIC is what
# every published number in this program was scored with. FEE_METRIC_ATTEMPTED
# is the alternative: fees actually spent, including on payments that failed,
# over the amount the batch was asked to deliver. Both are reported by every
# run, so the alternative arm re-scores archived outputs with no re-execution.
FEE_METRIC = "fee_ppm_on_success"
FEE_METRIC_ATTEMPTED = "fee_ppm_attempted"

# The one sentence about fees that every evaluator says unconditionally, in the
# style the exp-017 rewrite established for the give-up rule. A thresholded
# warning would not work here for the same reason it did not work there: fees
# falling is not by itself evidence of anything.
FEE_HINT = (
    "Fees fall for two reasons, cheaper routes and fewer completed "
    "payments, and only the first is an improvement: read fee_ppm "
    "against success_rate exactly the way attempts are read against it."
)


def capped_fee_ppm(agg: dict, fee_metric: str = FEE_METRIC) -> float:
    """The fee ppm the penalty is charged on, saturated at the cap."""
    return min(agg[fee_metric], FEE_PPM_CAP)


def composite_score(agg: dict, fee_metric: str = FEE_METRIC) -> float:
    """The objective: success rate less the attempt and fee penalties.

    Passing FEE_METRIC_ATTEMPTED scores the pre-registered alternative arm
    off the same aggregate, which is why this lives in one place.
    """
    extra_attempts = min(
        max(agg["attempts_per_scenario"] - 1.0, 0.0), ATTEMPT_CAP,
    )

    return (
        agg["success_rate"]
        - ATTEMPT_WEIGHT * extra_attempts
        - FEE_WEIGHT * capped_fee_ppm(agg, fee_metric)
    )


def run_routesim(params_json: str, scenario_path: str) -> dict:
    """Run one scenario file under the candidate params, returning the
    parsed output."""
    with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False) as f:
        f.write(params_json)
        params_path = f.name

    try:
        proc = subprocess.run(
            [ROUTESIM, "--params", params_path,
             "--scenarios", scenario_path],
            capture_output=True, text=True, timeout=300,
        )
        if proc.returncode != 0:
            raise RuntimeError(proc.stderr.strip())
        return json.loads(proc.stdout)
    finally:
        os.unlink(params_path)


def summarize_failures(results: list, limit: int = 5) -> list:
    """Compact per-failure traces for reflection feedback."""
    failures = []
    for result in results:
        if result["success"]:
            continue
        attempts = result.get("attempts") or []
        failures.append({
            "target": result["scenario"]["target"],
            "amt_msat": result["scenario"]["amt_msat"],
            "num_attempts": len(attempts),
            "terminal_error": result.get("error", ""),
            "attempt_failures": [
                {
                    "hops": len(a["hops"]),
                    "failure": a.get("failure", ""),
                    "failed_at_hop": a.get("failure_hop", -1),
                }
                for a in attempts[-4:]  # the last attempts show the endgame
            ],
        })
        if len(failures) >= limit:
            break
    return failures


def evaluate(candidate: str, example) -> tuple[float, dict]:
    """The optimize_anything evaluator contract.

    `example` is the path to one scenario file.
    """
    # Reject malformed candidates with actionable feedback.
    try:
        json.loads(candidate)
    except json.JSONDecodeError as exc:
        return 0.0, {"error": f"candidate is not valid JSON: {exc}"}

    try:
        output = run_routesim(candidate, str(example))
    except Exception as exc:  # simulator rejected the params
        return 0.0, {"error": f"routesim failed: {exc}"}

    agg = output["aggregate"]

    score = composite_score(agg)

    return score, {
        "score": score,
        "aggregate": agg,
        "failed_payments": summarize_failures(output["results"]),
        "hint": (
            "success_rate dominates the score; extra attempts and fee ppm "
            "apply small penalties. Failed payments show per-attempt "
            "failure codes and the hop index where each failed. "
            "TemporaryChannelFailure = liquidity miss (probability model "
            "was wrong), FeeInsufficient/IncorrectCltvExpiry = policy "
            "modeling bug. " + FEE_HINT
        ),
    }


if __name__ == "__main__":
    # Manual check: evaluate the lnd defaults on one example.
    import sys

    defaults = subprocess.run(
        [ROUTESIM, "--dump-defaults"], capture_output=True, text=True,
        check=True,
    ).stdout

    score, info = evaluate(defaults, sys.argv[1])
    print(f"score={score:.4f}")
    print(json.dumps(info["aggregate"], indent=2))
    print(json.dumps(info["failed_payments"], indent=2))
