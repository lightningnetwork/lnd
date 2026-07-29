---
session_id: 019f95d9-7ff7-709b-8847-ec904f72acdd
shortname: routing-evolution
last_updated: 2026-07-29T23:41:24Z
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
12.[ ] LIVE NOW (2026-07-29):
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
