#!/usr/bin/env python3
"""Generate a scenario corpus for routesim-based optimization runs.

Each example is one scenario file: a network (topology + hidden liquidity)
plus a sequence of payments from a fixed source, executed against a shared
mission control. The corpus deliberately mixes liquidity regimes and payment
sizes so that a parameter set cannot win by overfitting a single regime, and
skews hard (bimodal liquidity, amounts close to channel capacity) so the
seed candidate has real failures to learn from.

--hard drops the easy scale-free nets, --drift lets liquidity churn between
payments (exp-008), and --split generates a corpus that isolates MPP splitting
(exp-010).

--liquidity-family and --amount-family are the exp-017 robustness knobs: they
swap the hidden-liquidity generator and the payment-amount distribution the
corpus is drawn from, so a router can be checked against families it was never
evolved against. Both default to the historical behaviour and, on that
default, draw from the rng in exactly the original order: a corpus regenerated
from a fixed seed is byte-identical to the one generated before they existed.

--attribution is the exp-019 knob: it degrades the failure channel the router
learns from, which is the one part of the simulator that has always been
kinder than mainnet. It stamps a section onto every emitted file and makes no
rng draw, so it too leaves the default corpus byte-identical.
"""

import argparse
import json
import math
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

# The --hard profile: small channels with headroom, bimodal only. Kept as
# module constants so a driver can import this module and reproduce the hard
# corpus without shelling out (gen_family_corpora.py does exactly that).
HARD_TOPOLOGIES = [
    {"type": "smallworld", "num_nodes": 300,
     "channel_size_sat": 2_000_000, "avg_degree": 6},
    {"type": "smallworld", "num_nodes": 600,
     "channel_size_sat": 1_000_000, "avg_degree": 6},
    {"type": "grid", "num_nodes": 150,
     "channel_size_sat": 2_000_000},
    {"type": "hubspoke", "num_nodes": 200,
     "channel_size_sat": 4_000_000},
]
HARD_LIQUIDITY_MODELS = ["bimodal"]


def use_hard_profile() -> None:
    """Swap the module's topology and liquidity tables for the hard ones."""
    global TOPOLOGIES, LIQUIDITY_MODELS
    TOPOLOGIES = list(HARD_TOPOLOGIES)
    LIQUIDITY_MODELS = list(HARD_LIQUIDITY_MODELS)


# --- amount families (exp-017) ----------------------------------------------
#
# The tiered amounts below are drawn from a short list of fractions of a
# channel, which makes every amount in the corpus a round fraction of a round
# capacity. That is a distribution the champions were evolved against, so it
# is exactly the kind of thing they could be overfitting to. These two
# alternatives keep the scenario otherwise untouched and only re-draw the
# amounts.

# Payments below this are dust the simulator will not route.
MIN_AMT_MSAT = 1_000

# Spread of the lognormal family, in natural log units. The median is pinned
# to the amount the tiered logic would have produced, so sigma alone decides
# how far the family strays from it: at 1.0 the middle half of the draws lands
# within roughly half to twice the tiered amount and the tail runs an order of
# magnitude past it.
LOGNORMAL_SIGMA = 1.0

# Real payments cluster on round numbers, and round numbers collide: with
# everyone sending 100k sats, one node's failure bound sits exactly at the
# amount the next node is about to send. The ladder is 1 and 5 per decade of
# satoshis, which is where invoice amounts actually pile up.
ROUND_LADDER_MSAT = [
    mult * 10 ** exp * 1000
    for exp in range(3, 12)
    for mult in (1, 5)
]


def amount_scale_msat(example: dict) -> int:
    """The capacity the example's tiered amounts were sized against.

    For the ordinary topologies that is one channel; for the corridors
    topology it is the fattest tier, which is the head every --split amount is
    quoted as a multiple of.
    """
    topology = example["topology"]
    if topology["type"] == "corridors":
        return corridor_tiers_msat(topology)[0]

    return topology["channel_size_sat"] * 1000


def snap_round_msat(amt_msat: int) -> int:
    """The nearest round amount on a log scale."""
    target = math.log(max(amt_msat, MIN_AMT_MSAT))

    return min(ROUND_LADDER_MSAT, key=lambda r: abs(math.log(r) - target))


def apply_amount_family(example: dict, family: str,
                        rng: random.Random) -> dict:
    """Re-draw an example's payment amounts under an amount family.

    "tiered" is the historical behaviour and touches neither the amounts nor
    the rng, so the default path draws exactly what it always drew. The other
    families rewrite the amount of every payment in place and leave targets,
    part limits, topology and seeds alone, which is what makes a family corpus
    pair file-for-file with its control.
    """
    if family == "tiered":
        return example

    # A lognormal tail can run arbitrarily far; past twice the capacity the
    # amount was sized against, a payment is no longer a hard payment but an
    # impossible one, and impossible payments score every router the same.
    ceiling = max(2 * amount_scale_msat(example), MIN_AMT_MSAT)

    for scenario in example["scenarios"]:
        tiered = max(int(scenario["amt_msat"]), MIN_AMT_MSAT)
        if family == "lognormal":
            # Median pinned to the tiered amount, so the family is a spread
            # around what this file would otherwise have asked for rather
            # than a different corpus difficulty.
            drawn = rng.lognormvariate(math.log(tiered), LOGNORMAL_SIGMA)
            amt = min(max(int(round(drawn)), MIN_AMT_MSAT), ceiling)
        elif family == "round":
            # Snapping moves an amount by at most sqrt(5), so it needs no
            # ceiling of its own: clamping it would only push amounts back
            # off the ladder, which is the whole point of the family.
            amt = max(snap_round_msat(tiered), MIN_AMT_MSAT)
        else:
            raise ValueError(f"unknown amount family: {family}")

        scenario["amt_msat"] = amt

    return example


# --- attribution degradation (exp-019) --------------------------------------

# The knobs of the degraded failure channel, keyed by their short spec name.
ATTRIBUTION_KEYS = {
    "unknown": ("unknown_prob", float),
    "shift": ("shift_prob", float),
    "delay": ("delay_slices", int),
    "seed": ("seed", int),
}


def parse_attribution(spec: str) -> dict:
    """Parse 'unknown=0.3,shift=0.2,delay=4' into a scenario section.

    The section stamped here is what routesim reads: failures that arrive
    with no attribution at all, failures blamed on a neighbour of the node
    that really failed, and results that arrive after the network has moved.
    Omitting the flag emits no section, which is the perfect failure channel
    every corpus before exp-019 was generated against.
    """
    section = {}
    for item in spec.split(","):
        item = item.strip()
        if not item:
            continue

        name, sep, value = item.partition("=")
        name = name.strip()
        if not sep or name not in ATTRIBUTION_KEYS:
            raise ValueError(
                f"unknown attribution knob: {item!r} "
                f"(want {'|'.join(ATTRIBUTION_KEYS)}=value)"
            )

        key, cast = ATTRIBUTION_KEYS[name]
        section[key] = cast(value)

    if not section:
        raise ValueError("empty --attribution spec")

    return section

# --- splitting pressure (exp-010) -------------------------------------------
#
# The corridors topology puts one source and one target at the ends of K
# parallel corridors of deliberately unequal capacity. Every corridor ends in
# a single tier channel into the target and the target has no other channels,
# so the fattest tier is a hard ceiling on any single shard and the sum of the
# tiers a hard ceiling on the payment. Sizing a payment above the fattest tier
# therefore makes splitting mandatory, and the uneven ladder makes the right
# split unequal: 70/20/10-like, never a clean halving.

# Mirrors corridorBottleneckRatio, corridorFattestWeight and
# corridorTierLadder in routing/sim_topology.go. The three of them determine
# the tier of every corridor, which is what the amounts below are sized
# against, so they must move together with the Go side.
CORRIDOR_BOTTLENECK_RATIO = 128
CORRIDOR_FATTEST_WEIGHT = 12
CORRIDOR_TIER_LADDER = [6, 3, 5, 2]

# The fraction of the nominal tier capacity that bimodal liquidity leaves
# usable in the forward direction, measured over the generator's own seeds:
# roughly half the tier channels point the wrong way and the interior hops
# block a little more on top. Amounts are sized against this so that a file is
# usually feasible for a router that splits well while staying out of reach of
# one that splits badly. Raising it makes the corpus harder.
CORRIDOR_USABLE_FRAC = 0.40

# How much of what is left of the budget after the probes the ambitious payment
# asks for. Below 1 it leaves slack for the corridors that happen to point the
# wrong way, so a router that splits well usually completes the payment while
# one that splits badly does not. Raising it toward 1 makes the corpus harder.
LEAD_BUDGET_FRAC = 0.85

# Corridor counts run high on purpose. Every corridor is a coin flip under
# bimodal liquidity, so a network needs many of them before the corridors
# together can reliably carry more than the fattest one alone, which is the
# regime where splitting decides the payment.
SPLIT_TOPOLOGIES = [
    {"type": "corridors", "num_nodes": 80, "channel_size_sat": 96_000_000,
     "corridors": 12},
    {"type": "corridors", "num_nodes": 100, "channel_size_sat": 192_000_000,
     "corridors": 16},
    {"type": "corridors", "num_nodes": 100, "channel_size_sat": 48_000_000,
     "corridors": 20},
    {"type": "corridors", "num_nodes": 120, "channel_size_sat": 96_000_000,
     "corridors": 24},
]


def corridor_tiers_msat(topology: dict) -> list:
    """The nominal tier capacity of every corridor, in msat."""
    num = topology["corridors"]
    weights = [CORRIDOR_FATTEST_WEIGHT] + [
        CORRIDOR_TIER_LADDER[(i - 1) % len(CORRIDOR_TIER_LADDER)]
        for i in range(1, num)
    ]

    cap_msat = topology["channel_size_sat"] * 1000
    denom = weights[0] * CORRIDOR_BOTTLENECK_RATIO

    return [cap_msat * w // denom for w in weights]


def gen_split_example(rng: random.Random, leads: int = 1,
                      atomic: bool = False) -> dict:
    """One splitting-pressure scenario file: a corridors network, two cheap
    probes and payments that no single path can carry.

    With leads > 1 the file carries several ambitious payments instead of
    one. This raises per-file score resolution: with a single ambitious
    payment two-thirds of the success term is free probes and per-file
    scores are nearly binary, which quantizes minibatch selection below
    the attempt-efficiency signal actually being selected for.
    """
    topology = dict(rng.choice(SPLIT_TOPOLOGIES))
    topology["seed"] = rng.randrange(1, 2**31)

    tiers = corridor_tiers_msat(topology)
    head = tiers[0]

    # Amounts are multiples of the fattest tier, so anything above 1.0 is a
    # payment no single path can carry. The budget is what the corridors can
    # usually deliver between them, and the file spends it in one deliberate
    # order: two cheap probes first, which any router completes on one path and
    # which seed what it knows about the corridors, then one ambitious payment
    # that needs most of the corridors at once, sized to their tiers.
    #
    # Nothing refills a corridor once a shard has crossed it, and a shard that
    # settles for a payment that later fails is spent all the same, so the
    # ambitious payment goes last: it is the one that can burn the network.
    budget = CORRIDOR_USABLE_FRAC * sum(tiers) / head
    probes = [0.12, 0.18]

    if leads <= 1:
        lead = max(1.05, LEAD_BUDGET_FRAC * (budget - sum(probes)))
        lead = min(lead, 3.0)
        mults = probes + [lead]
    else:
        # Several ambitious payments share the usable budget, in a
        # descending geometric ladder: nothing refills a corridor, so
        # each successive payment is sized against what the depleting
        # network can still plausibly carry, while every lead stays
        # above the fattest tier so splitting remains mandatory. The
        # total deliberately brushes the budget: completing the tail is
        # the graded part of the score, and a router that wastes less
        # liquidity on failed shards completes more of it.
        remaining = LEAD_BUDGET_FRAC * (budget - sum(probes))
        mults = list(probes)
        size = min(remaining * 0.5, 3.0)
        for _ in range(leads):
            jitter = 0.9 + 0.2 * rng.random()
            mults.append(min(max(1.05, size * jitter), 3.0))
            size = max(1.05, size * 0.6)

    scenarios = [
        {
            "target": str(topology["num_nodes"]),
            "amt_msat": int(head * mult),
            "max_parts": 16,
        }
        for mult in mults
    ]

    example = {
        "topology": topology,
        "liquidity_model": "bimodal",
        "liquidity_seed": rng.randrange(1, 2**31),
        "source": "1",
        "scenarios": scenarios,
    }

    if atomic:
        # The exp-010b arena couples atomic commitment with a world that
        # keeps moving DURING the payment: each attempt costs thirty
        # seconds of clock, so a twenty-attempt probe ladder watches ten
        # minutes of churn while an up-front joint plan commits before
        # the corridors drift. Traffic amounts stay within the thinnest
        # rung so churn perturbs corridors without wiping them.
        example["clock"] = {
            "payment_gap_sec": 600,
            "attempt_sec": 30,
        }
        example["background_traffic"] = {
            "payments_per_gap": 8,
            "min_amt_msat": 1_000,
            "max_amt_msat": max(2_000, int(tiers[-1]) // 2),
            # A third of the churn crosses the corridors under test.
            # Traffic spread evenly over the graph moves liquidity
            # almost everywhere except the handful of channels a scored
            # payment actually uses.
            "focus_fraction": 0.33,
            "seed": rng.randrange(1, 2**31),
        }

    return example


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
            "focus_fraction": 0.33,
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
    parser.add_argument("--split", action="store_true",
                        help="splitting-pressure corpus: corridors "
                        "topologies where no single path can carry the "
                        "payment and the right split is unequal "
                        "(exp-010). Isolates the splitting variable, so "
                        "it composes with neither --hard nor --drift")
    parser.add_argument("--atomic", action="store_true",
                        help="mark every scenario atomic_mpp: shards hold "
                        "liquidity and settle or release together, and "
                        "background traffic keeps moving between attempts "
                        "(the exp-010b arena)")
    parser.add_argument("--split-leads", type=int, default=1,
                        help="ambitious payments per --split file. The "
                        "default 1 reproduces the original exp-010 "
                        "corpus; 8-10 raises per-file score resolution "
                        "so minibatch selection can see the "
                        "attempt-efficiency signal")
    parser.add_argument("--liquidity-family", default=None,
                        help="override every scenario's liquidity_model "
                        "with this exact string, e.g. bimodal:0.2, "
                        "beta:0.3:0.3, uniform, hubdrain:0.05 (exp-017). "
                        "The string passes through to the simulator "
                        "verbatim; liquidity seeds are untouched")
    parser.add_argument("--amount-family", default="tiered",
                        choices=["tiered", "lognormal", "round"],
                        help="payment-amount distribution (exp-017). "
                        "tiered is the historical fractions-of-a-channel "
                        "ladder, lognormal spreads around it with the "
                        "tiered amount as the median, round snaps to the "
                        "1/5-per-decade satoshi ladder real invoices "
                        "cluster on")
    parser.add_argument("--attribution", default=None,
                        help="degrade the failure channel of every emitted "
                        "scenario (exp-019), as "
                        "'unknown=0.3,shift=0.2,delay=4[,seed=N]': the "
                        "share of failures that arrive unattributed, the "
                        "share blamed on a neighbour of the node that "
                        "really failed, and how many attempt-sized slices "
                        "of traffic pass before a result is delivered. "
                        "Absent emits no section, which is the instant, "
                        "truthful, exactly attributed channel every "
                        "earlier corpus used")
    args = parser.parse_args()

    attribution = None
    if args.attribution is not None:
        try:
            attribution = parse_attribution(args.attribution)
        except ValueError as err:
            parser.error(str(err))

    # --split isolates one variable, so it does not mix with the other corpus
    # modes: --hard swaps the topology list it needs, and --drift adds the
    # liquidity churn it deliberately excludes.
    if args.split and (args.hard or args.drift):
        parser.error("--split composes with neither --hard nor --drift")

    if args.hard:
        use_hard_profile()

    rng = random.Random(args.seed)
    out = Path(args.out)

    for split, count in [("train", args.train), ("val", args.val),
                         ("test", args.test)]:
        split_dir = out / split
        split_dir.mkdir(parents=True, exist_ok=True)
        for i in range(count):
            if args.split:
                example = gen_split_example(
                    rng, leads=args.split_leads, atomic=args.atomic,
                )
            else:
                example = gen_example(rng, drift=args.drift)
            # Both family overrides run after the example is complete, so
            # they never move a draw the default path makes.
            apply_amount_family(example, args.amount_family, rng)
            if args.liquidity_family is not None:
                example["liquidity_model"] = args.liquidity_family
            if args.atomic:
                for scenario in example["scenarios"]:
                    scenario["atomic_mpp"] = True
            # The degradation section is stamped last and makes no draw of
            # its own, so a corpus generated without the flag is unchanged.
            if attribution is not None:
                example["attribution"] = dict(attribution)
            path = split_dir / f"example_{i:03d}.json"
            path.write_text(json.dumps(example, indent=2))
        print(f"{split}: {count} examples in {split_dir}")


if __name__ == "__main__":
    main()
