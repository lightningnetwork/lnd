---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-27T00:00:00Z
compaction_count: 4
progress_pct: 0
current_step: 1
total_steps: 4
---

# Quick Resume: routing-evolution

## TL;DR
Execution-state session for the GEPA routing-evolution program; the
science lives in `simulation/lab/` and orientation in root CLAUDE.md.
Current phase: exp-017 (liquidity generator-family robustness sweep,
the de-circularization) with three Opus 5 background agents building
the pieces; omni adjudication (exp-018) prepped in parallel.

## Checklist
1. [x] exp-017 implementation (agents A+B, committed 817804ea5..388fc426b)
2. [x] exp-017 sweep + writeup + docs + site v58 (verdict: paradigm
       survives 13/13; hb1 >= mx_c3 on 12/13; atomic1 flat-liquidity
       specialist; pushed through 5de6a7cb3)
3. [x] exp-018 prep (agent C; runner committed d6aafd52d, gepa fix
       durable at ~/codez/gepa main 7c20d98c, both venvs refreshed)
4. [ ] Awaiting user: launch exp-018 (locks tree) or degraded
       attribution (needs tree) first   <- HERE

## Key Context
- HEAD at session-current start: 18cc7719a (exp-016 retraction).
  Uncommitted: codex xhigh effort (codex_lm.py, run_gepa_code.py),
  give_up_rate/bg_settle_rate ASI surfacing (evaluate_code.py), plus
  whatever agents A/B produce.
- USER DIRECTIVE: GEPA searcher/reflection agents run codex
  gpt-5.6-sol at high/xhigh — now the default (per-call -c override
  beats the stale harness-home config.toml).
- USER DIRECTIVE: lead orchestrates Opus 5 agents for implementation
  (simulator, gepa tweaks, notebooks, site); Fable advisor for
  design checks.
- Advisor queue: exp-017 family sweep -> distillation patch (FailAmt
  as pathfinding constraint + amount-adaptive MPP in lnd's own
  stack) -> exp-018 omni adjudication -> degraded attribution ->
  offline replay on real payment data.
- Tasks tracked in the task tool: #19 (agent A), #20 (agent B),
  #21 (sweep, blocked on 19+20), #22 (agent C).
- Tree is being EDITED by agents A/B — do not launch any code-mode
  evolution run until they finish and the sweep completes.
- Champions unchanged: hb1 + mx_c3. Dashboard at v57.

## Current Position
- Last action: exp-017 closed and published (v58), digest mailed.
  HEAD 5de6a7cb3, tree clean. Champion adjudication (hb1 vs mx_c3)
  queued as CLAUDE.md item 3.

## Open Blockers
- None.

## Resume Commands
```bash
git status
git log --oneline -3
```
