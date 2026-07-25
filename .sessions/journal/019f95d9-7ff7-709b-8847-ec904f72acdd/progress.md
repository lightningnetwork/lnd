---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-25T18:07:32Z
compaction_count: 1
progress_pct: 0
current_step: 1
total_steps: 4
---

# Quick Resume: routing-evolution

## TL;DR
Execution-state session for the GEPA routing-evolution program; the
science lives in `simulation/lab/` and orientation in root CLAUDE.md.
Currently shepherding code_gen2 (113/400 evals) and the Opus site
redesign agent.

## Checklist
1. [ ] code_gen2 run completes (113/400)      <- HERE
2. [ ] Design agent lands: verify site, commit command-center, Litbucket
3. [ ] code_gen2 champion sweep (exploit-grep + three-way validation)
4. [ ] Unblocked follow-ups: exp-008, exp-010

## Key Context
- Scratch (wiped by reboot, regenerable):
  /private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/scratchpad
- Do NOT edit routing/ or cmd/routesim/ while code_gen2 is live.
- Design agent aff92136516d3cef3 owns simulation/command-center/*;
  dashboard cron is data-only until it lands.
- Champions decided ONLY by held-out three-way validation, never
  summary.json best_score.

## Current Position
- File: (monitoring, no active edits)
- Last action: Session initialized post-compaction; code_gen2 at
  113/400 evals; design agent mid screenshot-iterate loop.

## Open Blockers
None

## Resume Commands
```bash
git status
ps aux | grep -E "run_gepa|http.server 8777" | grep -v grep
```
