# EXP-027 — The integrated router, measured: interval-lnd is a champion in lnd's body

**Date:** 2026-07-29 (round 2 battery + round 3 re-bench, same day).
**Status:** complete. Round 2 tip (ab1c123ab) adjudicated in full;
round 3 (budget pricing + quarantine, 1bcbb1485) re-benched paired
against the round-2 raws — see the addendum at the bottom.

## Why this ran

Fourteen commits of integration work put the evolved belief system
inside lnd's real payment lifecycle — real pathfinding types, real
mission control seams, real SQL persistence, a flag flip away from
stock. Every number we had for "the champions" came from the sealed
simulator contract, where the candidate owns route selection outright.
Nobody had measured whether the PORT keeps the edge once the machinery
runs behind lnd's session interface, budget plumbing, and MPP
lifecycle. This is the e2e regression test the branch needed before
anyone says the word upstream.

## Method

A fresh worktree merged `interval-router`@ab1c123ab into the simulator
tree (`interval-sim` = gepa@7a2014c8e + the branch; two conflicts,
both bookkeeping, recorded in the merge commit). A new
`router_impl=interval` params knob swaps lnd's stock payment session
for `newIntervalPaymentSession` on the simulator's lnd arm — mission
control still built and fed every outcome, exactly as on a real node
running the flag. Two lifecycle seams are mirrored rather than
approximated: `PaymentResultReporter` delivery uses the same source
index and message mission control just received (unreadable failures
deliver `(nil, nil)`), and a finisher hook stands in for the deferred
`ReleaseAttempts`.

Two gates before any science: byte-identity of the merged binary
against the merge-base binary with the knob off (104/104
self-deterministic cells identical, 50.8 MB of full stdout with traces
on), and bit-exact reproduction of the exp-023 gate table (24/24
cells). Then a 6-arm × 14-tier × 134-file battery: 804 runs, zero
errors, three pre-registered questions.

## Q1 — regression vs stock lnd: none. The port kept the paradigm.

interval-lnd (ilnd) beats stock lnd on all six classic tiers by the
champions' own margin, not a fraction of it:

| tier | lnd | ilnd | mx_c3 | ilnd−lnd | mx_c3−lnd |
|---|---|---|---|---|---|
| hard | 0.309 | 0.571 | 0.583 | +0.262 (10/0, p=.002) | +0.274 |
| ood | 0.357 | 0.570 | 0.581 | +0.213 (9/1, p=.022) | +0.223 |
| split | 0.837 | 0.874 | 0.876 | +0.037 (6/2) | +0.039 |
| drift | 0.236 | 0.435 | 0.454 | +0.198 (8/0, p=.008) | +0.217 |
| atomic | 0.320 | 0.446 | 0.444 | +0.126 (8/0, p=.008) | +0.124 |
| mainnet | 0.694 | 0.788 | 0.791 | +0.095 [+.039,+.159] | +0.097 |

Attempts land on the champions' numbers too: mainnet 19.8 → 2.5
(champions 2.3), atomic 108.2 → 12.7. Against the champions
themselves, ilnd is indistinguishable from mx_c3 on five of six tiers
(mx_c3 takes drift by 0.019, 0/6, p=.031) and takes split off hb1
outright (+0.060, 8/0, p=.008 — consistent with exp-020: split is
where hb1 cannot fragment). Every lead grows when the attempt cap is
removed, so none of this is cap subsidy.

## Q2 — exp-019 robustness: inherited, with one honest gap

On degraded hard (unknown 0.2 + shift 0.1), stock lnd does its exp-019
collapse: success 0.493 → 0.240, give-ups +0.452, attempts falling to
3.0 because it stops paying. ilnd loses 0.049 with the CI straddling
zero and attempts RISING to 13.4 — inside the champion band (mx_c3
−0.051, hb1 −0.033). At unknown 0.3 the spread is starker: ilnd 0.531
vs lnd 0.162.

The gap: on degraded mainnet the champions lose EXACTLY zero success;
ilnd loses 0.040 (stock lnd 0.060). That is ilnd −0.036 vs mx_c3 with
the CI excluding zero — its only significant loss to a champion in
the sweep. The round-3 quarantine commit exists precisely for this
channel, which is why the re-bench's second pre-registered question is
whether this number moves toward zero.

One fingerprint worth recording: ilnd's `give_up_rate` equals
`1 − success` identically on every tier — the champion signature from
exp-017. The port carried the paradigm's style, not just its score.

## Q3 — the hybrid thesis: confirmed where it was made, refuted where the paradigm is weak

The hybrid claim (evolved beliefs + lnd's pricing) gets its first
direct test, and the budget side is clean: `fee_limit_failures` is
zero per file for both lnd and ilnd on every rung — the inherited
pruning genuinely binds — against seed 346/644, hb1 169/291, mx_c3
112/220. On the mainnet fee rungs ilnd is the best arm in the field:
0.713 at 400ppm (lnd 0.627, mx_c3 +0.113 behind) and 0.663 at 100ppm
(+0.164 over mx_c3, 9/0, p=.004). This is the exp-023 prediction
realized: exactly where the champions' lead went negative under
budgets, the hybrid restores it.

But on hard@4000ppm the hybrid inherits the paradigm's weakness, not
lnd's discipline: +0.029 over lnd with the CI straddling zero, losing
0.301 of success against its own no-budget control (hb1 −0.300, mx_c3
−0.277, lnd only −0.034) while the never-fitted seed wins the tier at
0.444. Discipline intact — zero violations — but the route choice
abandons hard payments it deems unaffordable. econ2 remains the only
router that solves that regime.

## Methodology correction, self-applied

Four mainnet identity cells failed byte-comparison — on the merge-base
binary compared against itself. Upstream lnd's `findPath` expands
predecessors by iterating a Go map (`routing/pathfind.go:1106`), and
exact cost ties on the dense mainnet graph break by map order. A
40-sample self-control shows the merge base as "novel" against its own
first group as the new binary is, with mean attempts agreeing to
±0.05. Consequence for our own record: `bit_exact` mainnet cells in
the exp-023/exp-025 gate tables were luck-dependent — the verdicts
stand (they never rested on mainnet bit-exactness; paired stats
carried them), but a future gate should compare mainnet cells
statistically, not byte-wise. Classic tiers stay byte-comparable.

## Reading

The program's central bet — the edge is the belief system, not the
paradigm's ownership of the whole route stack — survives its first
contact with lnd's actual machinery. A flag flip inside lnd now buys
+0.095 objective on real topology, the full exp-019 robustness story,
and the first router in the field that is simultaneously
champion-grade on clean tiers and lnd-grade on fee budgets (mainnet
rungs). What did not transfer: the champions' perfect zero under
degraded mainnet (quarantine pending, re-bench live), drift's last
0.019 (decay tie, exp-015's ghost), and the hard@4000 abandonment that
belongs to the paradigm itself (econ2's regime).

## Artifacts

`exp-027-isim-results-summary.json` (verdict, identity_proof, gate
blocks), `exp-027-isim-tables.txt.gz` (7 sections),
`exp-027-isim-commands.md`, `exp-027-identity-flaky.json` (the
map-iteration self-control). Branch `interval-sim` on the roasbeef
fork (merge bf1d2c6f9 + knob 04ab5ad77); raw per-run aggregates in the
session scratch under `isim/raw/`. n=10 per tier (n=8 drift/split),
sign tests paired by file.

## Addendum — round 3 re-benched: the budget price earns its keep, the quarantine does not

The re-bench merged interval-router@1bcbb1485 (clean, interval-only
files) and re-ran ONLY the interval arm over the same 14 tiers and
files, paired against the round-2 raws. Reuse gate first: 158/159
cells byte-identical across both stock binaries and the rebuilt mx_c3
overlay (the one exception is the known-flaky mainnet-lnd set, now
re-confirmed flaky at 40 samples/group — the earlier 3-sample screen
had called one cell deterministic on luck, which is the map-iteration
lesson applying to itself).

**Budget pricing: confirmed.** hard@4000 — round 2's worst result —
is the only paired round-3-vs-round-2 delta in the sweep whose CI
excludes zero: +0.079 objective, success 0.460 → 0.556,
control-relative loss 0.301 → 0.193. The tier flips from straddling
zero against lnd to +0.107 (9/0, p=.004), and now beats every
champion there (mx_c3 +0.067, hb1 +0.112, atomic1 +0.074) while still
trailing the never-fitted seed (0.410 vs 0.444, CI straddling): no
longer a loss, not yet a win. The gain is cap-insensitive (+0.072
uncapped), bought with success rather than free attempts.
`fee_limit_failures` stays 0.0 per file on every rung. The mainnet
rungs edge up (+0.016/+0.009, CIs straddling) and interval-lnd
remains the best arm in the field on all three.

**Quarantine: null, and its target gap widened.** No degraded-tier
delta reaches significance in either direction, but the
degraded-mainnet gap it was built to close moved the wrong way:
success loss 0.040 → 0.050, and ilnd−mx_c3 there is now −0.044 with
the CI excluding zero (round 2: −0.036). Unreadable failures are the
quarantine's home turf, so this is a measured verdict, not an
underpowered one: whatever buys the champions their exact zero on
degraded mainnet, it is not suspect-bound discounting. The mechanism
stays in the branch flag-gated with this null on its record; the gap
goes back on the open-questions list.

**Non-inferiority: pass, with one watch item.** Across all 14 tiers
exactly one classic-family CI excludes zero, and it is hard@4000 in
the intended direction. But ood_test moved −0.032 (CI straddling)
with success −0.029 at flat attempts, and it converts ilnd−mx_c3 on
that tier from a straddle to −0.042 CI-solid. The signature points at
the frontier's cheapest-label keep, which currently applies whether
or not a budget exists — the no-budget econ control shows the same
shape (−0.030 uncapped, attempts +1.9). Round 4's one-line
hypothesis: protect the cheapest label only when the payment carries
a fee budget, keeping the hard@4000 win while returning the
budget-less search to its validated eviction.

Bottom line after round 3: interval-lnd beats stock lnd CI-solidly on
13 of 14 tiers (degraded-mainnet the straddle), owns the mainnet fee
rungs against the entire field, and its two open edges are now
precisely localized — degraded-mainnet belief semantics, and a
label-eviction rule that should be budget-conditional.

Round-3 artifacts: `round3` block in
`exp-027-isim-results-summary.json`, `exp-027-round3-tables.txt.gz`,
commands appended to `exp-027-isim-commands.md`; merge 09b643d6f on
`interval-sim`.

## Addendum 2 — rounds 4 through 6: three falsified hypotheses and the bug underneath them

Round 3's ood regression (−0.032) took three rounds to run to ground,
and the chase is worth recording because every step falsified a
plausible story with a designed measurement.

**Hypothesis 1, the frontier rule (round 4): falsified.** Gating the
cheapest-label keep on a budget changed nothing measurable on any of
the 14 tiers — the gate was inert. The same round established that
the interval arm is not run-to-run reproducible on any binary (the
findPath map-iteration class, hit harder because the label search
holds more state); the replicate protocol that finding forced is what
made the next two rounds cheap.

**Hypothesis 2, IEEE-754 (round 5): half right.** Round 3 had
rewritten `5·fee/max(amt,1)` through a reciprocal price — equal in
exact arithmetic, different doubles on ~25% of realistic pairs, and
the frontier compares scores exactly. Restoring the verbatim
expression recovered the single-shard tiers precisely (split,
mainnet, atomic to ±0.0005) and left every tier that splits
untouched. The float story was real but could not explain the
survivors.

**The bug (rounds 5-6): a budget's remainder is not its existence.**
`intervalBudgeted` tested `feeLimit != MaxMilliSatoshi`, but lnd's
lifecycle hands the session what the limit has LEFT, recomputed every
RequestRoute — the very property round 3 leaned on to drop econ2's
redundant ledger. An unbudgeted payment carries the sentinel only on
its first request; from shard two onward it carries
`MaxMilliSatoshi − feesPaid`, is misclassified as budgeted, and gets
the clamped absolute fee price for a budget that does not exist.
Single-shard payments never reach a second request, which is exactly
the restored-vs-stuck split round 5 measured. The fix latches
budgetedness once at session construction from the payment's own
limit (`intervalFeeRate{budgeted, price}`: the latched bool picks the
branch, the live remainder still sets the price). Round 6
adjudication: 10 of 11 unbudgeted tiers return to round 2 (ood
0.5703 against a 0.5702 prediction), all three budgeted rungs
bit-identical to round 3, hard@4000 holds 0.410.

**The shipping configuration is 14/14.** The rebuilt final table at
branch tip 60cce3572: interval-lnd beats stock lnd CI-solidly on all
fourteen tiers with zero losses (4W/1L against hb1, 3W/5L against
mx_c3, owning all three fee rungs against the whole field).

**The production-default measurement.** Tracing the latch seam
surfaced that production lnd essentially never sends the unbudgeted
sentinel: `lnrpc.CalculateFeeLimit` falls back to
`DefaultRoutingFeeLimitForAmount` (100% up to 1,000 sats, then 5%),
so a real node takes the budgeted branch on every payment — and zero
of the 411 classic-corpus payments sit under the cut-off, so a
uniform 50,000 ppm reproduces production exactly. The prod-default
battery answers the upstream question directly: the margins hold on
all six classic tiers with CIs excluding zero (hard +0.261, mainnet
+0.093), the 420k msat/nat price ceiling costs at most 0.0095
anywhere (and gains on drift), and no arm refuses a single route at
the production default — mx_c3 included, which refused hundreds per
file on the exp-023 rungs. That last point reframes the exp-023/025
fee findings as tight-budget statements, not default-node ones.

**One open finding, filed with reproducing tiers.** deg_hard_mix sits
0.034 below round 2 (z = −11.1 at 32 replicates) while unknown-only,
shift-only, and unknown-at-0.3 tiers all sit at or above it — the two
attribution mechanisms together cost five times the sum of their
parts. The working hypothesis (measured interaction, unproven
mechanism): the quarantine handles failures that cannot name a
channel, and loses ground when failures name the WRONG one. Artifacts
in `exp-027-deg-mechanism-split.json`.

Round 4-6 artifacts: `round4`/`round5`/`round6`/`prod_default` blocks
and the rebuilt `final` in `exp-027-isim-results-summary.json`,
`exp-027-round6-tables.txt.gz`, `exp-027-prod-default.json`; branch
tips interval-router@60cce3572, interval-sim@fc7bfb065, both on the
roasbeef fork.
