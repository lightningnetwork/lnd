# CLAUDE.md — lnd-gepa (branch: gepa)

Experimental lnd fork: evolving the next generation of LN routing
algorithms with GEPA (LLM-driven reflective evolutionary search) against
an in-process payment simulator. NOT tied to the current Dijkstra +
mission-control paradigm — whole routing algorithms are the candidates.

Read `simulation/lab/NOTEBOOK.md` first for the full story; experiment
writeups live in `simulation/lab/experiments/` (exp-001…exp-009).

## Headline results (all validated, held-out, reproducible)

| router | mainnet obj | mainnet att/pmt | notes |
|---|---|---|---|
| lnd production stack | 0.694 | 19.8 | defaults; baseline |
| hand-written seed | 0.762 | 6.1 | ~300-line interval router |
| hb1 (evolved) | 0.790 | 2.3 | hard-regime specialist |
| **mx_c3 (evolved)** | **0.791** | **2.3** | generalist champion |

- Mainnet = real 12,161-node describegraph snapshot
  (`~/codez/data/mainnet_graph.json`), 100 payments (exp-009). Same
  ordering holds on the sealed synthetic test and OOD corpora
  (exp-006/007). Objective = success − 0.01·min(extra_attempts,15) −
  0.00002·min(fee_ppm,5000).
- Parameter tuning alone could NOT beat lnd's defaults (exp-002): the
  paradigm is the lever, not the knobs.
- What the champions evolved (all pure Go, `simulation/champions/`):
  dropped mission control and ALL time-decay; rediscovered the bimodal
  liquidity prior from failure traces; invented per-directed-channel
  liquidity intervals (lowerOK/upperFail bounds + evidence-count
  confidence) — that's where the 8.6× attempt reduction comes from.

## Map

- `routing/sim_*.go` — simulator: hidden liquidity, real forwarding
  checks, seeded topologies/liquidity, describegraph loader, and the
  paradigm-free `SimRouter` interface (candidates own route selection
  AND MPP splitting; sealed gossip view — candidates cannot reach
  hidden balances).
- `cmd/routesim` — CLI evaluator. `--router=lnd|candidate`;
  `candidate_impl.go` is the swappable slot replaced per candidate via
  `go build -overlay`.
- `simulation/*.py` — GEPA harness: `gen_scenarios.py` (fixed seeds →
  corpora regenerate identically), `evaluate.py`/`evaluate_code.py`,
  `run_gepa.py`/`run_gepa_code.py`/`run_gepa_omni.py`, `codex_lm.py`
  (reflection via `codex exec`, model gpt-5.6-sol), `export_run.py`,
  `preflight.py`, `refresh_dashboard.sh`.
- `simulation/champions/` — evolved winners. `simulation/lab/` —
  notebook, experiments, IDEAS backlog.
- `simulation/command-center/` — dashboard. Local:
  `python3 -m http.server 8777` from that dir. Published:
  https://lnd-routing-command-center.lightning.wiki/ (Litbucket, team
  labs, slug lnd-routing-command-center; publish via
  `refresh_dashboard.sh <run-name> <scratch-dir>`).

## Running things

```bash
go build -o /tmp/routesim ./cmd/routesim
python3 simulation/gen_scenarios.py --out /tmp/corpus [--hard]
# gepa: install from the durable clone with uv:
uv venv <dir> && uv pip install -p <dir> "$HOME/codez/gepa[full]"
python3 simulation/preflight.py   # before long runs
```

Evolution runs need `codex` CLI authed + OPENAI_API_KEY. Session
scratch dirs (`/private/tmp/claude-*/...`) hold venv/corpora/binaries
and are WIPED by reboots — everything there is regenerable (fixed
seeds); durable artifacts live in the repo, `~/codez/gepa` (gepa
source), `~/codez/data/mainnet_graph.json`.

## Gotchas (hard-won)

- **Never edit `routing/` or `cmd/routesim/` while a code-mode run is
  live** — evaluate_code.py recompiles from the tree every eval.
- `summary.json`'s `best_score` is an inflated per-minibatch metric.
  Champions are decided ONLY by held-out three-way validation (compile
  candidate via overlay, score on sealed test + OOD sets vs lnd + seed).
- Grep every candidate for `GraphSession|LocalBalances|AssignLiquidity|
  unsafe|reflect` before trusting it (sandbox was sealed in exp-005;
  keep verifying).
- Code evolution hits a complexity wall past ~800 lines; giant-seed
  reflection is slow. Prefer small seed + insights in the background
  prompt (this is `code_gen2`'s design, and it accepts candidates much
  faster).
- zsh: `"$VAR[full]"` is subscript expansion — write `"${VAR}[full]"`.
- The default codex home (~/.codex) injects the user's global
  AGENTS.md AND accumulated memories into every session — the
  reflection model obeyed those ("Watcher armed.") instead of the
  task, even for "reply with exactly: pong". The project-doc config
  knobs don't help (the leak is the user layer, not project docs).
  CodexLM runs with CODEX_HOME=~/codez/codex-harness-home (auth
  symlinked, memories off, no instruction files; 6a964d309) plus a
  role-pinning preamble + require_marker retry (2b5d84b66). Check any
  new run's log early: `grep "Proposed new text" <log> | grep -ci
  watcher` should be 0.

## Open work

- DONE: exp-008 (task #13). Sim gained a virtual clock + background
  traffic (d11a20dcb). Verdict: time-awareness DID re-evolve
  (confidence half-life 35min, bound expiry 20min, conf·learned +
  (1−conf)·prior interpolation) but does NOT beat the time-less
  champions even on drift (drift1 0.417 vs mx_c3 0.457 on drift-test;
  gen2, which never saw drift, scores 0.456 there). Evidence bounds
  degrade gracefully; decay buys nothing at realistic churn. Champions
  unchanged, now validated on four tiers.
- **exp-010:** MPP splitting pressure — champions evolved "halving-plus"
  (evidence-derived shard ladder); joint route-set planning
  (Pickhardt-style min-cost flow) remains unevolved.
- Batch-2 sim fidelity: live first-hop hints per attempt, lnd-vs-
  candidate memory symmetry, virtual MC clock.
- DONE: dashboard de-slop redesign + findings.html (Litbucket v30+,
  commit d13a376e5) and the `code_gen2` run (exp-011: insight transfer
  reaches champion level in 400 evals but plateaus at the same ceiling
  — three lineages now converge on the interval-belief paradigm;
  champions of record remain hb1 + mx_c3). The conclusion: more evals
  in the current environment buy nothing; change the environment
  (exp-008/exp-010).
