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
ATTEMPT_WEIGHT = 0.01
ATTEMPT_CAP = 15
FEE_WEIGHT = 0.00002
FEE_PPM_CAP = 5_000


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

    extra_attempts = min(
        max(agg["attempts_per_scenario"] - 1.0, 0.0), ATTEMPT_CAP,
    )
    fee_ppm = min(agg["fee_ppm_on_success"], FEE_PPM_CAP)

    score = (
        agg["success_rate"]
        - ATTEMPT_WEIGHT * extra_attempts
        - FEE_WEIGHT * fee_ppm
    )

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
            "modeling bug."
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
