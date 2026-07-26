---
id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
status: active
task_ids: []
created_at: 2026-07-24T20:38:07Z
updated_at: 2026-07-25T18:07:32Z
compaction_count: 1
git_branch: gepa
git_last_commit: 127c34067
---

# Session: GEPA LN Routing Evolution — Execution State

## TL;DR (Read This First)
This session tracks EXECUTION state only (live runs, agents, crons,
uncommitted work). The science — results, experiments, decisions,
ideas — lives in `simulation/lab/` (NOTEBOOK.md, DECISIONS.md,
IDEAS.md, experiments/) and orientation lives in the repo-root
CLAUDE.md. Read those first; this file is the "what was in flight"
layer they deliberately don't cover.

**Progress**: ongoing research program (no fixed step count)
**Current**: code_gen2 evolution run at 113/400 evals; Opus site
redesign agent live
**Blocker**: None

## Context
**Objective**: Evolve next-gen LN routing algorithms with GEPA against
the in-process simulator; validate champions three-way; keep the
command center + lab notebook current.

**Starting State**:
- Branch: gepa @ 127c34067 (all research artifacts committed)
- 5 uncommitted files: design agent's in-flight command-center edits +
  data/run.json — commit only AFTER the redesign agent lands.

**Key Files**:
- `simulation/lab/NOTEBOOK.md` — chronological log (read first)
- `CLAUDE.md` — orientation, headline results, gotchas
- `simulation/run_gepa_code.py`, `simulation/evaluate_code.py`
- `simulation/champions/` — hb1, hb2, mx_c3

**Key Context** (survives compaction):
- Scratch dir (venv, corpora, /tmp/routesim, run outputs):
  `/private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/scratchpad`
  — WIPED by reboots; everything regenerates from fixed seeds.
- Live run: `code_gen2` (pure gepa, --no-adaptive, corpus-mix,
  400 evals, small seed + insights prompt). Check:
  `ps aux | grep run_gepa`; outputs at `<scratch>/outputs/code_gen2`,
  run dir `<scratch>/runs/code_gen2`.
- NEVER edit `routing/` or `cmd/routesim/` while code_gen2 is live
  (evaluate_code.py recompiles from the tree). This blocks exp-008
  (batch-2 sim fidelity) and exp-010 (splitting pressure) until it ends.
- Opus design agent (id aff92136516d3cef3): full command-center
  de-slop redesign + findings.html; owns simulation/command-center/*;
  publishes to Litbucket itself. Do NOT touch those files or publish
  over it. Reach it via SendMessage.
- Dashboard cron (~30min): DATA-ONLY refresh while redesign is in
  flight (export_run.py → data/run.json; no index.html edits, no
  Litbucket publish). Resume full refresh_dashboard.sh flow after.
- Mail watcher: re-arm each wake —
  `substrate watch --session-id 8563fa98-1f3e-4c15-8b0f-7223a827b9a2`
  (run_in_background).
- summary.json best_score (0.977 currently) is INFLATED per-minibatch;
  champions decided only by held-out three-way validation (sealed hard
  test + OOD corpus-v2 + mainnet ~/codez/data/mainnet_graph.json)
  vs lnd + seed + hb1 + mx_c3, after exploit-grep
  (GraphSession|LocalBalances|AssignLiquidity|unsafe|reflect).

## Progress
### Completed
- All research through exp-009 committed (see NOTEBOOK.md);
  CLAUDE.md (b28da5f59) + DECISIONS.md (127c34067) written.
- Design agent LANDED: full de-slop redesign + findings.html,
  Litbucket v30 published, committed d13a376e5 (also fixed
  refresh_dashboard.sh to bundle findings.html). Dashboard cron
  restored to full export+publish flow (job 8650972a, session-only).

- code_gen2 DONE (400/400, clean exit) + champion sweep DONE: gen2
  reaches champion level (combined 0.638 vs mx_c3 0.652) but does not
  pass — champions of record remain hb1 + mx_c3. Writeup exp-011;
  candidate archived in lab/experiments/. Dashboard cron DELETED (run
  over, v35 has final data).

- exp-008 STARTED (user green-lit via mail): virtual clock +
  background traffic built/tested/committed (d11a20dcb); drift corpus
  at <scratch>/corpus-drift (gen_scenarios --hard --drift --seed 3031);
  baseline swept (drift-baseline.json): champions hold up under drift
  (~0.46 vs lnd 0.20 on drift-test), lnd decay doesn't close the gap.
  Writeup: lab/experiments/exp-008-drift-evolution.md (baseline done).

- code_drift1 DONE (400/400, 51 accepts) + sweep DONE: exp-008 verdict
  is time-awareness RE-EVOLVED (35min confidence half-life, 20min
  bound expiry, conf-weighted prior interpolation) but LOSES to the
  time-less champions even on drift (0.417 vs 0.455-0.457; gen2 at
  0.456 without ever seeing drift). Champions unchanged. Writeup
  complete; winner archived as
  lab/experiments/exp-008-drift1-best-candidate.go. Cron deleted.

### MORNING STATE (user awake; pre-compaction checkpoint)
- exp-010 codex arm COMPLETE + verdict committed (cda40a3dc): joint
  planning emerged, lost to mx_c3 everywhere (paired stats). Champions
  unchanged. Winner archived in lab/experiments/.
- code_split_opus1 (A/B arm) LIVE at 67/400, resumed-from-state after
  the ClaudeLM pipe-hang fix (75081d251); reflections take minutes;
  laptop sleep mimics stalls — judge by run_log.txt freshness. Tree
  (routing/, cmd/routesim/) FROZEN while it lives.
- code_split_opusmed1 LIVE (launched 2026-07-25 ~11:25): third arm,
  claude:claude-opus-5:medium (new effort knob in ClaudeLM), same
  corpus/budget/seed. Hypothesis: cheaper reflections + more
  iterations beats deliberate reflections at fixed eval budget. Log
  <scratch>/code_split_opusmed1.log. Isolation verified (11/11
  proposals clean on opus1; smoke test exact-pong on medium).
- KNOWN LEAK in both live Opus arms (pre-fix code): user-level Stop
  hook fires in claude -p, model's hook reply replaces the artifact —
  ~3-4% of iterations lost on BOTH arms (symmetric, A/B stays fair;
  opus1 iter 19, opusmed1 iters 43-44, all rejected by marker check).
  Fixed for future runs via sterile CLAUDE_CONFIG_DIR (f27bd470a).
  Mention in the A/B verdict writeup.
- LIMIT INCIDENT (2026-07-25 ~14:30): user's API limit hit; opus1's
  last ~11 iterations degraded to stub reflections (~33 evals wasted),
  opusmed1 lost 1. Both runs "completed" early. User added funds;
  both arms RESUMED from gepa_state (~16:18) with topped-up budgets
  (opus1 435, opusmed1 405) to refund the stub waste. Resumed procs
  run the NEW ClaudeLM (hook-sealed) — tail iterations are clean.
  Pre-resume snapshot: opus1 held-out 0.841 (beats codex-arm 0.810!),
  opusmed1 0.743 despite best-in-family val 0.874 (val overfit?).
- EXP-010B STARTED (2026-07-25 evening, user green-lit): atomic MPP
  sim change LANDED (d0f062747, Opus agent + my review): hold-and-
  release shards, held-liquidity contention, prorated traffic on
  attempt boundaries; flag-off byte-identity verified. Pre-registered
  design + --atomic corpus flag in f7c3b2e0c. Corpus:
  <scratch>/corpus-splitatomic (--split --split-leads 5 --atomic,
  seed 6061). All 7 binaries rebuilt against new tree. Baseline sweep
  on atomic_val/atomic_test running (out: atomic-baseline.json).
  Tempering rule pre-registered: recalibrate churn params only on
  baseline evidence, never after evolution results. Next: read
  baseline (criterion 1 = ordering change), then launch evolution.
- EXP-010 CLOSED (2026-07-25 ~17:15): both Opus arms completed their
  topped-up budgets (no new accepts post-resume). Five-tier sweep done
  (opus-ab-validation.json in scratch): opus1 TIES mx_c3 on split-val
  (+0.005, first ever) but craters off-corpus (hard 0.303); opusmed1
  val-overfit (best val 0.874, worst held-out 0.743). Champions
  UNCHANGED. Winners archived + verdict written into exp-010 +
  NOTEBOOK. Remaining: docs agent + site agent (fanning out now),
  final dashboard export+publish, mail user. Then tree UNFREEZES —
  exp-012 (warmup/cold-cache) and exp-010b become runnable.
- Advisor program applied: measurement-ceiling reframe; harness
  overhaul (554c79cc7, 754894b14); gepa clone patched on branch
  fix-claude-json-array (7c20d98c) — UPSTREAM IT; corpus-splitv2 +
  multi-vantage mainnet scenarios generated in scratch.
- PENDING DECISIONS (user): let opus1 run to 400 vs call A/B at
  matched eval counts; when to run exp-012 / exp-010b.
- PENDING WORK: docs+site Opus agents after A/B closes (standing
  directive); final dashboard export+publish for exp-010; exp-012
  warmup feature (tree unfreeze first); fresh never-seen corpus at
  writeup time (test-set-reuse red flag); hourly health cron 9ecd4545
  still active; mail watcher may need re-arming after compaction.

### OVERNIGHT (user asleep, full autonomy, Opus fan-out approved)
- exp-010 history: corridors corpus committed (11f4ccc65), baseline
  swept (mx_c3 0.876 = bar), code_split1 KILLED (codex hijacked by
  ~/.codex AGENTS.md+memories — 70% of reflections were "Watcher
  armed"), CodexLM hardened (preamble+marker 2b5d84b66, CODEX_HOME
  isolation 6a964d309), relaunched clean as code_split2.
- [ ] code_split2 LIVE (~71+/400, log <scratch>/code_split2.log,
      0 hijacks) — cron cdc5983d watches. Tree FROZEN.  <- CURRENT
- [ ] On completion: exploit-grep, sweep (sweep_split.py has all six;
      add split2 binary), structural readout (joint planning vs
      ladder?), exp-010 verdict, commit/push, mail; then Opus docs
      agent + Opus site agent for the verdict.
- [ ] Then exp-012: warmup_payments runner phase (routing/ edit — only
      after split2 ends), tests, cold/warm/staleness measurements,
      writeup. Design in IDEAS.md.
- UNBLOCKED + LAUNCHED: code_split_opus1 (Opus 5 reflection via
      ClaudeLM, token auth from ~/codez/.claude-harness-token, NEVER
      print that token; redaction in c41a52aef). Same corpus/budget as
      code_split2 → first clean reflection-model A/B. Log
      <scratch>/code_split_opus1.log.
- STANDING DIRECTIVE (user, before bed night 2): on each breakthrough,
      fan out Opus 5 agents to update the champion docs and the site.
      Dashboard publish cron cancelled by user — do final
      export+publish manually per verdict. Hourly health-check cron
      active (no publishing).

## Decisions
See `simulation/lab/DECISIONS.md` — the durable decision log lives
there, not here.

## Discoveries
See `simulation/lab/NOTEBOOK.md` and `simulation/lab/experiments/`.

## Blockers
- None currently

## Next Steps
1. Monitor code_gen2 (`ps aux | grep run_gepa`; eval count via
   `ls <scratch>/outputs/code_gen2/evals | wc -l`).
2. When the design agent completes, review its report, verify the site,
   commit `simulation/command-center/`, confirm Litbucket URL.
3. When code_gen2 completes, run the standard champion sweep.

## Files Modified This Session
- simulation/command-center/* (design agent, in flight, uncommitted)

## Resume Commands
```bash
git status
ps aux | grep -E "run_gepa|http.server 8777" | grep -v grep
SCRATCH=/private/tmp/claude-501/-Users-roasbeef-gocode-src-github-com-lightningnetwork-lnd-gepa/8563fa98-1f3e-4c15-8b0f-7223a827b9a2/scratchpad
ls "$SCRATCH/outputs/code_gen2/evals" | wc -l   # eval progress
```
