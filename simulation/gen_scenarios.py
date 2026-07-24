#!/usr/bin/env python3
"""Generate a scenario corpus for routesim-based optimization runs.

Each example is one scenario file: a network (topology + hidden liquidity)
plus a sequence of payments from a fixed source, executed against a shared
mission control. The corpus deliberately mixes liquidity regimes and payment
sizes so that a parameter set cannot win by overfitting a single regime, and
skews hard (bimodal liquidity, amounts close to channel capacity) so the
seed candidate has real failures to learn from.
"""

import argparse
import json
import random
from pathlib import Path

TOPOLOGIES = [
    {"type": "smallworld", "num_nodes": 200, "channel_size_sat": 5_000_000,
     "avg_degree": 8},
    {"type": "smallworld", "num_nodes": 500, "channel_size_sat": 2_000_000,
     "avg_degree": 6},
    {"type": "hubspoke", "num_nodes": 150, "channel_size_sat": 10_000_000},
    {"type": "grid", "num_nodes": 100, "channel_size_sat": 3_000_000},
    # Mainnet-like: preferential attachment hubs, log-normal capacities.
    {"type": "scalefree", "num_nodes": 800, "channel_size_sat": 3_000_000,
     "avg_degree": 6},
    {"type": "scalefree", "num_nodes": 1500, "channel_size_sat": 2_000_000,
     "avg_degree": 8},
]

# Bimodal dominates: it is both the realistic and the hard regime.
LIQUIDITY_MODELS = ["bimodal", "bimodal", "uniform"]


def gen_example(rng: random.Random, drift: bool = False) -> dict:
    topology = dict(rng.choice(TOPOLOGIES))
    topology["seed"] = rng.randrange(1, 2**31)

    num_nodes = topology["num_nodes"]
    cap_msat = topology["channel_size_sat"] * 1000

    scenarios = []
    for _ in range(rng.randint(6, 10)):
        # Payment sizes from 1% up to a full channel capacity. Singles are
        # capped at 40% of one channel so that the sender can always fund
        # them; MPP payments may exceed one channel to force splitting.
        max_parts = rng.choice([1, 4, 16])
        if max_parts == 1:
            frac = rng.choice([0.01, 0.05, 0.1, 0.25, 0.4])
        else:
            frac = rng.choice([0.1, 0.25, 0.5, 0.8, 1.0])
        amt = int(cap_msat * frac)
        scenarios.append({
            "target": str(rng.randint(2, num_nodes)),
            "amt_msat": amt,
            "max_parts": max_parts,
        })

    example = {
        "topology": topology,
        "liquidity_model": rng.choice(LIQUIDITY_MODELS),
        "liquidity_seed": rng.randrange(1, 2**31),
        "source": "1",
        "scenarios": scenarios,
    }

    if drift:
        # Virtual time passes between payments and background senders move
        # hidden liquidity in the gaps: ten minutes per gap, with traffic
        # volume scaled to the network size so knowledge genuinely goes
        # stale between a node's own sends. Amounts are log-uniform from
        # dust up to half a channel.
        example["clock"] = {
            "payment_gap_sec": 600,
            "attempt_sec": 1,
        }
        example["background_traffic"] = {
            "payments_per_gap": max(10, num_nodes // 10),
            "min_amt_msat": max(1_000, cap_msat // 1_000),
            "max_amt_msat": cap_msat // 2,
            "seed": rng.randrange(1, 2**31),
        }

    return example


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", default="corpus", help="output directory")
    parser.add_argument("--train", type=int, default=20)
    parser.add_argument("--val", type=int, default=8)
    parser.add_argument("--test", type=int, default=8)
    parser.add_argument("--seed", type=int, default=2026)
    parser.add_argument("--hard", action="store_true",
                        help="bimodal-only, small-channel topologies with "
                        "headroom (drop easy scale-free nets)")
    parser.add_argument("--drift", action="store_true",
                        help="enable the virtual clock and background "
                        "traffic so liquidity drifts between payments "
                        "(exp-008)")
    args = parser.parse_args()

    global TOPOLOGIES, LIQUIDITY_MODELS
    if args.hard:
        TOPOLOGIES = [
            {"type": "smallworld", "num_nodes": 300,
             "channel_size_sat": 2_000_000, "avg_degree": 6},
            {"type": "smallworld", "num_nodes": 600,
             "channel_size_sat": 1_000_000, "avg_degree": 6},
            {"type": "grid", "num_nodes": 150,
             "channel_size_sat": 2_000_000},
            {"type": "hubspoke", "num_nodes": 200,
             "channel_size_sat": 4_000_000},
        ]
        LIQUIDITY_MODELS = ["bimodal"]

    rng = random.Random(args.seed)
    out = Path(args.out)

    for split, count in [("train", args.train), ("val", args.val),
                         ("test", args.test)]:
        split_dir = out / split
        split_dir.mkdir(parents=True, exist_ok=True)
        for i in range(count):
            example = gen_example(rng, drift=args.drift)
            path = split_dir / f"example_{i:03d}.json"
            path.write_text(json.dumps(example, indent=2))
        print(f"{split}: {count} examples in {split_dir}")


if __name__ == "__main__":
    main()
