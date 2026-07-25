#!/usr/bin/env python3
"""Generate multi-vantage mainnet scenario files.

exp-009 validated the champions from a single, highest-degree source —
a hub-resident vantage that is exactly the easiest place to route
from. This generator writes scenario files for sources spanning the
degree distribution (deciles from well-connected to leaf-adjacent), so
validation can claim "wins from any vantage" or discover that it does
not. Payments and liquidity assignments stay in the exp-009 pattern so
numbers are comparable.

Pure JSON generation; run the sweeps separately (they are CPU-heavy).
"""

import argparse
import json
import random
from collections import Counter
from pathlib import Path


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--graph", default=str(
        Path.home() / "codez" / "data" / "mainnet_graph.json"))
    parser.add_argument("--out", required=True, help="output directory")
    parser.add_argument("--sources", type=int, default=9,
                        help="one source per degree decile")
    parser.add_argument("--payments", type=int, default=10)
    parser.add_argument("--liquidity-seeds", type=int, default=3)
    parser.add_argument("--seed", type=int, default=6061)
    args = parser.parse_args()

    graph = json.loads(Path(args.graph).read_text())

    degree = Counter()
    for edge in graph["edges"]:
        degree[edge["node1_pub"]] += 1
        degree[edge["node2_pub"]] += 1

    # Degree is heavy-tailed (median ~2, max ~2000), so rank deciles
    # would sample leaves almost exclusively. Log-spaced degree targets
    # span the vantage spectrum instead: from the exp-009 hub class down
    # to a 2-channel node, halving each step. For each target, pick the
    # node whose degree is closest (deterministic tie-break by pubkey).
    ranked = [pub for pub, _ in degree.most_common()]
    rng = random.Random(args.seed)
    max_deg = degree[ranked[0]]
    targets = [max_deg]
    while len(targets) < args.sources and targets[-1] > 2:
        targets.append(max(2, targets[-1] // 2))

    picks = []
    used = set()
    for target in targets:
        candidates = sorted(
            (pub for pub in ranked if pub not in used),
            key=lambda pub: (abs(degree[pub] - target), pub),
        )
        pick = candidates[0]
        used.add(pick)
        picks.append((pick, degree[pick]))

    nodes = [node["pub_key"] for node in graph["nodes"]]

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    manifest = []
    for idx, (source, deg) in enumerate(picks):
        for liq_seed in range(1, args.liquidity_seeds + 1):
            for model in ("bimodal", "uniform"):
                scenarios = []
                for _ in range(args.payments):
                    target = nodes[rng.randrange(len(nodes))]
                    while target == source:
                        target = nodes[rng.randrange(len(nodes))]
                    amt_sat = rng.choice(
                        [100_000, 250_000, 500_000, 1_000_000,
                         2_000_000],
                    )
                    scenarios.append({
                        "target": target,
                        "amt_msat": amt_sat * 1000,
                        "max_parts": 8,
                    })
                name = (f"mnv_{idx:02d}_deg{deg}_"
                        f"{model}_s{liq_seed}.json")
                (out / name).write_text(json.dumps({
                    "graph_file": args.graph,
                    "liquidity_model": model,
                    "liquidity_seed": liq_seed * 11 + idx,
                    "source": source,
                    "scenarios": scenarios,
                }, indent=2))
                manifest.append({
                    "file": name,
                    "source": source,
                    "degree": deg,
                    "model": model,
                })

    (out / "manifest.json").write_text(json.dumps(manifest, indent=2))
    print(f"wrote {len(manifest)} scenario files for "
          f"{args.sources} sources (degrees: "
          f"{[d for _, d in picks]}) to {out}")


if __name__ == "__main__":
    main()
