# EXP-022 — Breeding under a lying channel: run complete, sweep pending

**Date:** 2026-07-27 (run); sweep same night, 648 paired runs.
**Status:** complete — challenger failure #8, and the most
informative one yet. Champions unchanged: hb1 + mx_c3.

## Why this ran

Every evolution run before this one bred against a perfect failure
channel, and exp-019 showed the champions survive degradation only
because they treat no-information as no-information — machinery that
ignores the lie, not machinery that exploits it. This run is the first
to breed with the lie present: corpus-deg is the sealed corpus-mix
train/val with the exp-019 realistic mix stamped on every file
(`unknown_prob 0.2, shift_prob 0.1`), test left clean so the held-out
line reads transfer back to a truthful channel. The background prompt
gained a `--degraded` section (2eeec117a) stating the channel facts and
posing the open question: nobody has evolved machinery that actively
exploits a lying channel.

## Run facts

400-eval budget (384 used), pure gepa, codex gpt-5.6-sol at high/900s,
36 iterations, 10 candidates in the pool, no reflection hijacks. The
launch gate held: the in-tree seed reproduced exp-011's iteration-0
val score to full float precision on the clean split before anything
counted.

| | degraded val | clean held-out test |
|---|---|---|
| seed | 0.3906 | 0.5082 |
| best (pool #5, 1,113 lines) | **0.4343** | **0.4988** |

Two numbers, two stories. On the degraded world it was bred for, the
winner gains +0.044 — comparable to what evolution buys on clean
corpora at this budget. On the clean held-out test it lands 0.009
BELOW its own seed: the degradation machinery is not free when the
channel stops lying. Which of those dominates on the sealed tiers is
exactly what the sweep decides.

## What it evolved (source audit, exploit-grep clean)

The interesting part, and the reason this run existed. The winner is
the first candidate in the program to build attribution-confidence
machinery rather than ignoring what it cannot read:

1. **Quarantined suspect evidence.** Per-directed-channel
   `suspectAmt`/`suspectWeight` fields hold observations whose
   attribution the router does not trust, kept apart from the hard
   lowerOK/upperFail bounds. A suspect entry is cleared when later
   evidence contradicts it instead of poisoning the interval. This is
   one of the three designs the prompt posed as open questions, built
   without being shown any implementation.
2. **Payment-local penalties for unreadable failures.** On an unknown
   failure it penalizes every traversed edge within the payment
   (0.16 per edge, softening to 0.11 on long routes) and writes
   nothing to shared beliefs — convergent with the champions' session
   penalties and with the soft_unknown patch's design logic, evolved
   independently under the pressure that produced the lnd pathology.
3. **An escalation threshold.** After four unknown failures in one
   payment it changes policy rather than looping — the guardrail class
   omni1 conspicuously lacked.

## Pre-registered sweep (to run when the tree is free post-restart)

Standard three-way champion protocol, nothing bespoke: build the
candidate via overlay, run the sealed tiers (hard-test, ood-test,
mainnet, drift, split, atomic) in BOTH channel conditions — clean, and
degraded at the exp-019 realistic mix — against seed, hb1, mx_c3 and
atomic1. Paired per-file deltas, bootstrap CIs, two-sided sign tests.
Success and attempts read separately on every arm; on degraded corpora
low attempts is as likely the give-up attractor as efficiency
(the exp-013 rule, doubly binding here). Verdict criteria: champion
displacement requires beating a champion with CIs excluding zero on
the tier family it claims; a degraded-only win with clean-tier losses
files it as a degraded-channel specialist alongside atomic1's
flat-liquidity niche.

The clean-test regression (−0.009) is the number to beat expectations
on: if it holds across clean tiers, the honest headline is that
robustness machinery has a price, which converges with exp-021's
attempts-for-success trade from the other direction.

## Verdict (the sweep ran exactly as pre-registered above)

Gates first: all five prior routers rebuilt from HEAD reproduce the
exp-020 clean numbers exactly (24/24 cells to four decimals), and the
degraded hard arm reproduces exp-019's realistic-mix level to four
decimals for every prior router — the corpus, evaluator and
degradation instrument are the published ones. 648/648 runs, zero
errors, determinism double-checked.

**No displacement.** deg1 beats a champion with a CI excluding zero on
zero tiers, in either channel condition. Its best cell (atomic_test
degraded, +0.043/+0.041 over the champions) straddles zero. Going the
other way it loses to mx_c3 with CIs excluding zero on four tiers,
unanimously on split (0/8, p=.008) and mainnet (0/10, p=.002, −0.11
against all three incumbents) — and on mainnet it is the first evolved
router in the program to land BELOW production lnd (0.679 vs 0.694).
It does not earn a specialist filing either: atomic1's niche wins were
CI-solid; deg1's degraded edges are not.

**What it was bred for, it achieved — measurably.** deg1 is the most
degradation-robust router ever measured here: its degraded−clean
deltas are −0.013 to +0.000 across all six tiers (the champions lose
up to 0.067 with CIs excluding zero; lnd loses 0.25 of success on
hard). The champion gap narrows under the lying channel exactly as
the breeding predicted, with CIs excluding zero on hard_test (+0.051
vs hb1, +0.056 vs mx_c3, p=.002).

**And the mechanism is the finding.** The flatness is bought by never
stopping: 26 to 92 attempts per payment on every tier, pinned past
the objective's 15-extra-attempt cap everywhere, failing almost
never by giving up (it breaks the give_up_rate identity — the
harness's 200-attempt ceiling abandons for it). Re-scoring the same
raw runs at higher caps shows the subsidy plainly: the champions are
cap-insensitive to four decimals, while deg1's one directional lead
inverts at cap 30 and lands worst-in-field uncapped. This is the
inverse of the exp-013 give-up attractor — omni1's no-guardrail shape
reached by a different road — and it is the third independent line of
evidence (with exp-018 and exp-021) that the champions' edge is
plan-time: at this budget the search bought robustness with unbounded
retrying, not with better plans.

## Consequences

1. Challenger ledger: failure #8. Champions unchanged.
2. The suspect-bound machinery (quarantine, payment-local penalties,
   escalation caps) is real, novel and worth the idea ledger — it is
   the first evolved answer to the attribution question, and its
   degradation-flatness is genuine. A future run could seed FROM
   mx_c3 with the degraded corpus to test whether the machinery
   composes with a plan-time architecture instead of replacing it.
3. **The attempt cap is now a measured objective weakness, not a
   suspicion.** It silently subsidized this candidate the way it hid
   soft_unknown's cost in exp-021. The exp-023 fee-term rule (a cost
   term must stay below the abandonment price) has an attempt-side
   sibling: a capped cost term creates a free direction past the cap.
   Any future objective revision should treat the two symmetrically.

## Artifacts

`exp-022-deg1-best-candidate.go` (pool #5, matched byte-for-byte
against the runner's final selection), `exp-022-deg1-summary.json`,
`exp-022-deg1-run.log.gz`, `exp-022-results-summary.json` (the sweep:
gates, both arms, paired stats, cap-sensitivity re-scoring).
corpus-deg regenerates from the sealed
`simulation/lab/scenarios/corpus-mix/` plus the one-field attribution
stanza documented in its README.
