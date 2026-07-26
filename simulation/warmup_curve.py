#!/usr/bin/env python3
"""Measure how fast each router gets cheap.

Payments inside a scenario file run in order against one mission control
and one set of candidate beliefs, so the attempt count at payment index i
says how much the first i-1 payments taught the router. Averaging that
curve over files gives a warmup curve: where it starts, how steeply it
falls, and where it flattens. This is the analysis half of exp-012 and
needs no simulator changes -- the runner already reports per-attempt
traces for every scenario.

Usage:
    python3 warmup_curve.py --tier mainnet=/path/to/mn_*.json \
        --router lnd=/path/bin/routesim:lnd \
        --router mx_c3=/path/bin/routesim_mxc3:candidate \
        [--out curves.json]
"""

import argparse
import glob
import json
import subprocess
import tempfile
from pathlib import Path


def run_file(binary: str, router: str, scenario: str) -> list[dict]:
    """Run one scenario file and return its per-payment results."""
    with tempfile.NamedTemporaryFile(suffix=".json") as out:
        subprocess.run(
            [binary, "--scenarios", scenario, f"--router={router}",
             "--out", out.name],
            check=True, capture_output=True,
        )
        return json.load(open(out.name))["results"]


def curve(binary: str, router: str, files: list[str]) -> dict:
    """Average attempts and success by payment index across files."""
    attempts: dict[int, list[int]] = {}
    successes: dict[int, list[int]] = {}

    for path in files:
        for i, result in enumerate(run_file(binary, router, path)):
            attempts.setdefault(i, []).append(len(result["attempts"] or []))
            successes.setdefault(i, []).append(
                1 if result.get("success") else 0,
            )

    indices = sorted(attempts)
    return {
        "attempts_by_index": [
            sum(attempts[i]) / len(attempts[i]) for i in indices
        ],
        "success_by_index": [
            sum(successes[i]) / len(successes[i]) for i in indices
        ],
        "n_files": len(files),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--tier", action="append", required=True,
                        help="name=glob-or-directory")
    parser.add_argument("--router", action="append", required=True,
                        help="name=binary[:router_flag]")
    parser.add_argument("--out", default=None)
    args = parser.parse_args()

    tiers = {}
    for spec in args.tier:
        name, pattern = spec.split("=", 1)
        if Path(pattern).is_dir():
            files = sorted(str(p) for p in Path(pattern).glob("*.json"))
        else:
            files = sorted(glob.glob(pattern))
        tiers[name] = files

    routers = {}
    for spec in args.router:
        name, rest = spec.split("=", 1)
        binary, _, flag = rest.partition(":")
        routers[name] = (binary, flag or "candidate")

    out = {}
    for tier, files in tiers.items():
        out[tier] = {}
        for name, (binary, flag) in routers.items():
            c = curve(binary, flag, files)
            out[tier][name] = c
            att = " ".join(f"{a:5.1f}" for a in c["attempts_by_index"])
            print(f"{tier:10s} {name:12s} attempts by payment index: {att}")

    if args.out:
        Path(args.out).write_text(json.dumps(out, indent=1))
        print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
