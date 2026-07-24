# Decisions Log

Load-bearing choices and why. Newest at the bottom.

- **Build our own sim; mine dijkstrasden for ideas only.** Its portal
  architecture (patched lnd, gossip injection, itests) targets
  implementation testing; an optimizer needs cheap, seeded, in-process
  evals. (2026-07-24)
- **Sim lives in package `routing`** so it drives the real payment
  session + mission control unexported internals; ships only on this
  experimental branch.
- **Candidates own everything** (route selection AND splitting) behind
  `SimRouter`; sealed gossip view so information asymmetry matches a
  real sender. The lnd stack is just the default factory.
- **Composite objective, success-dominant:** success − capped attempt −
  capped fee penalties. Uncapped attempt penalties drowned the signal
  (exp-001).
- **Champions decided only by held-out three-way validation** (sealed
  test + OOD + mainnet vs lnd and seed) — never by GEPA's internal
  minibatch `best_score`, which is inflated.
- **Pure gepa engine after meta_harness broke** (claude-CLI JSON-array
  parser bug; patched in venv but unvalidated). Adaptive/omni composers
  deferred.
- **Contract changes freeze while a code run is live** — evaluate_code
  compiles from the working tree; mid-run edits corrupt comparisons.
  Batch-2 fidelity fixes deferred for this reason.
- **Small seed + insights-in-prompt over giant-champion seeds**
  (code_gen2 design): giant seeds reflect slowly and hit the ~800-line
  complexity wall; transfer the ideas, not the body.
- **Zero-time-logic treated as partly sim artifact, not gospel** —
  exp-008 will add background traffic and test whether time-awareness
  re-evolves before drawing conclusions for lnd. Same posture for
  splitting (exp-010).
