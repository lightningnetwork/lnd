# EXP-023 stage B — landed, with four spec-vs-reality findings

**Date:** 2026-07-27
**Status:** implemented (seven commits on `econ-realism`, ending
fe84ec412 plus this writeup); the evolution arm and the paired tier
sweep remain to run.

Stage B of the economic-realism program is in the tree: `SimPolicy`
carries the inbound fee pair, the describegraph loader keeps the 4,783
real policies it used to parse away, `checkPolicy` charges the total
node fee floored at zero, the sealed gossip view exposes each node's own
inbound fee in exactly one place, the background traffic engine prices
it too, and the `inbound_fees` scenario section with the
`mainnet_empirical`, `heavy` and `as_loaded` families is stamped by
`--inbound-fees` in `gen_scenarios.py`.

## The census, verified

Recounted independently against `~/codez/data/mainnet_graph.json`. Every
figure in the design spec reproduces exactly:

| statistic | spec | measured |
|---|---|---|
| announced directed policies | 62,798 | 62,798 |
| carrying a non-zero inbound fee | 4,783 (7.6%) | 4,783 (7.616%) |
| discounts | 4,660 | 4,660 |
| surcharges | 123 | 123 |
| distinct nodes advertising one | 284 | 284 |
| median inbound rate | -200 ppm | -200 ppm |
| 5th percentile | -2,000 ppm | -2,000 ppm |
| most negative | -18,800 ppm | -18,800 ppm |
| base component | ~always 0, to -10,000 | 837 non-zero, all negative, to -10,000 |

Two refinements the spec did not record, both of which the empirical
family needed. The sign split is over the RATE: 4,601 policies announce
a negative rate, 123 a positive one, and 59 announce a base component
and no rate at all. And the largest surcharge is 1,000,000 ppm, a node
charging the amount again to receive it; the family keeps it, because a
router that cannot survive one pathological node is worth knowing about.

## Four things implementation taught the spec

1. **A discount leaves NO trace on the wire, so the wire counters
   cannot be the manipulation check.** Stage A learned that announced
   limits bind at plan time; stage B is the sharper version of the same
   lesson. A forwarding node's inbound fee only decides whether an htlc
   is refused. It never changes what money moves, because the amounts
   on a route are chosen by the sender. So on a tier of pure discounts
   the mechanism can be everywhere and refuse nothing, and
   `inbound_fee_refusals` reads zero for a reason that has nothing to
   do with how much the fees matter. The CENSUS is the measurement here
   (`inbound_fee_charging`/`discounts`/`surcharges`), `charged` says only
   that the mechanism reached the wire, and `refusals` is an alarm.
   Recorded at all six declarations.

2. **The refusal alarm is the expected reading for a candidate, not a
   bug.** lnd prices inbound fees at plan time and reports zero
   refusals on every tier measured. Every evolved router in this
   program builds its adjacency from one policy per directed edge, so
   it cannot see an inbound fee at all, and it underpays surcharges the
   moment a tier has any: the seed candidate takes 2 refusals on the
   empirical hard tier and 27 on the authored one. That is H-B1's
   starting gun rather than a defect.

3. **The gate has to cover the gossip view and the traffic engine, not
   just `checkPolicy`.** The loader now preserves real inbound fees
   unconditionally, so a gate only on forwarding would still have moved
   every mainnet number the day lnd's pathfinder started reading a
   non-zero `DirectedChannel.InboundFee`. One flag on `SimGraph` gates
   all three. The traffic engine had to learn the arithmetic for the
   same reason exp-014 exists: a background payment that fails moves no
   liquidity, so a fee-blind environment would silently drop the churn
   every scenario file asks for on any tier with surcharges in it.

4. **The mainnet tier is NOT byte-reproducible, and never was.** Found
   while running the byte-identity proof: the pre-change binary at
   c6ee8b44b produces two or more distinct whole outputs across repeated
   runs of the SAME file on all 11 mainnet scenario files, on BOTH the
   lnd and candidate arms. The cause is lnd's own production code,
   `pathfind.go:1106` iterating `nodeEdgeUnifier.edgeUnifiers`, a Go map
   whose iteration order is randomized: tied candidate predecessors
   break by iteration order, and on a 12,161 node graph ties are common.
   The synthetic tiers are unaffected and reproduce byte for byte.
   Aggregates are mostly stable but not always: over five runs per file,
   `mn_11_uniform` moved its `total_attempts`, and an earlier four-run
   pass caught `mn_55_uniform` doing the same. Nothing published is
   invalidated, since the drift is in the last digit of an attempt count
   rather than in success, but every future mainnet claim should be
   read as having a small run-to-run component that no seed controls.

## Byte identity, proven

Flag off is byte identical, proven the stage A way against a binary
built at c6ee8b44b:

- **88 paired whole-output runs, zero diffs.** Sealed hard tier (10
  files), sealed OOD tier (10), a fresh default corpus (8), plus
  regenerated drift, atomic, split and hard corpora (16), each run on
  both the lnd and candidate arms.
- **220 mainnet runs, zero aggregate mismatches.** 11 files x 2 arms x
  5 runs x 2 binaries, compared as sets of aggregates because of finding
  4. The mainnet tier is byte-identical by default: the loader preserves
  its real inbound fees, and with no section they are dead data.
- **Generator output tree diff-identical** at a fixed seed either side
  of the change.
- Two goldens in the tree: `TestCheckPolicyLegacyGolden` pins what the
  rewritten fee line accepts and refuses, and `TestInboundFeesAbsentGolden`
  pins that the topology generator announces no inbound fee anywhere.
  `TestInboundFeeForwarding` transcribes lnd's own
  `TestChannelLinkInboundFee` cases so the arithmetic is checked against
  lnd's rather than against itself.

## Smoke, labelled as smoke

Single runs, n=10 or 11 files, no pairing statistics. NOT results.

**Mainnet with `as_loaded`** (the snapshot's own inbound fees priced,
11 files, both arms). Objective delta +0.0000 for both arms; lnd's mean
realized fee moves 204.94 to 204.58 ppm and the candidate's not at all.
The mechanism fires but barely: 0 to 57 forwarding hops per file price a
non-zero inbound fee, because only 7.6% of policies carry one and
mainnet routes are short (exp-019b measured 1.9 mean hops). The census
reads 62,854 policies, 4,783 charging, 4,660 discounts, 123 surcharges.
Read this as the realism anchor, not the power source, exactly as stage
A's empirical family had to be.

**Hard tier with the synthetic families** (10 files, both arms):

| arm | knob | succ | att | fee ppm | charged | refused | obj |
|---|---|---|---|---|---|---|---|
| lnd | off | 0.493 | 45.5 | 2778 | 0 | 0 | 0.309 |
| lnd | empirical | 0.480 | 52.2 | 2324 | 32 | 0 | 0.306 |
| lnd | heavy | 0.527 | 45.5 | **573** | 383 | 0 | **0.391** |
| candidate | off | 0.704 | 34.7 | 2736 | 0 | 0 | 0.530 |
| candidate | empirical | 0.704 | 33.6 | 2743 | 145 | 2 | 0.530 |
| candidate | heavy | 0.704 | 39.4 | 2926 | 2088 | 27 | 0.526 |

The authored family separates the arms in exactly the shape H-B2
predicts and the empirical one does not. lnd captures the discounts (fee
ppm 2778 to 573, success up 3.4 points) because its pathfinder prices
them; the seed candidate cannot see them, pays the same fees, and takes
27 refusals where it underpays a surcharge. The pre-registered stage B
hypotheses should therefore be tested on `heavy`, with `mainnet_empirical`
reported as the realism anchor. That is the same corpus-design warning
stage A ended on, arrived at independently.

## Schema as landed

```json
"inbound_fees": {"family": "mainnet_empirical", "seed": 0}
```

`--inbound-fees mainnet_empirical|heavy|as_loaded` on
`gen_scenarios.py`, or `family=heavy,seed=N` to pin the seed. Absent
emits no section, which leaves the mechanism off entirely.

Reported fields: `inbound_fee_policies`, `inbound_fee_charging`,
`inbound_fee_discounts`, `inbound_fee_surcharges` (census, emitted only
when a file asks for the mechanism), `inbound_fee_charged` and
`inbound_fee_refusals` (wire).

## Open for the lead

1. **Finding 4 wants a decision.** The mainnet tier's run-to-run
   variation is lnd's own map iteration and cannot be seeded away
   without patching production code. Options: leave it and report
   mainnet numbers as means over k runs, sort `edgeUnifiers` keys behind
   a sim-only flag (a divergence from lnd, but a small and legible one),
   or accept single runs and state the caveat. This predates stage B and
   touches every published mainnet number, so it is not stage B's call.
2. **`heavy` is authored and should be treated as stage A's `tight`
   is.** It keeps the measured shape and sign split and turns two dials,
   the share to 1.0 and the magnitudes by 5. That is a choice, and the
   smoke above shows it is the only rung with power.
3. **No champion carries an inbound-fee-aware variant.** exp-016 had to
   add importer variants of two champions after the fact because nothing
   in the contract had ever asked for the capability. The same shape is
   here: the contract now documents inbound fees, but hb1, mx_c3 and
   atomic1 were all evolved before it did, so a sweep measures their
   blindness rather than their design. Whether to hand-write aware
   variants (as exp-016 did) or to let the evolution arm answer it is a
   budget question for the lead.

## Lead decision on the mainnet nondeterminism (2026-07-28)

Accept and caveat, for now. The map-iteration tie-break in lnd's own
`pathfind.go` predates every published mainnet number, its aggregate
effect is exactly the ±0.1-attempt wobble the protocol already
tolerates (the objective gate at three decimals has held through
every reproduction, 24/24 cells in the latest sweep), and a sim-only
deterministic sort would CHANGE route choices, shifting the published
numbers it exists to protect. So: the wobble now has two identified
sources on record (wall-clock penalty decay and this tie-break), the
caveat attaches to mainnet attempt figures at that precision, and a
deterministic tie-break is deferred to the next full re-baseline,
where all mainnet numbers regenerate together under one binary.
