# Lab Notebook — LN Routing Evolution

Running log of the GEPA × lnd pathfinding project. Newest entries at the
bottom. Detailed experiment writeups live in `experiments/`.

---

## 2026-07-24 — Project kickoff, evaluator built, loop validated

**Goal set by roasbeef:** create the next generation of LN routing
algorithm, using a payment simulator (improved as far as needed) and GEPA
as the guide. Explicitly *not* tied to the current Dijkstra + mission
control paradigm. Dijkstrasden (bitromortac's virtual LN, PR #3) reviewed
and mined for ideas — describegraph-loaded graphs, liquidity models,
observer traces, scenario sweeps — but not adopted as code: its portal
architecture (patched lnd, gossip injection, itests) is built for
implementation testing, too heavy and nondeterministic for an optimizer's
inner loop.

**Built today:**
- `routing/sim_*.go` simulator: real lnd pathfinding + mission control
  against hidden liquidity, per-direction policies, real forwarding error
  semantics. Caught an off-by-one porting the mock graph: the amount over
  channel i is `Hops[i-1].AmtToForward`, not `Hops[i]` — until fixed,
  every intermediate hop failed with FeeInsufficient.
- `cmd/routesim` CLI (~0.3s per 10-payment batch on 200-node nets).
- Scenario corpus generator, evaluator, GEPA runners (parameter mode and
  code mode), codex-CLI reflection wrapper (gpt-5.6-sol headless).
- `SimRouter` paradigm-free interface + candidate slot with overlay
  builds; seed = simple cheapest-path router with failure blacklisting.

**Experiments:** see `experiments/exp-001-param-smoke.md` (smoke run,
budget starvation diagnosed) and `experiments/exp-002-param-run1.md`
(400-eval parameter run, in flight).

**Headline early finding:** on a hard bimodal-liquidity example, the
*naive* 300-line seed router beat the full lnd stack — 33% vs 22% success
at 7.8 vs 112.6 attempts/payment. The lnd defaults burn huge retry budgets
in bimodal small-channel regimes. Detail in
`experiments/exp-003-seed-router-vs-lnd.md`.

**Infra notes:** PyPI `gepa` 0.1.4 lags the repo API — install from git
main. GEPA's budget arithmetic: each candidate costs ~(minibatch +
periodic full-val) evals; 60 evals only bought 7 proposals on the smoke
corpus. Score shaping matters: unbounded attempt penalties (down to −2)
drowned success-rate deltas; penalties now saturate at −0.25 total.

---

## 2026-07-24 (night) — run1 verdict, corpus v2, overnight evolution runs

- **run1 (params, 400 evals): seed survived.** No knob change beat lnd
  defaults on val aggregate; bimodal specialists won individual examples
  only. Paradigm > knobs (exp-002). Real lineage now on the dashboard;
  Litbucket v2 published (https://lnd-routing-command-center.lightning.wiki/).
- **Corpus v2** adds Barabási-Albert scale-free nets (800/1500 nodes,
  log-normal capacities). Baseline: lnd near-parity on scale-free
  (0.819 vs 0.860) but still behind overall (0.559 vs 0.681) — exp-003.
- **Overnight runs in flight:** code1 (adaptive gepa↔meta_harness,
  corpus v1, 240-eval pool) and omni1 (blog recipe via optimize_best_of:
  gepa+meta_harness explore 100 evals each → fresh gepa continues 160,
  corpus v2). Preflight passed; reflection = codex/gpt-5.6-sol,
  meta_harness = claude CLI.
- **Farmed out:** Fable code-reviewer auditing the simulator's BOLT
  forwarding semantics + reward-hack surfaces (report-only, results land
  in a later entry). Cron agent refreshes the dashboard every 30 min.

## 2026-07-24 (night) — BREAKTHROUGH: evolved router beats lnd + seed

`code_hard1` (pure gepa, gpt-5.6-sol) evolved an 872-line router that
**wins the sealed test set and generalizes OOD**:
- hard sealed test: hb1 0.586 > seed 0.530 > lnd 0.309 (objective)
- OOD corpus-v2 test: hb1 0.545 > seed 0.487 > lnd 0.357
- ~9 attempts/payment vs lnd's ~50, higher success on both.

What it discovered from failure traces alone: an explicit **bimodal
liquidity prior** (rediscovering lnd's own bimodal-estimator hypothesis)
+ per-edge liquidity bounds with confidence + risk-adjusted Dijkstra.
Clean (no exploit). Full detail + caveats: exp-006. Champion saved to
`champions/router_hb1_v1.go`. Sim audit (exp-005) fixed a critical
sandbox escape before it was exploited.

**Update (code_mix1 follow-up):** continuing evolution from hb1 on a
mixed corpus produced **mx_c3** (`champions/router_mx3_generalist_v1.go`),
the best generalist: it dominates hb2, ties hb1 on the hard test (0.583
vs 0.586), wins OOD (0.581 vs 0.545), and has the best combined average
(0.582). Champions of record: hb1 (hard specialist) + mx_c3 (generalist).
Detail: exp-007.

## 2026-07-24 (day) — MAINNET VALIDATION: champions win on the real graph

Real 12,161-node mainnet snapshot, 100 payments, highest-degree source:
mx_c3 0.791 / hb1 0.790 vs lnd 0.694 objective — comparable success
(0.81 vs 0.79) at **8.6× fewer attempts** (2.3 vs 19.8/payment). The
synthetic-bred champions generalize to lnd's home turf. exp-009.

## 2026-07-24 (evening) — code_gen2: insight transfer works, same ceiling

The small-seed + insights-in-prompt run finished (400/400 evals, 31
accepts — ~4× code_mix1's acceptance rate). Its best candidate reaches
champion-class performance on all three held-out tiers (combined 0.638
vs mx_c3's 0.652, hb1's 0.640; mainnet 0.787 at the same 2.3
attempts/payment) but does NOT pass the champions. Three independent
lineages now converge on the same paradigm and the same performance
band: the interval-belief design looks like a local optimum *for these
environments*. gen2 did evolve two novel mechanisms (in-flight local
liquidity reservation; weakest-edge failure attribution) that the sim
never rewards — more eval budget in the same regime buys nothing.
Champions of record stay hb1 + mx_c3; the next lever is changing the
environment (exp-008 background traffic, exp-010 splitting pressure).
Detail: exp-011.
