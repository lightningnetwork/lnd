# EXP-032 — The wider corpus: margins hold, magnitudes humble

**Date:** 2026-07-31.
**Status:** complete. The last simulator experiment before replay
week. The release candidate's lead over lnd re-pins CI-solid at n=30
on files nobody had seen; the exp-030 point estimates do not, and the
record is corrected accordingly.

## Design

Thirty fresh files per tier from the hard-family generator at seed
32032 (five attribution variants pairing byte-for-byte with the clean
control, verified 30/30 each) plus thirty degraded-mainnet files.
Three arms: stock lnd, the RC tip (interval-sim@dcde83cca, committed
V3), mx_c3. Framing caveat first: the sealed hard tier is
unreproducible (exp-020), and this fresh corpus is a HARDER world
(lnd 0.136 vs the sealed 0.309) — only contrasts transfer between
corpora, never levels.

## Q1 — the RC's margins hold at n=30, larger than the sealed tiers showed

Six of seven tiers CI-solid over lnd: hard_clean +0.230, hard_unk20
+0.261, hard_shift10 +0.164, hard_mix +0.238, hard_unk30 +0.246,
mn_clean +0.096 (p from 8e-06 to 2e-03). The exp-019 collapse
reproduces on unseen files: unknown 0.2 takes lnd's success 0.298 →
0.138 with attempts 61 → 3.0 while the RC holds 0.537 → 0.540. Every
lead survives uncapping; on unknown tiers it SHRINKS uncapped because
part of lnd's measured loss there is an attempt collapse the cap was
hiding — the leads are success, not subsidy.

The seventh tier, mn_mix, is a composite artifact worth its own
paragraph: lnd's OBJECTIVE rises +0.063 under degradation while its
success falls and give-ups climb 0.397 → 0.470 — the attempt-cap
term paying lnd for quitting, the third independent sighting of the
abandonment subsidy (exp-013, exp-022, here). Success columns rank
mx_c3 0.557 > RC 0.530 = lnd 0.530 on that tier.

## Q2 — the exp-030 magnitudes do NOT re-pin, and the record now says so

On thirty fresh mix files the original bug measures −0.0099
[−0.0325,+0.0118] and the V3 fix +0.0034 [−0.0185,+0.0241] — both
indistinguishable from zero. The 0.034/0.044 point estimates were a
property of the ten sealed files, not of the hard family; the
interval arm's per-file spread (objective range up to 0.29 on
hard_mix) is why n=10 magnitudes were fragile. What SURVIVES is what
the mechanism always rested on: the same-file ablation and the
ground-truth counters (67 hard bounds on never-failed channels under
the mix, 0 under unknown-only). The V3 give-back on unknown-only
tiers stays insignificant at n=30, which was the question that
mattered for keeping the fix. **The PR quotes the mechanism and the
counters, not the deltas.** One incidental CI-solid effect: rounds
3-6 net −0.0015 on the clean control (7/30 files) — real, negligible,
recorded.

## Q3 — the degraded-mainnet deficit halves and loses significance

Round 3's −0.044 CI-solid becomes −0.0208 [−0.0424,+0.0012] at n=30,
signs 13/12. Directionally intact, no longer CI-solid, still the
largest deficit in the sweep — and the underlying success story is
clean: mx_c3 is the only arm whose success does not move at all
under degraded mainnet (0.557 → 0.557; lnd −0.013, RC −0.033).
Whatever buys the champion its exact zero there remains unidentified
(exp-030 ruled OUT suspect discounting) and is the one open deficit
going into replay week.

## Closing state of the simulator program

Thirty-two experiments. The RC beats stock lnd CI-solid on every
fresh-file tier that measures information handling; its two known
soft spots (degraded-mainnet success, hard@4000 vs the seed) are
localized, directional, and on the record; the objective's
abandonment subsidy is a thrice-measured instrument error to carry
into any future scoring. Replay on real payment data is the next and
final external-validity step.

## Artifacts

`exp-032-results-summary.json`, `exp-032-tables.txt.gz`,
`exp-032-commands.md`. Generators and seeds in the summary
(hard seed 32032; mainnet generator script recorded); corpora
regenerate deterministically.
