# EXP-025 — Evolution in the economic world: the fee-budget specialist

**Date:** 2026-07-28/29 (two runs, one sweep).
**Status:** complete. Champions unchanged (challenger #9); econ2 files
as the program's second specialist, and it carries two firsts.

## The runs

The first attempt (code_econ1) died an instructive death: 59 of 59
proposals scored zero, all on one API confusion. The prompt described
inbound-fee semantics without the Go type, so 53 proposals reached for
the reflect package to duck-type an imagined Option wrapper (the
exp-005 sandbox rejected every one, an accidental live demonstration
that the seal works) and six guessed UnwrapOr on a plain struct and
failed to compile. Eight prompt lines fixed it (b1533b5db): state the
type, show the field reads, name the ban. The lesson generalizes to
every future harness prompt: describe data without its type and a
code-writing model invents an API.

The relaunch (code_econ2, 382 evals, same corpus and seeds) accepted
nine candidates. The winner is 1,230 lines, exploit-grep clean,
byte-matched against the runner's final selection.

## What evolution built, given a world where money is real

The winner is the first candidate in the program to touch either
economic contract surface: it reads `spec.FeeLimitMsat` and prices
inbound fees, both of which every incumbent (champions included)
ignores entirely. And the machinery is planning, not re-weighting: a
remaining-budget ledger net of settled shards, per-shard budget
allocation that decrements as shards commit, fee-cap pruning inside
the Dijkstra, a budget-derived fee/reliability exchange rate, and an
8-label Pareto search over (score, fee, amount) that never evicts the
minimum-fee label, so a cheap feasible path stays in the frontier
when the budget binds. Around it: the exp-018 dual-ledger idea
realized (process-global soft beliefs vs a payment-local strict upper
bound), re-invented confidence time-decay, policy failures kept out
of liquidity beliefs, and exp-021-style soft handling of unattributed
failures. One defect, caught by the sweep's counters: it computes the
inbound fee on the incoming amount where the wire charges it on the
outgoing amount plus the outbound fee, so it underpays positive
surcharges by a few hundred msat and eats 13 refusals per file on the
heavy tier where lnd eats zero.

## The verdict (1,524 runs, gates bit-exact, 21 tiers, 6 arms)

**Champion displacement: no.** econ2 loses the classic sealed set to
both champions (hb1 4L/1W, mx_c3 4L/0W) and two to three of the four
economic control tiers. Its worst tier is drift (−0.17 vs mx_c3,
below even the seed): the composition world is low-drift by
construction, so it never bred against staleness — the corpus README
pre-registered exactly this risk.

**Specialist filing: yes, CI-solid.** On the fee-budget tiers it
beats hb1 4/4, mx_c3 3/4 and atomic1 3/4 with CIs excluding zero,
unanimously on mainnet at 100 ppm (10/0, p=.002 against both
champions) — and the mandatory attempt-cap re-scoring shows NO
subsidy on those tiers (the leads survive uncapped; its econ_test
headline margin, by contrast, is 56-73% cap subsidy and is filed with
that label). H-C2 confirmed in the strong form: zero budget
violations on every budgeted tier, matching lnd exactly, where every
other evolved router violates constantly.

**Beat lnd on the economic world: yes — a program first.** The
corpus was the first on which lnd outscored the hand-written seed, so
the bar was live, and econ2 clears it: +0.135 on econ val (p=.004),
+0.048 on the held-out test (16/4, p=.012), and all four fee-budget
rungs with CIs excluding zero. Sharpest form: exp-023 found the
champions' clean-mainnet lead goes NEGATIVE at every fee rung; econ2
restores an lnd-beating lead exactly there (0.721 vs lnd's 0.627 at
400 ppm, best of all six routers).

## Reading

exp-023 said the champions' edge is informational and their weakness
is pricing. One evolution run in the priced world produced a router
that keeps the informational machinery (interval beliefs, soft
evidence) AND does the arithmetic (budget-pruned search) — beating
lnd where the champions cannot — while giving back ground in the
old worlds, the exp-022 trade shape again but milder and this time
with cap-robust wins where it was bred. The frontier is now three
regimes deep: hb1/mx_c3 own the clean informational worlds, atomic1
the flat/atomic/contention niches, econ2 the fee-budget regime. No
single router owns everything, and the interval-router lnd branch
remains the hybrid the data keeps pointing at: evolved beliefs on
top of lnd's pricing — to which econ2 now contributes the concrete
suggestion that the pricing side wants budget-aware search, not just
budget-aware filtering.

## Caveats

n=10 per tier (20 on contention); the econ_test cap subsidy is
stated above; the inbound-fee defect means the heavy-tier numbers
understate a corrected variant; one gate pass was interrupted by the
/tmp daily cleaner removing the mainnet graph mid-run at date
rollover (errors are never cached; re-run filled cleanly, and the
graph should live somewhere less ephemeral for future sweeps).

## Artifacts

`exp-025-econ2-best-candidate.go`, `exp-025-econ2-summary.json`,
`exp-025-econ2-run.log.gz`, `exp-025-econ1-failed-run.log.gz` (the
postmortem record), `exp-025-results-summary.json` (gates, all 21
tier tables, hypothesis and verdict blocks),
`exp-025-sweep-tables.txt.gz`, `exp-025-sweep-commands.md`.
