# EXP-022 — Breeding under a lying channel: run complete, sweep pending

**Date:** 2026-07-27 (run); validation sweep pending, pre-registered
below.
**Status:** run harvested, verdict OPEN. Champions unchanged until the
sweep says otherwise.

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

## Artifacts

`exp-022-deg1-best-candidate.go` (pool #5, matched byte-for-byte
against the runner's final selection), `exp-022-deg1-summary.json`,
`exp-022-deg1-run.log.gz`. corpus-deg regenerates from the sealed
`simulation/lab/scenarios/corpus-mix/` plus the one-field attribution
stanza documented in its README.
