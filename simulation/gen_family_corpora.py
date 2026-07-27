#!/usr/bin/env python3
"""Emit the exp-017 robustness corpora: one tier per generator family.

Every champion this program has produced was evolved against one hidden
liquidity generator (`sim_liquidity.go`'s exponential draw at 5% of capacity)
and one payment-amount ladder (fixed fractions of a channel). Both were
written by us, and the evolved priors fit the first of them closely enough
that the fit is the top pre-upstream worry in the notebook. This driver builds
the tiers that put a number on it: the same scenarios, replayed under
liquidity families and amount families the routers never saw.

The layout is deliberately paired. Ten base scenarios are drawn once from
fixed master seeds, and every tier is those same ten with exactly one thing
changed:

    <out>/liq-<slug>/scen-NNN.json   same everything, different
                                     liquidity_model string
    <out>/amt-<family>/scen-NNN.json same everything, different amounts

So file i in liq-uniform is file i in liq-bimodal down to the byte except for
the model string, and a per-file paired delta between two tiers measures the
generator and nothing else -- no topology luck, no payment luck. liq-bimodal
is the control for both axes: it is the base corpus untouched, so the amount
tiers pair against it as well.

The liquidity strings pass through to the simulator verbatim; Python never
interprets them.

Usage:
    python3 simulation/gen_family_corpora.py --out /tmp/exp017
"""

import argparse
import hashlib
import json
import random
from pathlib import Path

import gen_scenarios

# The liquidity families under test. "bimodal" is the legacy string and the
# control; the rest are the parameterized families the simulator learned to
# parse for exp-017. Ordering is control first so the summary reads as a
# ladder away from home.
LIQUIDITY_FAMILIES = [
    "bimodal",
    "bimodal:0.01",
    "bimodal:0.2",
    "beta:0.3:0.3",
    "beta:2:2",
    "uniform",
    "hubdrain:0.05",
]

# The amount families under test. Liquidity stays on the legacy bimodal
# generator here so the two axes never move at once.
AMOUNT_FAMILIES = ["lognormal", "round"]


def slug(family: str) -> str:
    """Directory-safe form of a family string."""
    return family.replace(":", "_")


def derive_seed(*parts) -> int:
    """A stable seed from a tuple of labels.

    Explicitly hashed rather than taken from hash(), which is randomized per
    process for strings and would make the amount tiers unreproducible.
    """
    key = "|".join(str(part) for part in parts).encode()

    return int.from_bytes(hashlib.sha256(key).digest()[:8], "big")


def base_examples(count: int, seed: int) -> list:
    """The shared hard-tier scenarios every family variant is built from."""
    gen_scenarios.use_hard_profile()
    rng = random.Random(seed)

    return [gen_scenarios.gen_example(rng) for _ in range(count)]


def write_tier(out: Path, name: str, examples: list) -> Path:
    tier = out / name
    tier.mkdir(parents=True, exist_ok=True)
    for idx, example in enumerate(examples):
        (tier / f"scen-{idx:03d}.json").write_text(
            json.dumps(example, indent=2),
        )

    return tier


def amount_summary(examples: list) -> dict:
    """Min / median / max payment amount over a tier, in sats."""
    amts = sorted(
        scenario["amt_msat"] // 1000
        for example in examples
        for scenario in example["scenarios"]
    )

    return {
        "payments": len(amts),
        "min_sat": amts[0],
        "median_sat": amts[len(amts) // 2],
        "max_sat": amts[-1],
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", required=True, help="output directory")
    parser.add_argument("--files", type=int, default=10,
                        help="scenario files per family tier")
    parser.add_argument("--seed", type=int, default=20260727,
                        help="master seed for the shared base scenarios")
    args = parser.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    bases = base_examples(args.files, args.seed)

    manifest = []
    for family in LIQUIDITY_FAMILIES:
        # A fresh copy per tier: only the model string moves, and the
        # scenario lists must not be shared between tiers.
        examples = [json.loads(json.dumps(base)) for base in bases]
        for example in examples:
            example["liquidity_model"] = family
        name = f"liq-{slug(family)}"
        write_tier(out, name, examples)
        manifest.append({
            "tier": name,
            "axis": "liquidity",
            "liquidity_model": family,
            "amount_family": "tiered",
            **amount_summary(examples),
        })

    for family in AMOUNT_FAMILIES:
        examples = [json.loads(json.dumps(base)) for base in bases]
        for idx, example in enumerate(examples):
            # One rng per file, derived from the master seed, so a tier
            # regenerates identically and one file's draws never depend on
            # how many files came before it.
            rng = random.Random(derive_seed(args.seed, family, idx))
            gen_scenarios.apply_amount_family(example, family, rng)
        name = f"amt-{slug(family)}"
        write_tier(out, name, examples)
        manifest.append({
            "tier": name,
            "axis": "amount",
            "liquidity_model": "bimodal",
            "amount_family": family,
            **amount_summary(examples),
        })

    (out / "manifest.json").write_text(json.dumps(manifest, indent=2))

    print(f"{len(manifest)} tiers x {args.files} files in {out}")
    for entry in manifest:
        print(f"  {entry['tier']:18s} liq={entry['liquidity_model']:14s} "
              f"amt={entry['amount_family']:9s} "
              f"sats min={entry['min_sat']:,} "
              f"median={entry['median_sat']:,} max={entry['max_sat']:,}")


if __name__ == "__main__":
    main()
