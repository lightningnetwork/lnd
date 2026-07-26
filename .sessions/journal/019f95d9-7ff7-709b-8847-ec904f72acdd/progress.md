---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-26T21:55:00Z
compaction_count: 3
progress_pct: 40
current_step: 1
total_steps: 3
---

# Quick Resume: routing-evolution

## TL;DR
Execution-state session for the GEPA routing-evolution program; the
science lives in `simulation/lab/` and orientation in root CLAUDE.md.
Everything is committed and pushed; the only thing in flight is
exp-013 (`code_hybrid1`), a continuation evolution seeded from
atomic1. Monitor it to verdict, then validate.

## Checklist
1. [ ] exp-013 `code_hybrid1` reaches 400 evals   <- HERE
2. [ ] On completion: exploit-grep the winner, overlay-compile,
       held-out paired sweep vs mx_c3 (+ hb1, atomic1, lnd)
3. [ ] If and only if it wins the paired sweep: champion swap,
       writeup, docs fan-out, dashboard publish

## Key Context
- Scratch (wiped by reboot, regenerable from fixed seeds):
  /private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/scratchpad
- exp-013: started 12:22 on 2026-07-26, ~64 evals/hour, log
  `<scratch>/code_hybrid1.log`, outputs `<scratch>/outputs/code_hybrid1`.
  Canary and stub counts both 0 as of 161/400.
- TREE IS FROZEN while it runs: no `routing/` or `cmd/routesim/`
  edits (evaluate_code.py recompiles the tree every eval). This
  blocks all four of CLAUDE.md's top pre-upstream tasks.
- `summary.json`'s `best_score` is an inflated per-minibatch metric
  (currently 0.996 — meaningless). Champions are decided ONLY by
  held-out paired validation.
- Closed and written up this session: exp-010b, exp-012, exp-002b.
  Docs reconciled through commit 201724c80; dashboard at v55.

## Current Position
- File: (monitoring, no active edits)
- Last action: post-compaction resume; exp-013 at 161/400, healthy.

## Open Blockers
- None. The four next-priority experiments all need `routing/`, so
  they wait on exp-013 rather than being blocked in the usual sense.

## Resume Commands
```bash
git status
ps aux | grep "[r]un_gepa"
ls <scratch>/outputs/code_hybrid1/evals | wc -l
grep -c "reflection unavailable" <scratch>/runs/code_hybrid1/run_log.txt
```
