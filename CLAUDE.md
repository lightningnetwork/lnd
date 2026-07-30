# CLAUDE.md — lnd-gepa (branch: gepa)

Experimental lnd fork: evolving the next generation of LN routing
algorithms with GEPA (LLM-driven reflective evolutionary search) against
an in-process payment simulator. NOT tied to the current Dijkstra +
mission-control paradigm — whole routing algorithms are the candidates.

Read `simulation/lab/NOTEBOOK.md` first for the full story; experiment
writeups live in `simulation/lab/experiments/` (exp-001…exp-027).

## Headline results (all validated, held-out, reproducible)

| router | mainnet obj | mainnet att/pmt | notes |
|---|---|---|---|
| lnd production stack | 0.694 | 19.8 | defaults; baseline |
| hand-written seed | 0.762 | 6.1 | ~300-line interval router |
| hb1 (evolved) | 0.790 | 2.3 | sharp-bimodal specialist; edges over mx_c3 are family-specific only (exp-020) |
| **mx_c3 (evolved)** | **0.791** | **2.3** | generalist champion — title DEFENDED (exp-020: split_test 8/0 p=.008; hb1 wins nothing on the original set) |
| atomic1 (evolved) | 0.790 | 1.6 | flat-liquidity specialist (exp-017 ladder rank 4→1), attempt record, beats a champion under staleness |
| **interval-lnd (integrated)** | **0.788** | **2.5** | the interval-router branch running INSIDE lnd's payment lifecycle (exp-027): champions' margin on all six tiers, best arm in the field on mainnet fee rungs, exp-019 robustness inherited |

- Mainnet = real 12,161-node describegraph snapshot
  (`~/codez/data/mainnet_graph.json`), 100 payments (exp-009). Same
  ordering holds on the sealed synthetic test and OOD corpora
  (exp-006/007). Objective = success − 0.01·min(extra_attempts,15) −
  0.00002·min(fee_ppm,5000).
- The champions lead all six held-out tiers against lnd, and nine
  challengers have failed to displace them (exp-010 x3, 010b, 013,
  018, 024, 022, 025). The frontier is three regimes deep: hb1/mx_c3
  own the clean informational worlds, atomic1 the atomic/contention
  niches, econ2 the fee-budget regime (and only econ2 beats lnd
  there).
  The twin question is settled (exp-020): two significant hb1 signals
  (exp-015 p=.014, exp-017 liq-uniform p=.004) did NOT replicate on
  the original tier set — hb1 beats mx_c3 nowhere there, while mx_c3
  takes split_test unanimously (+0.062, 8/0, p=.008), the one tier
  where hb1 cannot even beat lnd. The twins differ only at the edges:
  hb1 on some new synthetic families, mx_c3 exactly where payments
  must fragment.
- Parameter tuning alone could NOT beat lnd's defaults (exp-002), and
  neither can lnd's own bimodal estimator at any of seven scales,
  including the one matching this environment (exp-002b). The paradigm
  is worth 0.18–0.22 of objective; the estimator at most 0.02.
- **What a weight-serving API should serve (exp-016).** Hand three
  routers the same third-party observations, free: atomic1 +0.055
  (p=.016), mx_c3 +0.031 with attempts 8.1→4.4, lnd −0.029 with
  attempts going UP. Successes help everyone; FAILURES are the whole of
  lnd's loss (−0.039, CI excludes zero, worse on 9/10 files). The
  damage scales with the VOLUME of imported bounds, not their
  staleness: each blocks one edge at the amount the consumer is about
  to send, and lnd's only response is to route around. The interval
  routers turn the identical removals into instructions (upperFail X →
  try (X−1)/k). Serve observations, not weights.
- What the champions evolved (all pure Go, `simulation/champions/`):
  dropped mission control and ALL time-decay; invented
  per-directed-channel liquidity intervals (lowerOK/upperFail bounds +
  evidence-count confidence) — that's where the attempt reduction
  comes from. **The 8.6× framing is RETIRED (exp-019):** it was a
  perfect-channel artifact — under realistic attribution degradation
  lnd uses FEWER attempts than the champions because it stops paying
  for hard payments. The durable claim: on degraded mainnet the
  champions hold success at exactly their undegraded values while lnd
  loses 6 points of success and doubles its give-ups — the edge
  converts from attempts to success. **Correction (WHY.md §0):** we long claimed they
  "rediscovered the bimodal prior from failure traces." They did not —
  the harness prompt has stated the bimodal hypothesis since the first
  committed version. What was NOT supplied: the prior's functional
  shape and constants, and the entire interval apparatus. Worse, the
  evolved constants FIT OUR GENERATOR (`sim_liquidity.go` draws
  `ExpFloat64()*0.05`; atomic1's low mode is `exp(−x/0.055)`), and the
  mainnet tier overwrites real balances with that same generator — so
  the mainnet number is real topology and policies, synthetic
  liquidity.
- **The generator-family question is CLOSED (exp-017).** Thirteen
  paired tiers moving the liquidity family (wrong bimodal scales, beta
  with polynomial tails, beta:2:2 where the bimodal hypothesis is
  false, uniform, degree-correlated hubdrain), the amount family, and
  the mainnet balances: lnd rank 5 on 13/13, an evolved router rank 1
  on 13/13, hb1−lnd CI excludes zero on 12/13. Margins compress on
  easy worlds, but the never-fitted seed compresses with the same
  shape — a difficulty ceiling, not memorized constants. The paradigm
  wins, not the fitted priors. What stays authored: every world is
  still one we chose, so the full escape remains degraded attribution
  and offline replay on real payment data.

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
seeds) — EXCEPT the sealed validation tiers: exp-020 found hard-test
and OOD are NOT regenerable from any committed generator (they came
from an uncommitted working copy), and scratch's copy of the sealed
hard tier had been silently overwritten by exp-010. The sealed tiers
now live in `simulation/lab/scenarios/` (hard-test, ood-test,
mainnet); drift/split/atomic regenerate (seeds 3031/4041/6061).
Durable artifacts live in the repo, `~/codez/gepa` (gepa source),
`~/codez/data/mainnet_graph.json`.

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
- **Check where a seed sits on the attempt axis before continuing
  it.** exp-013 continued atomic1, which was already at the attempt
  frontier, and the search found the only cheap direction left: give
  up on hard payments. The composite objective hides abandonment
  inside the same number as efficiency, so read success and attempts
  separately on every candidate.
- The exp-012 multivantage mainnet set is NOT a champion tier: every
  router scores an identical 0.227 success on it (reachability is
  fixed by the graph at low-degree vantages), so it scores an
  attempt-cost contest and reports it as objective. Use the exp-009
  hub scenario (`scen-mainnet.json`) for validation.
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
- **code_full2** — compose world seeded FROM econ2 (400 evals). TREE
  LOCKED (`routing/`, `cmd/routesim/`) until it exits.
- **exp-027 round-3 re-bench** — isim agent re-running the ilnd arm
  at interval-router@1bcbb1485 (budget pricing + quarantine), paired
  against the round-2 raws.

### Closed since exp-011 (champions UNCHANGED throughout: hb1 + mx_c3)
- **exp-027** — the integration benchmark. interval-router@ab1c123ab
  merged into the sim tree, `router_impl=interval` knob on the lnd
  arm (mission control still fed; lifecycle seams mirrored), 104/104
  byte-identity off, gates 24/24 vs exp-023, 804-run battery. The
  flag flip pays the champions' margin on ALL six classic tiers
  (mainnet 0.788 vs lnd 0.694 vs mx_c3 0.791; attempts 2.5 vs 2.3),
  indistinguishable from mx_c3 on 5/6, takes split off hb1. exp-019
  robustness inherited (degraded hard −0.049 inside the champion
  band; lnd −0.253). Hybrid thesis CONFIRMED on mainnet fee rungs
  (best arm in field, zero budget violations both rungs) and REFUTED
  on hard@4000 (inherits the paradigm's abandonment; econ2 keeps that
  regime). One gap: degraded mainnet −0.040 success where champions
  lose exactly 0.000 (the round-3 quarantine's target — re-bench
  live). Methodology self-correction: mainnet cells were NEVER
  byte-reproducible on any binary (findPath map iteration), so
  bit-exact mainnet gate cells in exp-023/025 were luck; paired stats
  carried those verdicts, future gates read mainnet statistically.
- **exp-026** — the compose world holds. First breed under economics
  AND the lying channel together: an honest defeat (8 pool accepts,
  all attempting the full budget+inbound+reservations synthesis,
  three with suspect bounds; zero broken-proposal artifacts) that
  returned the SEED after the full budget. The difficulty ladder is
  now monotone to zero: clean +0.05, degraded +0.044, econ +0.022,
  compose +0.000 at identical optimizer/budget/seed. Pre-registered
  escapes: seed from econ2 (watch the exp-013 give-up direction),
  or 800 evals. No challenger produced; ledger stays at nine.
- **exp-025** — evolution in the economic world. First run died on an
  API confusion (59/59 proposals; 53 used reflect around an imagined
  Option, sandbox caught every one; prompt fix: state the TYPE).
  Relaunch produced econ2: the FIRST candidate to read
  spec.FeeLimitMsat or price inbound fees — budget-pruned Dijkstra
  with a never-evicted min-fee Pareto label, per-shard budget
  allocation, dual belief ledger (exp-018 idea realized). Verdict:
  challenger #9, filed as the FEE-BUDGET SPECIALIST — CI-solid over
  all champions on the fee rungs, cap-robust, zero budget violations
  (matches lnd exactly; every other evolved router violates), and
  the program's first beat-lnd-on-a-live-bar (econ world, all fee
  rungs, restores the lead exactly where champions go negative). But
  loses the classic set and drift badly (never bred vs staleness).
  Frontier now three regimes: hb1/mx_c3 (informational), atomic1
  (atomic/contention), econ2 (fee budgets). One defect on record:
  inbound fee computed on the wrong base, refusals on surcharges.
- **exp-023** — economic realism, full cycle in one day. Five
  flag-gated mechanisms (min/max HTLC, inbound fees, fee budgets,
  concurrency via deterministic virtual-time event loop, latency),
  each byte-identical off; 1,920-run sweep, gates bit-exact. Verdict:
  the champions' edge is INFORMATIONAL, not pricing — fee budgets
  close the gap unanimously on mainnet, heavy inbound fees erase the
  lead, htlc/concurrency/latency move nothing (latency-alone: five
  routers byte-identical). Validates the interval-router hybrid
  (evolved beliefs + lnd pricing). atomic1 audit: its fee robustness
  is a UNITS choice (absolute msat pricing = implicit tightening ppm
  ceiling). Incidentals: mainnet never byte-reproducible (lnd map
  iteration; accept+caveat), 41% of fees uncounted pre-stage-C,
  attempt-cap subsidy now measured twice (exp-022, econ_test).
- **exp-022** — the first breed under a lying channel (corpus-mix +
  exp-019 realistic mix on train/val, --degraded prompt). The winner
  evolved the program's first attribution-confidence machinery
  (quarantined suspect bounds, payment-local unknown penalties,
  escalation caps) and is the most degradation-robust router ever
  measured (degraded−clean deltas −0.013..+0.000 on all six tiers;
  champion gap narrows under degradation, hard_test CI excludes
  zero). Still challenger failure #8: zero CI-solid wins over
  champions in either condition, unanimous losses on split/mainnet,
  first evolved router BELOW lnd on mainnet (0.679). Mechanism: it
  never stops (26-92 att/pmt, pinned past the objective's attempt
  cap; breaks the give_up_rate identity — the harness ceiling
  abandons for it). Cap-sensitivity re-scoring: champions
  cap-insensitive, deg1 worst-in-field uncapped — the attempt cap is
  now a MEASURED objective weakness (sibling of the exp-023 fee-term
  rule). Third independent evidence the champions' edge is plan-time.
- **exp-024** — the ceiling arm. meta_harness at 10x evals (1,496,
  $15.68, 157m) iterates and improves for the first time (8 iters,
  five new-bests) but CONVERGES by iteration 3 at 0.4677 val / 0.5136
  test — below gepa's result at one tenth the budget (0.5102/0.5565),
  with the last 950 evals buying +0.0002. Both halves of the exp-018
  question are now closed: the band is not a gepa artifact and not
  budget starvation. gepa's eval-efficiency moat compounds with scale
  (68 evals per full-set benchmark bought 22 candidate evaluations
  from 1,500). log_bimodal_cost = challenger failure #7. Remaining
  escape hatches are environment changes (exp-023), not optimizers.
- **exp-021** — the distillation patch (flag-gated, in-tree at
  9c07cbe7f, byte-identical off). soft_unknown (single-pair penalty
  replacing the unreadable-failure route nuke) recovers 86-148% of
  the exp-019 collapse on hard/drift, success up and give-ups down
  unanimously, exact-identical on clean controls; buys success with
  attempts (+18-29, capped out of the objective); mainnet inert.
  UPSTREAMABLE. adaptive_split is a genuine null after three designs
  each reduced to geometric bound-descent, which lnd's halving
  already does fastest and free — with exp-002b this kills both
  halves of the reactive distillation theory. The champions' edge is
  plan-time (success-side memory + joint route-set construction): an
  architectural price, not a patch.
- **exp-018** — the omni adjudication. gepa vs meta_harness vs
  autoresearch, identical seed/corpus/150-eval budget. gepa alone
  produced anything (13 iterations, a real candidate); meta_harness's
  full-set benchmarking bought ONE iteration and returned the seed;
  autoresearch burned its budget in 13 minutes and returned the seed.
  At practical budgets the ~0.64 band is NOT a gepa artifact — the
  alternatives cannot reach the starting line; whether it is a true
  ceiling needs meta_harness at ~10x evals (costed: ~$2 +
  19min/swing). The candidate omni1 is challenger failure #6: beats
  no champion, no collapse tier, the inverse of the give-up attractor
  (most attempts everywhere — it evolved no attempt/hop/search caps).
  Idea ledger: dual belief ledgers (own-shard contention vs standing
  balance) and contradiction-triggered confidence decay. Searcher
  defaults retuned to high/900s after xhigh lost 4/13 reflections to
  timeouts.
- **exp-019** — degraded attribution, the decisive pre-upstream test.
  Ladder over unknown/shift/delay on the sealed hard tier, mainnet,
  and drift (520 paired runs; controls reproduce exp-020 exactly).
  The champions survive the realistic channel — none of them writes a
  bound from an unattributed failure, so hard-tier margins WIDEN
  under unreadable errors. lnd collapses: processPaymentOutcomeUnknown
  penalizes the whole route both directions, so 10% unreadable errors
  drive give-ups 0.31→0.71 and 30% pins files to zero success — a
  self-contained upstream finding (third input to the distillation
  patch). Anomaly shipped as anomaly: shift=0.3 HELPS lnd (+0.122,
  10/10, p=.002) — and exp-019b bounded it same-night: hard-tier-only
  (mainnet CIs straddle zero, sign flips), the route-geometry story
  refuted by its own premise (mainnet routes are 3x SHORTER, 1.9 mean
  hops), no mechanism survives. Delay is free for everyone — misattribution, not staleness, is
  the binding constraint. The 8.6× attempt headline is retired; the
  edge is a success edge under degradation.
- **exp-020** — the championship adjudication. Original tier set, the
  exp-017 binaries, gates reproducing published numbers to three
  decimals. hb1 significantly beats mx_c3 NOWHERE; mx_c3 beats hb1 on
  split_test only, unanimously (+0.062, 8/0, p=.008) — the tier where
  hb1 alone fails to beat lnd. Title DEFENDED; the exp-015/exp-017
  hb1 signals were real but family-specific and did not transfer.
  Also found: the sealed hard tier had been overwritten in scratch by
  exp-010, and hard-test/OOD are not regenerable from any committed
  generator — both now checked into `simulation/lab/scenarios/`.
- **exp-017** — the de-circularization sweep. 13 paired tiers (7
  liquidity families, 2 amount families, 4 re-liquified mainnet), 5
  routers, 650 runs; the untouched mainnet control reproduced exp-009
  to three decimals. lnd rank 5 on 13/13, an evolved router rank 1 on
  13/13; margins compress on easy worlds but the never-fitted seed
  compresses identically, so the compression is a ceiling, not
  overfit priors. atomic1 revealed as the flat-liquidity specialist
  (ladder rank 4→1 monotone; abandonment signature on sharp bimodal).
  hb1 ≥ mx_c3 on 12/13 (liq-uniform p=.004; all mainnet families tie
  to 0.001) — resolved by exp-020: those edges are family-specific
  and do not transfer to the original tier set. Also: give_up_rate ==
  1−success_rate identically for candidates (they always fail by
  giving up), so it is a style fingerprint, not an abandonment
  signal; the evaluator hint now states the read-the-pair rule. The
  hubdrain tier is underpowered at n=10 — no verdicts from it.
- **exp-016** — served weights, the arm exp-012 could not build.
  Third-party observations injected with no payment sent: atomic1
  +0.055 (p=.016), mx_c3 +0.031 with attempts 8.1→4.4, lnd **−0.029
  with attempts going UP**. Successes help everyone; failures are the
  whole of lnd's loss (−0.039, CI excludes zero, 9/10 files).
  **Mechanism took three wrong guesses** (all recorded in the writeup):
  NOT amount-blind penalties (`probability_apriori.go:363` gates on
  amount — this one was published before it was checked), NOT node
  contagion (`apriori.weight=1.0` leaves −0.038), NOT staleness
  (size-matched stale = −0.003 vs fresh +0.000). It is VOLUME: 2,808
  bounds over a 761-edge graph block the consumer's amount nearly
  everywhere, and nothing downstream of lnd's estimator can resize a
  payment — `findPath` takes the amount as a fixed argument. Converges
  with exp-002b on one patch. Champions cannot consume imports at all
  (no contract asked them to), hence
  `exp-016-{mxc3,atomic1}-importer.go`, each its ancestor plus one
  method and cold-identical to it.
- **exp-015** — churn ladder. drift1 vs mx_c3 on ONE fixed corpus at
  payments_per_gap 0/20/80/240 (the top being ~18x exp-008's effective
  churn): −0.016/−0.005/−0.007/−0.003, every CI straddling zero, no
  trend. Decay is a tie at every churn level, not a loss — and not a
  win either, so it stays unproven rather than disproven. The harness
  BACKGROUND prompt has been corrected; it had been telling every
  candidate decay "LOST" and to spend its complexity elsewhere, which
  is a search restriction we imposed on ourselves. Open hypothesis:
  hb1 gains on mx_c3 monotonically with churn (+.018/+.015/+.028/
  +.037, none significant at n=8) — a champion question if it holds.
- **exp-014** — the traffic engine, fixed. Background payments settled
  at 0.41/0.61/0.18 (drift/atomic/mainnet); a failed one moves no
  liquidity, so that ratio was the factor between configured and
  actual churn. Causes: the route search ignored hidden balances,
  amounts were drawn blind and never revisited, and endpoints were
  drawn UNIFORMLY on a graph whose median degree is 1 (68% of nodes
  have ≤2 channels), so most pairs had no path at any amount. Only
  the last explains mainnet: the first two fixes moved it 0.177 →
  0.184, degree weighting moved it to 0.951. Now 0.69/0.89/0.95. Adds
  `focus_fraction` (share of churn aimed at the scenario's own
  corridors; corpora set 0.33) and `GaveUp`/`give_up_rate`/
  `bg_settle_rate` reporting. Re-running the champions over both
  traffic tiers leaves every ordering intact — nothing published is
  overturned.
- **exp-013** — the give-up attractor. Continuation from atomic1 lost
  to its own seed on gepa's held-out test (0.512 vs 0.527) and sits
  below mx_c3 on all six tiers. Cause: atomic1 was already at the
  attempt frontier, so the only cheap direction left was abandoning
  hard payments — fewest attempts everywhere (split_test 2.2) bought
  with the lowest success (0.750 where everyone else is >0.917). The
  recipe that made mx_c3 from hb1 inverts when the seed has no
  attempts left to save.
- **exp-008** — drift. Time-decay re-evolved; its evolved form
  (confidence softening toward the prior, bounds expiring) is
  structurally unlike lnd's penalty fading, and costs nothing on
  static tiers. **Corrected by exp-015: it did not "lose" — it TIED.**
  The 0.04 gap was two point estimates at n=8 with no paired test.
  Caveat added later: our churn is weak (see
  exp-014), so this is a statement about weak churn — the engine is
  fixed now, and the re-run is item 4 below.
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
  Proposer A/B FLIPPED vs exp-010. **Read as a hypothesis, not a
  finding:** "deliberate large-step reflection wins in low-noise
  environments and misfires in churn-noisy ones" is a mechanism story
  fitted to two opposite-sign runs, and per-proposer variance is high
  enough that noise produces this pattern on its own. Untested; gates
  nothing.
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
1. **Upstream PR prep for soft_unknown** — extract the exp-021 Part B
   diff, strip fork-specific comments, port the evidence chain
   (exp-019 pathology, exp-021 recovery table) into a PR narrative.
   Known limitation to state: the min-probability hop choice needs
   capacity threading before it works under the bimodal estimator.
2. Offline replay on real payment data — replay both belief systems
   over a real node's historical attempt stream, score predictive
   log-loss. No simulator in the loop; the escape from
   "simulator-shaped."
3. Plan-time distillation (the hard half): success-side memory
   feeding initial amount choice + joint route-set construction —
   priced as an architectural change after exp-021 measured the
   reactive half null.
4. Upstream the gepa meta_harness JSON fix (merged durable into
   `~/codez/gepa` main at 7c20d98c; the upstream PR to gepa-ai/gepa
   remains). The ceiling arm is DONE (exp-024).
