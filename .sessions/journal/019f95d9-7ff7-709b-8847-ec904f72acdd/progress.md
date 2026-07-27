---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-26T21:55:00Z
compaction_count: 3
progress_pct: 100
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
1. [x] exp-013 `code_hybrid1` completed (352 evals, 30 iterations)
2. [x] Exploit-grep clean, overlay-compiled, six-tier paired sweep run
3. [x] Verdict NEGATIVE: below mx_c3 on all six tiers and below its
       own seed on gepa's held-out test. No champion swap. Written up
       in exp-013-hybrid-continuation.md, pushed at 8360a3829.
4. [ ] Next: traffic-engine fix (tree is now free)   <- HERE

## Key Context
- Scratch (wiped by reboot, regenerable from fixed seeds):
  /private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/scratchpad
- exp-013 closed 17:22 on 2026-07-26. Its lesson: a seed already at
  the attempt frontier makes continuation evolution walk toward
  giving up, not toward quality.
- TREE IS FREE — no code-mode run holds `routing/`. All four
  pre-upstream tasks in CLAUDE.md are unblocked.
- The exp-012 multivantage mainnet set is NOT a valid champion tier
  (identical 0.227 success for every router). Use scen-mainnet.json.
- `summary.json`'s `best_score` is an inflated per-minibatch metric
  (currently 0.996 — meaningless). Champions are decided ONLY by
  held-out paired validation.
- Closed and written up this session: exp-010b, exp-012, exp-002b.
  Docs reconciled through commit 201724c80; dashboard at v55.

## Current Position
- File: (monitoring, no active edits)
- Last action: exp-013 closed negative, written up, pushed, digest mailed.

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
