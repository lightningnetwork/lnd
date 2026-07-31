---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-31T07:14:33Z
compaction_count: 5
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
       DONE: exp-022 verdict (challenger failure #8, 2096b9737).
10.[x] 2026-07-28 marathon: exp-023 FULL CYCLE closed. Five stages
       implemented (worktree ../lnd-econ, branch econ-realism, all
       merged to gepa), 1920-run sweep, verdict at 1673ab3bd: the
       edge is INFORMATIONAL, not pricing. Fee budgets close the
       gap unanimously on mainnet; inbound heavy erases the lead;
       htlc/concurrency/latency move nothing. atomic1 = fee-robust,
       contention-immune. Dashboard v63 published. exp-024 ceiling
       closed. interval-router at 14 commits on fork.
       DONE: atomic1 audit committed (e9873ae40, fee robustness =
       units choice; contention = footprint + reservations).
11.[x] econ arc CLOSED: exp-025 (econ2 = fee-budget specialist,
       challenger #9, first beat-lnd on live bar, 151733ce7) after
       econ1 postmortem (59 proposals killed by missing type;
       fix b1533b5db). exp-026 CLOSED (compose world HOLDS, seed
       returned, honest defeat, ladder monotone to zero, 3b5b5b690).
       Dashboard v64 PUBLISHED (445e8fb89). Specialist roster in
       champions/README (7a2014c8e). exp-023 verdict: edge is
       INFORMATIONAL (1673ab3bd).
14.[ ] 2026-07-31 state: exp-031 CLOSED (90cef300f): compose
       world CLOSED at any seed/budget (800 evals returned seed to
       7 digits; 19 pool accepts all died on full val). Evolution
       track at measured boundary; TREE FREE, no runs planned.
       DONE: dashboard v66 PUBLISHED (version 66 confirmed,
       §21-23, commit pushed both remotes).
       LIVE: exp-032 re-pin ONLY, in
       benchmark agent a7b0df7ce66fc0246 (wider degraded corpus
       n=30, 3 arms + optional broken-tip mix arm + deg-mainnet
       n=30; LAST sim experiment; clears PR to quote magnitudes).
       SHIP STATE: RC interval-router@eb4fc3e62 + rebased
       b489649f6 + interval-sim@dcde83cca all on fork; PR draft in
       docs/interval_routing_pr_draft.md. Pending user: unbudgeted
       branch existence, intervalMaxFeePrice ceiling (lean keep
       both). NEXT WEEK: offline replay (needs node access).
13.[ ] 2026-07-29 evening wave:
       DONE: interval-router ROUND 3 verified+pushed (1bcbb1485:
       budget fee price clamp(remaining/2,30k,420k) msat/nat,
       cheapest-label keep, suspect quarantine). DONE: exp-027
       written+pushed (53637bbb9): integrated branch = champions'
       margin all 6 tiers (mainnet 0.788 att 2.5), exp-019 robustness
       inherited, best arm on mainnet fee rungs; gaps: deg-mainnet
       -0.040 (quarantine target), hard@4000 abandonment (econ2's
       regime). interval-sim pushed to roasbeef. Mainnet cells NEVER
       byte-reproducible (findPath map iter) — future gates read
       mainnet statistically. DONE: dijkstrasden graph secured
       ~/codez/data/realistic_graph.json (first un-authored liquidity
       family, soft U-shape; ideas logged e3308afe7), report
       published https://fee-liquidity-correlation.lightning.wiki/.
       DONE: round-3 re-bench -> exp-027 addendum (5912dd545):
       budget price CONFIRMED (hard@4000 +0.079 only CI-solid r3-r2
       delta, beats every champion, cap-insensitive), quarantine
       NULL (deg_mn gap widened to -0.044 vs mx_c3 CI-solid),
       non-inferiority PASS w/ ood -0.032 watch item. ilnd 13/14
       CI-solid over lnd. interval-sim@09b643d6f pushed.
       ROUND 4 CLOSED as a no-op: keepCheapest gate INERT on
       unbudgeted tiers (pushed as 991f6401e; kept on merits).
       Re-bench diagnosis (agent a7b0df7ce66fc0246, round4 block in
       SCRATCH/isim): (1) round-3 ood -0.032 = IEEE-754
       NON-EQUIVALENCE of the fee-penalty refactor (5*fee/amt vs
       fee/(amt/5) differ in last bit on 24.9% of pairs; frontier
       compares exactly, ulp flips Pareto ties). (2) interval arm NOT
       run-to-run reproducible (map-iter ties; tier obj range
       0.0000-0.0144, worst econ_hard_4000; round-3 headline deltas
       survive 14-29x). (3) budgeted rungs bit-equal r3==r4,
       hard@4000 holds 0.410.
       ROUND 5 verified+pushed (b79f535de): intervalFeePenalty
       helper, verbatim r2 expression both sites (shard score had
       same reciprocal shape), bit-level pin test (25.3% disagree
       rate confirms diagnosis), budgeted branch untouched.
       ROUND-5 re-bench: budgeted rungs bit-equal r3 (19/19),
       single-shard tiers restored to r2, but multi-shard unbudgeted
       tiers NOT (ood still 0.538). TRUE ROOT CAUSE (bisect +
       probe): intervalBudgeted sees REMAINING budget, not the
       payment's limit — MaxMilliSatoshi-feesPaid != sentinel, so
       every unbudgeted payment that splits flips to the budgeted
       branch from shard 2 on. Probe predicate returned 10/11 tiers
       to r2 exactly (ood 0.5702). isim merge d33ae8a1c committed
       (not pushed); `final` block written (13/14 CI-solid over lnd,
       0 losses; 3W/5L vs mx_c3, 4W/1L vs hb1).
       ROUND 6 verified+pushed (60cce3572): intervalFeeRate
       {budgeted, price} latched at construction; regression test
       drives 2 real RequestRoutes. KEY PRODUCTION FINDING (agent):
       lnrpc.CalculateFeeLimit falls back to
       DefaultRoutingFeeLimitForAmount (100% <= 1000 sat, else 5% =
       50000 ppm) — real payments ALWAYS budgeted; unbudgeted branch
       near-unreachable in production. Open design Qs on record:
       drop unbudgeted branch? retune intervalMaxFeePrice (420k
       msat/nat, prod default pins against it on large payments)?
       ROUND 6 ADJUDICATED + exp-027 CLOSED (f6f0b5fb4): latch
       lands (ood 0.5703 vs 0.5702 predicted; budgeted rungs
       bit-equal; 10/11 unbudgeted == r2). FINAL: 14/14 CI-solid
       over lnd, 0 losses; prod-default battery (fee_limit 5% = real
       node): margins hold all 6 tiers, clamp costs <=0.0095, zero
       refusals every arm -> exp-023/025 fee verdicts are
       tight-budget statements. Open finding filed: deg_hard_mix
       unknown x shift interaction (5x sum, z=-11.1), quarantine
       wrong-channel hypothesis, tiers in
       exp-027-deg-mechanism-split.json. Branches pushed:
       interval-router@60cce3572, interval-sim@fc7bfb065.
       code_full2 DONE -> exp-028 (508ac3738): give-up attractor
       reproduced from econ2 seed (best-val loses 0.030 held-out to
       its own seed, all success; verified by independent overlay
       rerun). Compose escape arm 1 dead. TREE FREE.
       USER DIRECTIVES 2026-07-30 (committed to CLAUDE.md): ship
       target = interval router in next lnd MAJOR RELEASE;
       soft_unknown PR DROPPED; offline replay NEXT WEEK.
       ALL THREE TRACKS EXECUTED (2026-07-30):
       (1) loader flag LANDED (7d10989fd, verified+pushed both):
       from_graph + unbalanced_source; byte-identity 14/14 proven;
       smoke on realistic graph binds (33% tails vs our 63%).
       (2) code_full3 LIVE (800 evals, launched after loader; gate
       reproduced 0.3162367 to 6 decimals; watcher armed task
       b7htr0zi1; log SCRATCH/code_full3.log). TREE LOCKED again.
       On exit: artifact check, give-up watch, exp-030 writeup =
       compose final verdict.
       (3) dashboard v65 PUBLISHED (version 65 confirmed; commit
       pushed both remotes): findings §19 exp-027 + §20 exp-028.
       exp-029 CLOSED (b0795367b + CLAUDE.md entry): ordering
       SURVIVES on foreign balances, margin a third WIDER
       (+0.127..0.132 vs +0.097, all 10/0 p=.002); family swap ~zero
       (7 CIs straddle; fitted mx_c3 gains least) — circularity
       caveat measured ~0. ilnd tracks champions to 3rd decimal,
       gains under prod default. atomic1 top (flat-liquidity filing
       predicted it). Fee signal present (Spearman -0.149), nobody
       exploits it. isim tip ffe5e5537 pushed. Digest mailed.
       LIVE (3 tracks, 2026-07-30 evening):
       (a) code_full3 (232/800 evals healthy, watcher b7htr0zi1,
       tree locked). On exit: artifact check, give-up watch, exp-030
       writeup = compose FINAL verdict, dashboard v66.
       (b) RELEASE-READINESS DONE, verified+pushed: RC tip
       c5881065d (23 commits) + interval-router-rebased 22161949a
       (zero-conflict rebase onto upstream master, full battery
       green) both on roasbeef fork. Postgres tests already wired
       (verified vs real PG), flush knob added
       (routerrpc.intervalflushinterval), blinded fallback test
       strengthened (same-session property), quarantine severable
       (revert clean OR DisableQuarantine gate),
       docs/interval_routing_pr_draft.md drafted. GOTCHA: repo has
       rebase.updateRefs=true — moved interval-router during the
       spare rebase; agent restored from reflog; use -c
       rebase.updateRefs=false in these worktrees.
       PENDING USER DECISIONS for ship: (1) quarantine keep/drop
       (await deg_hard_mix mechanism, track c); (2) unbudgeted
       branch: near-unreachable in prod (RPC always sets finite
       limit) — keep or remove; (3) intervalMaxFeePrice 420k
       msat/nat as the de facto prod ceiling (exp-029 A_prod says
       clamp costs ~nothing).
       (c) exp-030 CLOSED (71e93d2dc): mechanism = misattribution
       manufactures innocence (shifted reports write false LowerOK
       on guilty channel -> disarms quarantine's 3 suppression
       rules; 9.6% of mix convictions on innocent channels, ground
       truth). Fix V3 = ProvenOK (settlements only) measured
       +0.0406 mix (above pre-quarantine ref), clean identical.
       QUARANTINE KEEPS. V3 IMPLEMENTED + verified + pushed
       (8eabe1daf + docs eb4fc3e62, tip on fork): ProvenOK
       settlements-only, forward-only, NOT persisted (agent's call,
       accepted: pre-restart settlements shouldn't suppress fresh
       suspicions; Restored re-arming hole), fourth LowerOK path
       (coincident-amount promotion swallow) documented+pinned NOT
       widened. LIVE: code_full3 ONLY. exp-030 RECORD CLOSED (addendum
       pushed): committed V3 == throwaway on all 5 tiers, mix 0.5138
       (+0.0439 vs broken tip, +0.0098 vs pre-quarantine ref),
       innocent convictions 9.6%->8.4% = the shift-only floor.
       isim@dcde83cca pushed. Rebased branch b489649f6 pushed
       (see (c) entry above).
       QUEUED: wider degraded corpus re-pin before PR quotes
       magnitudes; exp-031 = full3 compose verdict; dashboard v66.
       Then: offline replay NEXT WEEK (user); ship target = interval
       router in next lnd major.
12.[x] LIVE NOW (2026-07-29):
       (a) code_full2: compose world seeded FROM econ2 (--seed-file
       exp-025-econ2-best-candidate.go, --econ --degraded, 400
       evals, timeout 1200s for the 1230-line seed). Log
       SCRATCH/code_full2.log; SLOW start is NORMAL (heavy seed on
       event-loop files). Watcher armed (watch_full2.sh). TREE
       LOCKED (routing/, cmd/routesim/) until done. On completion:
       econ1-style artifact check FIRST, then verdict sweep w/
       attempt-cap check + exp-013 give-up watch (econ2 seed is
       attempt-heavy; cheap direction = attempt shedding).
       (b) interval-router ROUND 3 (same integration agent):
       budget-aware shard pricing (econ2 design), units audit
       (atomic1 lesson), suspect quarantine (deg1). Worktree
       ../lnd-interval @ ab1c123ab+. Verify build/vet/test + itest,
       push to roasbeef fork after review.
       (c) interval-sim BENCHMARK agent: new worktree ../lnd-isim,
       branch interval-sim = gepa + interval-router@ab1c123ab
       merged, params knob router_impl=interval on the lnd arm,
       byte-identity gate, then 6-arm battery (classic/degraded/
       econ) answering: regression vs stock lnd, exp-019
       robustness, hybrid fee discipline. Results SCRATCH/isim.
       NEXT after wakes: verdicts + writeups (exp-027 = isim bench?),
       re-bench round 3, dashboard v65 when verdicts land.
       Pending user: node access (replay), soft_unknown PR hold,
       tracked 19MB routesim binary at root, sealed mainnet tier's
       /tmp graph path. Champions: hb1+mx_c3 (9 challengers);
       specialists atomic1 + econ2. HEAD 7a2014c8e pushed both.
