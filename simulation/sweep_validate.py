#!/usr/bin/env python3
"""Champion validation sweeps with honest statistics.

Runs a set of router binaries over one or more scenario tiers and
reports, per tier: mean composite objective with a bootstrap confidence
interval, success and attempts, and PAIRED per-file comparisons against
a chosen baseline router (mean paired delta, its bootstrap CI, and a
sign test). Point estimates from a single ordering are not enough to
propose anything upstream; this makes the uncertainty visible.

Usage:
    python3 sweep_validate.py --tier name=/path/to/dir_or_glob ... \
        --router name=/path/to/binary[:router_flag] ... \
        --baseline mx_c3 --out results.json

Router flag defaults to "candidate"; use lnd=/path/routesim:lnd for the
production stack.
"""

import argparse
import glob as globmod
import json
import math
import random
import subprocess
from pathlib import Path

ATTEMPT_WEIGHT = 0.01
ATTEMPT_CAP = 15
FEE_WEIGHT = 0.00002
FEE_PPM_CAP = 5_000

BOOTSTRAP_ITERS = 10_000
BOOTSTRAP_SEED = 20260725


def score_file(binary: str, router: str, scenario: str,
               params: str = "") -> dict:
    cmd = [binary, "--scenarios", scenario, f"--router={router}",
           "--traces=false"]
    if params:
        cmd += ["--params", params]
    proc = subprocess.run(
        cmd,
        capture_output=True, text=True, timeout=1800,
    )
    if proc.returncode != 0:
        raise RuntimeError(f"{binary} {scenario}: {proc.stderr[-400:]}")
    agg = json.loads(proc.stdout)["aggregate"]
    extra = min(max(agg["attempts_per_scenario"] - 1.0, 0.0), ATTEMPT_CAP)
    fee = min(agg["fee_ppm_on_success"], FEE_PPM_CAP)
    return {
        "objective": (agg["success_rate"] - ATTEMPT_WEIGHT * extra
                      - FEE_WEIGHT * fee),
        "success": agg["success_rate"],
        "attempts": agg["attempts_per_scenario"],
    }


def bootstrap_ci(values: list, rng: random.Random,
                 iters: int = BOOTSTRAP_ITERS) -> tuple:
    """95% percentile bootstrap CI of the mean."""
    n = len(values)
    if n == 0:
        return (float("nan"), float("nan"))
    means = sorted(
        sum(rng.choice(values) for _ in range(n)) / n
        for _ in range(iters)
    )
    return (means[int(0.025 * iters)], means[int(0.975 * iters)])


def sign_test(deltas: list) -> float:
    """Two-sided sign test p-value on paired deltas (zeros dropped)."""
    nonzero = [d for d in deltas if d != 0]
    n = len(nonzero)
    if n == 0:
        return 1.0
    wins = sum(1 for d in nonzero if d > 0)
    # Two-sided binomial tail at p=0.5.
    tail = sum(
        math.comb(n, k)
        for k in range(0, min(wins, n - wins) + 1)
    ) / 2 ** n
    return min(1.0, 2 * tail)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tier", action="append", required=True,
                        help="name=dir_or_glob of scenario JSON files")
    parser.add_argument("--router", action="append", required=True,
                        help="name=binary[:flag[:params]], flag defaults to "
                        "'candidate'")
    parser.add_argument("--baseline", default=None,
                        help="router name to pair comparisons against")
    parser.add_argument("--out", default=None)
    args = parser.parse_args()

    routers = {}
    for spec in args.router:
        name, _, rest = spec.partition("=")
        parts = rest.split(":")
        binary = parts[0]
        flag = parts[1] if len(parts) > 1 and parts[1] else "candidate"
        params = parts[2] if len(parts) > 2 else ""
        routers[name] = (binary, flag, params)

    rng = random.Random(BOOTSTRAP_SEED)
    report = {}

    for tier_spec in args.tier:
        tier, _, pattern = tier_spec.partition("=")
        path = Path(pattern)
        files = (sorted(str(p) for p in path.glob("*.json"))
                 if path.is_dir() else sorted(globmod.glob(pattern)))
        if not files:
            print(f"!! tier {tier}: no files for {pattern}")
            continue

        per_file = {}
        for name, (binary, flag, params) in routers.items():
            per_file[name] = [
                score_file(binary, flag, f, params) for f in files
            ]
            objs = [r["objective"] for r in per_file[name]]
            lo, hi = bootstrap_ci(objs, rng)
            mean = sum(objs) / len(objs)
            succ = sum(r["success"] for r in per_file[name]) / len(objs)
            att = sum(r["attempts"] for r in per_file[name]) / len(objs)
            report.setdefault(tier, {})[name] = {
                "objective": mean,
                "ci95": [lo, hi],
                "success": succ,
                "attempts": att,
                "n_files": len(objs),
                "per_file_objective": objs,
            }
            print(f"{tier:12s} {name:8s} obj={mean:.3f} "
                  f"[{lo:.3f},{hi:.3f}] succ={succ:.3f} att={att:.1f}",
                  flush=True)

        if args.baseline and args.baseline in per_file:
            base = [r["objective"] for r in per_file[args.baseline]]
            for name in routers:
                if name == args.baseline:
                    continue
                other = [r["objective"] for r in per_file[name]]
                deltas = [o - b for o, b in zip(other, base)]
                lo, hi = bootstrap_ci(deltas, rng)
                p = sign_test(deltas)
                report[tier][name]["paired_vs_" + args.baseline] = {
                    "mean_delta": sum(deltas) / len(deltas),
                    "ci95": [lo, hi],
                    "sign_test_p": p,
                }
                print(f"{tier:12s} {name:8s} vs {args.baseline}: "
                      f"delta={sum(deltas)/len(deltas):+.3f} "
                      f"[{lo:+.3f},{hi:+.3f}] p={p:.3f}", flush=True)

    if args.out:
        Path(args.out).write_text(json.dumps(report, indent=2))
        print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
