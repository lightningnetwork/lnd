# EXP-027 — The integrated router, measured: interval-lnd is a champion in lnd's body

**Date:** 2026-07-29.
**Status:** round 2 tip (ab1c123ab) adjudicated; round 3 re-bench
(budget pricing + quarantine, 1bcbb1485) in flight as the addendum.

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
