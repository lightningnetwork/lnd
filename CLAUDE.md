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
  dropped mission control and ALL time-decay; invented
  per-directed-channel liquidity intervals (lowerOK/upperFail bounds +
  evidence-count confidence) — that's where the 8.6× attempt reduction
  comes from. **Correction (WHY.md §0):** we long claimed they
  "rediscovered the bimodal prior from failure traces." They did not —
  the harness prompt has stated the bimodal hypothesis since the first
  committed version. What was NOT supplied: the prior's functional
  shape and constants, and the entire interval apparatus. Worse, the
  evolved constants FIT OUR GENERATOR (`sim_liquidity.go` draws
  `ExpFloat64()*0.05`; atomic1's low mode is `exp(−x/0.055)`), and the
  mainnet tier overwrites real balances with that same generator — so
  the mainnet number is real topology and policies, synthetic
  liquidity. Fixing that is the top pre-upstream task.

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
  notebook, experiments, IDEAS backlog. `simulation/lab/WHY.md` — the
  flagship explainer: each evolved mechanism paired against the lnd
  production code that it replaces, plus the corrections to our own
  published claims (the bimodal prior WAS in the harness prompt; the
  evolved priors fit `sim_liquidity.go`'s 5%-of-capacity generator;
  lnd's decay never fires on the static tiers).
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
- The `claude` binary is a wrapper spawning the real CLI as a
  grandchild sharing the stdout pipe: subprocess timeouts kill only
  the wrapper then block forever reading the pipe. ClaudeLM uses
  process-group kill (75081d251); reflection failures degrade to a
  stub proposal, never a run-killing exception. Nested claude -p needs
  CLAUDE_CODE_OAUTH_TOKEN (harness reads ~/codez/.claude-harness-token,
  0600 — NEVER print it; error text is token-redacted). --system-prompt
  strips CLAUDE.md/memories but NOT user-level hooks, whose feedback
  reaches the model even in -p mode (one Opus reflection came back
  discussing the mail-watcher Stop hook instead of emitting a router).
  ClaudeLM therefore also sets a sterile
  CLAUDE_CONFIG_DIR=~/codez/claude-harness-home (f27bd470a).
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

Chronology and detail live in `simulation/lab/NOTEBOOK.md`; the
mechanism-by-mechanism comparison against lnd's production code, and
the corrections to our own published claims, live in
`simulation/lab/WHY.md`. This section is only what is live and what is
next.

### Live
- **exp-013 `code_hybrid1`**: continuation evolution seeded from
  atomic1 (the exp-010b no-collapse hybrid) on corpus-mix, 400 evals,
  codex proposer — the recipe that turned hb1 into mx_c3, applied to
  the strongest challenger yet. TREE IS FROZEN while it runs (no
  `routing/` or `cmd/routesim/` edits). Log
  `<scratch>/code_hybrid1.log`; canary must stay 0.

### Closed since exp-011 (champions UNCHANGED throughout: hb1 + mx_c3)
- **exp-008** — drift. Time-decay re-evolved and lost to time-less
  champions even on drift. Caveat added later: our churn is weak (see
  the traffic defect below), so this is a statement about weak churn.
- **exp-010** — splitting pressure, three proposer lineages. All three
  evolved joint route-set planning; none beat mx_c3. opus1 scored the
  program's first statistical tie on any tier (split-val) then
  collapsed off-corpus — traced afterwards to ONE overfit constant
  (`maxRouteHops = 7`), not its architecture.
- **exp-010b** — atomic MPP arena (hold-and-release shards, held
  liquidity contention, per-attempt drift). The subsidy was real: lnd
  fell from second place to LAST at 105 attempts/payment once probing
  stopped being free. Codex's winner `atomic1` is the first challenger
  with NO collapse tier (even with mx_c3 on hard/OOD/mainnet, 1.6
  att/pmt on mainnet — program record) but loses held-out atomic-test.
  Proposer A/B FLIPPED vs exp-010: deliberate large-step reflection
  wins in low-noise environments, misfires in churn-noisy ones.
- **exp-012** — cold cache / hot load. No hot-cache regime exists here:
  across depletion, staleness, foreign vantage and valid probes, at
  25/100/400 observations, warming never lifts any router above its
  cold score and mission control never approaches the champions.
  Champions are cheap from payment ONE, so their edge is a PRIOR not a
  history. Under stale knowledge the consumer's staleness policy
  dominates: lnd thrashes, champions abandon (hard `upperFail` zero),
  atomic1 shrugs (0.012 floor) — and atomic1 beats mx_c3 there by
  +0.233/+0.428 (p=.002), the first significant win over a champion.
  Vantage: remote-pair observations transfer, own-local-channel ones
  are actively harmful to import.
- **exp-002b** — the knob WHY.md said we never turned. lnd's own
  bimodal estimator at seven scales, including the environment-matched
  one, beats neither lnd's apriori default nor the champions. It raises
  success while DOUBLING attempts: a better prior makes lnd keep
  trying without changing WHAT it retries. Estimator worth ≤0.02 of
  objective; paradigm worth 0.18–0.22.

### Next, in priority order
1. **Traffic engine defect (do first; needs `routing/`).** Only ~18% of
   background payments settle, and a failed payment moves no
   liquidity, so our exogenous process is ~5× weaker than configured.
   This caveats exp-008 and exp-010b. Make traffic settle (size
   amounts to available liquidity, or retry), and aim a share of it at
   the corridors under test.
2. **`--import-weights` (needs `routing/`).** exp-012 cannot construct
   free knowledge because every arm buys it with payments. Injecting
   beliefs from a file with no payments sent is the only design that
   isolates imported knowledge from the price of acquiring it, and it
   is what the proposed API actually does.
3. **Fix the circular mainnet liquidity.** `sim_liquidity.go` draws
   `ExpFloat64()*0.05` and the evolved priors fit that constant; the
   mainnet tier overwrites real balances with the same generator. Until
   liquidity comes from somewhere we did not write, the mainnet number
   is real topology and policies with synthetic balances. TOP
   pre-upstream fix.
4. **Degraded attribution** — the advisor's decisive pre-upstream test.
   Our failure channel is instant, truthful and exactly attributed;
   mainnet's is not. The 8.6× is an upper bound until this runs.
5. Re-run staleness and exp-008's decay question underneath the fixed
   traffic engine.
6. Upstream the gepa meta_harness JSON fix (`~/codez/gepa` branch
   `fix-claude-json-array`).
