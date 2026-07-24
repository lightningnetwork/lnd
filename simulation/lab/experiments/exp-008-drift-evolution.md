# EXP-008 — Drift: does time-awareness re-evolve under background traffic?

**Date:** 2026-07-24 (started)
**Status:** in flight — baseline done, evolution run `code_drift1` live

## Question
The champions carry zero time-based logic, and we suspected that was
partly a simulator artifact: hidden liquidity only moved when OUR
payments moved it, so evidence never went stale and hard bounds were
unbeatable by construction. With a virtual clock and exogenous
background traffic (commit d11a20dcb), knowledge now decays for real.
Does time-awareness re-evolve? Candidate outcomes, all informative:
(a) decay re-emerges → validates lnd's rationale with evolved
constants; (b) something better emerges (e.g. interval-widening with
elapsed time); (c) intervals still win → decay was overweighted.

## Environment
`corpus-drift` (seed 3031): hard topologies + bimodal liquidity, ten
virtual minutes between payments, one second per attempt, background
senders per gap scaled to network size (≥10, num_nodes/10), amounts
log-uniform from ~dust to half a channel. lnd's mission control runs
on the virtual clock, so its decay half-lives genuinely operate. The
reflection prompt describes drift neutrally and flags the hard-bounds
insight as learned in a static world — we do not prescribe the answer.

## Baseline (before evolution)

| router | drift-val obj | drift-test obj | test succ | test att |
|---|---|---|---|---|
| lnd stack | 0.213 | 0.203 | 0.388 | 34.5 |
| seed | 0.320 | 0.377 | 0.592 | 48.3 |
| hb1 | **0.387** | 0.455 | 0.642 | 11.8 |
| mx_c3 | 0.380 | **0.457** | 0.642 | 12.3 |
| gen2 | 0.383 | 0.456 | 0.642 | 12.7 |

Two findings already:
- **The champions' hard bounds do NOT collapse under drift.** They
  still beat lnd by >2× objective at a third of the attempts. Stale
  bounds degrade gracefully — a wrong lowerOK just costs a retry.
- **lnd's decay does not close the gap.** Even with its half-lives
  finally operating over meaningful time spans, the production stack
  stays last. Decay as lnd implements it is not the missing
  ingredient; whatever drift-awareness helps here has to look
  different.
- Everyone lost ground vs the static hard corpus (champions ~0.59 →
  ~0.42 combined val/test; attempts up from ~9-10 to ~12): drift
  creates genuine headroom for the evolution run to claim.

## Evolution run
`code_drift1`: pure gepa, codex/gpt-5.6-sol reflection, small seed +
insights prompt (with the drift paragraph and the static-world caveat
on hard bounds), 400 evals, corpus-drift. Success criterion: beat the
champions on held-out drift-test; then inspect whether the winner
encodes any function of view.Now() or evidence age.

## Verdict
(pending run completion)
