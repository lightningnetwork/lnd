---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-27T09:30:00Z
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
4. [ ] Site v60 (exp-019 + 8.6x sweep) — agent working; then commit,
       publish, mail combined digest   <- HERE
5. [ ] exp-018 verdict when run completes (overnight)

## Key Context
- **exp-018 LIVE**: pid in scratch, log <scratch>/exp018.log, workdir
  <scratch>/exp018-work (runs/, outputs/ per engine), venv-omni,
  corpus-mix, engines gepa,meta_harness,autoresearch, 150 evals each,
  sequential arms. Check: ps aux | grep run_gepa_omni; eval count:
  ls <scratch>/exp018-work/outputs/exp018_<engine>/evals | wc -l.
  Watcher-leak check on meta/autoresearch arms when they start:
  grep -ci watcher on their run logs should be 0.
- **TREE LOCKED** while exp-018 runs: no edits to routing/ or
  cmd/routesim/. All tree work landed before launch (d47283c34).
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
