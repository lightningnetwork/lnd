---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-27T20:29:02Z
compaction_count: 4
progress_pct: 85
current_step: 4
total_steps: 5
---

# Quick Resume: routing-evolution

## TL;DR
Execution-state session; science in simulation/lab/. Autonomous run
(user granted full autonomy 2026-07-27): exp-017 (de-circularization
PASSED), exp-020 (mx_c3 DEFENDS title), exp-019 (8.6x retired, edge
converts to success under degraded attribution) all closed same day.
exp-018 omni adjudication is LIVE overnight.

## Checklist
1. [x] exp-017 family sweep: paradigm survives 13/13 (v58)
2. [x] exp-020 championship adjudication: mx_c3 defends; sealed
       hard/OOD tiers checked into repo (v59)
3. [x] exp-019 degraded attribution: champions hold, lnd give-up
       spiral from 10% unreadable, 8.6x retired (commit 7282858f6)
4. [x] Site v60/v61 published; digests mailed
5. [x] exp-018 closed: gepa only engine to produce anything; omni1 =
       challenger failure #6; band not a gepa artifact at practical
       budgets. exp-019b: anomaly hard-tier-only, story refuted.
       ALL OVERNIGHT WORK COMPLETE
6. [x] exp-021 distillation: soft_unknown PR-READY (86-148%
       recovery, unanimous direction, inert when off);
       adaptive_split a genuine null after 3 designs -> champions'
       edge is plan-time architecture. v62 published, HEAD 68fe3e150.
7. [ ] Mail #8738 directives (2026-07-27): THREE agents live:
       (a) DONE incl. gap-fill: interval-router at 13 commits on
       roasbeef fork (SQL persistence w/ restore floor, pair-scope
       attribution, itest PASSES locally, docs/interval_routing.md);
       remaining: Postgres CI, own flush flag, blinded fallback;
       (b) DONE: simulation/lab/DISTILLATION.md reviewed, committed
       35c04ebef, pushed to origin + roasbeef;
       (c) code_deg1 LIVE (pid 30636, launched 2026-07-27): breed
       under degraded attribution, corpus-deg = corpus-mix +
       unknown=0.2/shift=0.1 train+val, test clean. Log
       SCRATCH/code_deg1.log, runs/outputs SCRATCH/{runs,outputs}/
       code_deg1. Iter-0 gate passed (0.3906 as predicted). TREE
       LOCKED (routing/, cmd/routesim/) until done. Commits pushed:
       35c04ebef (DISTILLATION.md), 2eeec117a (--degraded flag),
       faee68f57 (corpus-mix train/val sealed, NOT regenerable).
       <- CURRENT. On completion: exploit-grep, three-way sweep
       degraded+clean vs champions, read success/attempts separately.
       Ext order after: econ realism spec, replay (needs node).
8. [x] ceiling1 DONE -> exp-024 (ed353108f): meta_harness at 10x
       converges below gepa at 1x; band is neither gepa artifact nor
       starvation; challenger failure #7. exp-023 spec committed
       (a1e779915) with lead decisions.
9. [x] code_deg1 DONE -> exp-022 harvest (06f14513d): first evolved
       attribution-confidence machinery (suspect bounds, payment-
       local unknown penalties, escalation cap). +0.044 degraded,
       -0.009 clean test. VERDICT OPEN.
       <- NEXT SESSION: (1) exp-022 pre-registered sweep (corpora
       from sealed scenarios; ~1h); (2) exp-023 stage A impl (tree
       FREE, no runs live); (3) dashboard v63 when verdicts land.
       interval-router branch at 14 commits on roasbeef fork.
       SAFE-TO-RESTART mailed; scratch fully harvested.

## Key Context
- exp-018 DONE (was live): pid in scratch, log <scratch>/exp018.log, workdir
  <scratch>/exp018-work (runs/, outputs/ per engine), venv-omni,
  corpus-mix, engines gepa,meta_harness,autoresearch, 150 evals each,
  sequential arms. Check: ps aux | grep run_gepa_omni; eval count:
  ls <scratch>/exp018-work/outputs/exp018_<engine>/evals | wc -l.
  Watcher-leak check on meta/autoresearch arms when they start:
  grep -ci watcher on their run logs should be 0.
- Tree FREE. Next up per CLAUDE.md: distillation patch (top),
  offline replay, 10x ceiling arm, upstream gepa fix.
- HEAD at last checkpoint: d9fbba193, everything pushed. Dashboard
  v59 live; v60 pending site agent.
- exp-019 anomaly queued as CLAUDE.md item 3: shift-isolated mainnet
  arm before publishing any mechanism for shift-helps-lnd.
- Champions: hb1 + mx_c3 (title defended). Task list: #25 exp-018
  in_progress; all others completed.

## Current Position
- Last action: exp-018 launched (gepa arm iterating), site agent
  spawned for exp-019/v60.

## Open Blockers
- None.

## Resume Commands
```bash
git status && git log --oneline -5
ps aux | grep "[r]un_gepa_omni"
tail -20 <scratch>/exp018.log
```
