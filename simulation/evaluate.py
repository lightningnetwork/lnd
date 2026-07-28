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
#
# AND THE RULE APPLIES TO BOTH FEE METRICS, which corrects the exp-023 design
# spec. The spec argued that fee_ppm_attempted escapes the rule because the
# abandoned amount stays in its denominator, so the weight could safely rise
# once the fee term was charged against it. It does not escape. Fixing the
# denominator only stops abandonment from SHRINKING it; the numerator still
# falls, because a payment nobody completes pays no fee. Abandoning a payment
# that costs f and spends s on partial shards moves the ratio from (F+f)/A to
# (F+s)/A, which is a weak improvement for EVERY payment that pays a fee.
# fee_ppm_on_success is at least partly self-limiting by comparison: it only
# improves when the abandoned payment was dearer than the file's average, and
# abandoning a cheap payment makes it worse. Measured on the sealed hard tier,
# switching metric raises the objective of the arm that abandons most by the
# most (exp-023 stage C landing note).
#
# What fee_ppm_attempted is actually for is different and still worth having:
# it counts money that LEFT THE SENDER, including on payments that then failed,
# which fee_ppm_on_success cannot see at all. A router that burns fees on
# partial mpp shards that never complete is invisible to the scored metric and
# obvious in this one. It runs as a pre-registered side-by-side arm scored
# offline, not as a change to what the optimizer maximizes, and the 1/N rule
# above governs it exactly as it governs the scored metric.
ATTEMPT_WEIGHT = 0.01
ATTEMPT_CAP = 15
FEE_WEIGHT = 0.00002
FEE_PPM_CAP = 5_000

# The aggregate field the fee penalty is charged against. FEE_METRIC is what
# every published number in this program was scored with: fees on the payments
# that completed, over the amount they delivered. FEE_METRIC_ATTEMPTED is the
# alternative: fees actually spent, including on payments that failed, over the
# amount the batch was asked to deliver. Both are reported by every run, so the
# alternative arm re-scores archived outputs with no re-execution.
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


# --- objective L: latency as the cost (exp-023 stage E) ---------------------
#
# PRE-REGISTERED, and OFFLINE. The scored objective above is unchanged and
# stays unchanged: makespan_sec and the payment latencies stay out of it for
# this whole program, by the lead's decision at spec review, until an
# experiment earns them a place. What follows re-scores archived runs with no
# re-execution, which is the same construction the fee_ppm_attempted arm uses.
#
# The substitution is the deepest reading of "latency as a cost": REPLACE the
# attempt term rather than supplement it.
#
#   objective L = success_rate
#                 - w_t * min(mean_payment_latency_sec, cap)
#                 - FEE_WEIGHT * min(fee_ppm, FEE_PPM_CAP)
#
# It matters because the attempt axis has been doing work it should not. Three
# parallel shards cost one unit of time and three units of attempt penalty. A
# nine hop route and a two hop route cost the same attempt penalty. exp-019
# already retired the 8.6x attempt headline as a perfect-channel artifact;
# this is the question of whether the attempt axis was ever measuring the
# thing it claimed to.
#
# THE 1/N RULE GOVERNS THIS TERM TOO, and check_latency_budget is where it is
# enforced rather than merely stated. A term whose maximum value reaches 1/N
# can be paid for by abandoning one payment in the smallest scored file, which
# is the exp-013 attractor. The attempt term it replaces maxes out at
# ATTEMPT_WEIGHT * ATTEMPT_CAP = 0.15 against 1/6 = 0.167, and a latency term
# has to clear the same bar. It is easier to break here than with the fee
# term, because w_t is calibrated from data rather than chosen: a reference
# arm with low latencies produces a large weight, and a large weight against a
# generous cap breaks the rule without anyone typing a number.
LATENCY_METRIC = "mean_payment_latency_sec"

# The payment count of the smallest scored file, which is what 1/N is measured
# against: gen_scenarios.py draws randint(6, 10) payments per example.
MIN_SCORED_PAYMENTS = 6


def check_latency_budget(weight: float, cap: float,
                         payments: int = MIN_SCORED_PAYMENTS) -> None:
    """Refuse an objective L calibration that could pay for abandonment.

    The maximum penalty is weight * cap, and abandoning one payment in the
    smallest scored file costs 1/payments of objective. If the first reaches
    the second, a candidate can buy a better score by giving up, which is
    exp-013 and is the one failure mode this program has already paid for.
    """
    worst = weight * cap
    limit = 1.0 / payments
    if worst >= limit:
        raise ValueError(
            f"objective L would saturate at {worst:.3f}, at or past the "
            f"{limit:.3f} an abandoned payment costs in a {payments}-payment "
            f"file: lower the cap or the weight. The attempt term it "
            f"replaces saturates at {ATTEMPT_WEIGHT * ATTEMPT_CAP:.3f}."
        )


def payment_latency_sec(agg: dict) -> float:
    """The latency objective L is charged on, with a legible failure.

    Every run before exp-023 stage E reports no latency at all, and a run of a
    tier with no latency section reports none either. Neither can be re-scored
    on time, and silently reading a zero would score them as instantaneous.
    """
    if LATENCY_METRIC not in agg:
        raise KeyError(
            f"no {LATENCY_METRIC} in this aggregate: objective L can only "
            f"re-score a run of a tier that carried a latency section, so "
            f"the tier has to be re-run with one rather than converted"
        )

    return agg[LATENCY_METRIC]


def latency_penalty(agg: dict, weight: float, cap: float) -> float:
    """The time penalty, saturated the way the attempt penalty is."""
    return weight * min(payment_latency_sec(agg), cap)


def objective_l(agg: dict, weight: float, cap: float,
                fee_metric: str = FEE_METRIC) -> float:
    """Objective L: success rate less the LATENCY and fee penalties.

    The attempt term is replaced rather than supplemented, which is the whole
    question. Callers pass a weight from calibrate_latency_weight and a cap
    they have checked with check_latency_budget.
    """
    return (
        agg["success_rate"]
        - latency_penalty(agg, weight, cap)
        - FEE_WEIGHT * capped_fee_ppm(agg, fee_metric)
    )


def calibrate_latency_weight(aggs: list, cap: float) -> float:
    """The weight that makes objective L cost a reference arm what attempts do.

    Calibration is against one named arm's runs, per the spec: the weight is
    chosen so the mean time penalty on the current champion equals the mean
    attempt penalty it pays today. Everything else is then re-scored with that
    weight, so a router does better under objective L only by being faster
    than the champion was, not by the term being cheaper for everybody.

    Returns zero when the reference arm records no time at all, which is a
    tier with no latency section rather than an instantaneous router.
    """
    if not aggs:
        raise ValueError("calibrate_latency_weight needs at least one run")

    attempts = sum(
        min(max(agg["attempts_per_scenario"] - 1.0, 0.0), ATTEMPT_CAP)
        for agg in aggs
    ) / len(aggs)
    seconds = sum(
        min(payment_latency_sec(agg), cap) for agg in aggs
    ) / len(aggs)

    if seconds <= 0:
        return 0.0

    return ATTEMPT_WEIGHT * attempts / seconds


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
