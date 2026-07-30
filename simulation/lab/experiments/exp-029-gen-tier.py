#!/usr/bin/env python3
"""exp-029 tier generation: the foreign balance sheet.

The graph is `~/codez/data/realistic_graph.json`, a describegraph-shaped model
of 11,255 nodes and 37,203 edges carrying a per-edge `balance` (node1's side, in
sats) and `balance_certainty`, generated from ln-scores mission-control data by
an independent model. It is the first liquidity family in this program that we
did not author.

The tier mirrors the sealed exp-009 mainnet tier's shape:

  vantage   the exp-009 hub, translated through each node's `pub_key_og`. The
            sealed tier's source 03864ef0... maps to 02cb18f3..., which is the
            rank-1 node of this graph at degree 2,013 (the sealed tier's hub had
            2,015 channels, so the model kept it).
  amounts   drawn uniformly from the four values the sealed tier uses,
            {100M, 500M, 1000M, 2000M} msat, with max_parts 8 throughout.
  files     ten, one per payment seed, so per-file pairing is exact.

Two variants per file, IDENTICAL in every field except the liquidity model:

  A  liquidity_model "from_graph"   the foreign balances
  B  liquidity_model "bimodal"      our own generator on the same topology

Both set `unbalanced_source: true`. That is a deliberate deviation from the
sealed tier's convention, which rebalances the source's own channels 50/50: if
only A skipped the rebalance, then A minus B would confound the balance family
with the source's own liquidity, and separating those two is exactly what
variant B is for. A third variant, B_rebal, keeps the sealed convention so the
cost of the rebalance itself stays measurable.
"""

import json
import random
from pathlib import Path

S = ("/private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-"
     "lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/"
     "scratchpad")
W = Path(S + "/exp029")
GRAPH = "/Users/roasbeef/codez/data/realistic_graph.json"
SEALED_HUB = ("03864ef025fde8fb587d989186ce6a4a186895ee44a926bfc370e2c366"
              "597a3f8f")
SEALED_GRAPH = "/Users/roasbeef/codez/data/mainnet_graph.json"
SEALED_TIER = ("/Users/roasbeef/gocode/src/github.com/lightningnetwork/"
               "lnd-gepa/simulation/lab/scenarios/mainnet")

AMOUNTS = [100_000_000, 500_000_000, 1_000_000_000, 2_000_000_000]
MAX_PARTS = 8
PAYMENTS_PER_FILE = 100
SEEDS = list(range(101, 111))


def sealed_target_degrees():
    """The degrees of the sealed exp-009 tier's hundred targets, measured on
    the mainnet snapshot they were drawn from. Read at generation time rather
    than hard-coded, so the mirror cannot drift from what it mirrors."""
    graph = json.loads(Path(SEALED_GRAPH).read_text())
    deg = {}
    for e in graph["edges"]:
        deg[e["node1_pub"]] = deg.get(e["node1_pub"], 0) + 1
        deg[e["node2_pub"]] = deg.get(e["node2_pub"], 0) + 1

    out = []
    for f in sorted(Path(SEALED_TIER).glob("mn_*.json")):
        for s in json.loads(f.read_text())["scenarios"]:
            out.append(deg.get(s["target"], 0))
    assert len(out) == 100 and min(out) >= 3, (len(out), min(out))

    return sorted(out)


def main():
    g = json.loads(Path(GRAPH).read_text())
    og2new = {n["pub_key_og"]: n["pub_key"] for n in g["nodes"]
              if n.get("pub_key_og")}
    source = og2new[SEALED_HUB]

    deg = {}
    for e in g["edges"]:
        deg[e["node1_pub"]] = deg.get(e["node1_pub"], 0) + 1
        deg[e["node2_pub"]] = deg.get(e["node2_pub"], 0) + 1

    # Targets are NOT drawn uniformly, and it matters. This graph's median node
    # degree is 1, so a uniform draw puts most payments at a leaf and the tier
    # degenerates into a reachability floor that every router fails equally --
    # the exp-012 multivantage trap, and the endpoint half of the exp-014
    # traffic bug.
    #
    # The sealed exp-009 tier did not draw uniformly either: its hundred targets
    # have a minimum degree of 3, a median of 6 and a maximum of 45, sitting
    # between the 68th and 96th percentile of the mainnet graph's degree
    # distribution. So we mirror that empirical distribution rather than invent
    # one: sample a degree from the sealed tier's own hundred, then take a node
    # of the closest available degree in this graph.
    sealed_degrees = sealed_target_degrees()
    by_degree = {}
    for k, d in deg.items():
        if k != source:
            by_degree.setdefault(d, []).append(k)
    for d in by_degree:
        by_degree[d].sort()
    degrees_available = sorted(by_degree)

    def pick_target(rng):
        want = rng.choice(sealed_degrees)
        near = min(degrees_available, key=lambda d: (abs(d - want), d))

        return rng.choice(by_degree[near])

    (W / "scen").mkdir(parents=True, exist_ok=True)
    manifest = {"A_from_graph": [], "B_bimodal": [], "B_rebal": []}

    for seed in SEEDS:
        rng = random.Random(seed)
        scen = []
        for _ in range(PAYMENTS_PER_FILE):
            scen.append({
                "target": pick_target(rng),
                "amt_msat": rng.choice(AMOUNTS),
                "max_parts": MAX_PARTS,
            })

        base = {
            "graph_file": GRAPH,
            "source": source,
            "unbalanced_source": True,
            "scenarios": scen,
        }

        variants = [
            ("A_from_graph", dict(base, liquidity_model="from_graph",
                                  liquidity_seed=0)),
            ("B_bimodal", dict(base, liquidity_model="bimodal",
                               liquidity_seed=seed)),
            ("B_rebal", dict(base, liquidity_model="bimodal",
                             liquidity_seed=seed, unbalanced_source=False)),
        ]
        for name, doc in variants:
            # Key order is fixed so the files are byte-stable across runs.
            ordered = {k: doc[k] for k in (
                "graph_file", "liquidity_model", "liquidity_seed",
                "unbalanced_source", "source", "scenarios")}
            path = W / "scen" / f"{name}_{seed}.json"
            path.write_text(json.dumps(ordered, indent=2))
            manifest[name].append(str(path))

    # ------------------------------------------------ distribution facts
    amts = [s["amt_msat"] for f in manifest["A_from_graph"]
            for s in json.loads(Path(f).read_text())["scenarios"]]
    amts.sort()
    tdeg = [deg[s["target"]] for f in manifest["A_from_graph"]
            for s in json.loads(Path(f).read_text())["scenarios"]]
    tdeg.sort()

    bal, cert, tails = [], [], 0
    for e in g["edges"]:
        cap = int(e["capacity"])
        if cap <= 0:
            continue
        frac = int(e["balance"]) / cap
        bal.append(frac)
        cert.append(e.get("balance_certainty", 0.0))
        if frac < 0.05 or frac > 0.95:
            tails += 1
    bal.sort()
    cert.sort()

    def q(xs, p):
        return xs[min(len(xs) - 1, int(p * len(xs)))]

    readme = {
        "graph": {
            "path": GRAPH, "nodes": len(g["nodes"]), "edges": len(g["edges"]),
            "balance_fraction_quantiles": {
                f"p{int(p*100)}": round(q(bal, p), 4)
                for p in (0.01, 0.1, 0.25, 0.5, 0.75, 0.9, 0.99)},
            "pct_channels_in_tails_sub5_or_over95": round(
                100 * tails / len(bal), 2),
            "balance_certainty_median": round(q(cert, 0.5), 4),
        },
        "vantage": {
            "sealed_exp009_hub_og": SEALED_HUB,
            "translated": source,
            "degree": deg[source],
            "rank_by_degree": 1 + sorted(deg.values(), reverse=True).index(
                deg[source]),
            "note": "the sealed exp-009 tier's hub had 2,015 channels; the "
                    "model's copy has %d" % deg[source],
        },
        "payments": {
            "files": len(SEEDS), "per_file": PAYMENTS_PER_FILE,
            "total": len(amts), "max_parts": MAX_PARTS,
            "amount_model": "uniform over the four values the sealed tier "
                            "uses",
            "amount_counts": {str(a): amts.count(a) for a in AMOUNTS},
            "target_degree_quantiles": {
                f"p{int(p*100)}": q(tdeg, p)
                for p in (0.1, 0.25, 0.5, 0.75, 0.9)},
            "targets_with_degree_1": sum(1 for d in tdeg if d == 1),
            "target_selection": "mirrors the sealed exp-009 tier's empirical "
                                "target-degree distribution (min 3, median 6, "
                                "max 45); a uniform draw would have put 58% of "
                                "payments at a degree-1 leaf and turned the "
                                "tier into a reachability floor",
            "sealed_target_degree_quantiles": {
                "min": min(sealed_target_degrees()),
                "median": sealed_target_degrees()[50],
                "max": max(sealed_target_degrees())},
        },
        "variants": {
            "A_from_graph": "liquidity_model from_graph, the foreign balances",
            "B_bimodal": "identical scenarios, our bimodal generator, "
                         "unbalanced_source true so the ONLY difference from A "
                         "is the balance family",
            "B_rebal": "B with the sealed tier's convention restored "
                       "(source channels rebalanced 50/50), so the cost of the "
                       "rebalance itself stays measurable",
        },
    }
    (W / "manifest.json").write_text(json.dumps(manifest, indent=2))
    (W / "tier-README.json").write_text(json.dumps(readme, indent=2))

    print(json.dumps(readme, indent=2))
    print(f"\n{sum(len(v) for v in manifest.values())} scenario files written")


if __name__ == "__main__":
    main()
