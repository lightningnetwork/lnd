#!/usr/bin/env python3
"""Build hot-load variants of a corpus for exp-012.

Takes scenario files and emits copies whose scored batch is untouched
but which are preceded by N unscored warmup payments, optionally aged by
a staleness gap and optionally sent from a different node. Holding the
scored batch fixed and varying only what the router knew when it started
is what separates learning from payment difficulty -- the confound the
warmup-curve analysis could not remove.

Warmup payments are drawn from the same amount distribution as the
scored ones and aimed at targets drawn from the file's own scenarios, so
warming teaches the router about the corridors it will actually use. A
fixed seed per (file, N) keeps every arm of the sweep reproducible.

Usage:
    python3 gen_warmup_scenarios.py --src corpus/test --out /tmp/warm \
        --warmup 25 [--stale-gap-sec 3600] [--source 42] [--seed 7]
"""

import argparse
import json
import random
from pathlib import Path


def warmup_payments(scenarios: list[dict], n: int,
                    rng: random.Random) -> list[dict]:
    """Draw n warmup payments resembling the scored ones."""
    amounts = [s["amt_msat"] for s in scenarios]
    targets = [s["target"] for s in scenarios]
    template = dict(scenarios[0])

    out = []
    for _ in range(n):
        payment = dict(template)
        # Jitter the amount so warmup does not merely memorize the exact
        # payments it is about to be scored on: a served cache carries
        # knowledge of channels, not of your future invoices.
        payment["amt_msat"] = int(rng.choice(amounts) * rng.uniform(0.5, 1.2))
        payment["target"] = rng.choice(targets)
        out.append(payment)

    return out


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--src", required=True,
                        help="source corpus directory")
    parser.add_argument("--out", required=True,
                        help="output directory")
    parser.add_argument("--warmup", type=int, required=True,
                        help="number of unscored warmup payments")
    parser.add_argument("--stale-gap-sec", type=float, default=0.0,
                        help="virtual seconds (with background traffic) "
                        "between warmup and the scored batch")
    parser.add_argument("--source", default=None,
                        help="send warmup payments from this node "
                        "instead of the file's own source")
    parser.add_argument("--seed", type=int, default=9091)
    args = parser.parse_args()

    src = Path(args.src)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    files = sorted(src.glob("*.json"))
    for i, path in enumerate(files):
        example = json.loads(path.read_text())
        rng = random.Random(args.seed + 1000 * args.warmup + i)

        if args.warmup > 0 or args.stale_gap_sec > 0:
            warmup = {
                "scenarios": warmup_payments(
                    example["scenarios"], args.warmup, rng,
                ),
            }
            if args.stale_gap_sec > 0:
                warmup["stale_gap_sec"] = args.stale_gap_sec
            if args.source:
                warmup["source"] = args.source
            example["warmup"] = warmup

        (out / path.name).write_text(json.dumps(example, indent=2))

    print(f"{len(files)} files -> {out} "
          f"(warmup={args.warmup}, gap={args.stale_gap_sec}s, "
          f"source={args.source or 'self'})")


if __name__ == "__main__":
    main()
