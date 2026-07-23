# Lean Local Reputation PR — Steps 1 & 2 (tracking + switch hooks)

Branch: `local-reputation-tracking` (off current master).
Reference (parked, full impl): branch `local-reputation-subsystem` / PR #10919.

Scope narrowed per PR #10919 discussion (carlaKC's 5-step breakdown, George's
"keep only 1 & 2"). This PR is a **minimal, log-only** first slice designed to
lighten LND review burden. Buckets, persistence, restart/in-flight, cold-start,
and the debug RPC are all **deferred to later steps** (3/4/5) or retired.

## In scope
- **Step 1 — reputation tracking**: decaying-average math, per-peer/channel
  reputation + revenue threshold, add/resolve HTLCs to update it.
- **Step 2 — connect to switch**: read-only hooks feeding forwarded HTLCs to the
  manager; per-HTLC **isolation** mock-decision log — "if this HTLC were
  forwarded in isolation, would its outgoing channel have sufficient reputation
  (to be protected)?" No buckets, no action taken.

## Out of scope (deferred / retired)
- **Buckets** (general/congestion/protected, ChaCha20 slots) → step 4/5.
- **Persistence** (kvdb Store, snapshots) → step 3.
- **Restarts / in-flight replay / ActiveCircuits** → step 3.
- **Cold-start / historical read** → **retired** (once 1&2 deploy, live nodes
  self-bootstrap from real forwarding; a historical-read follow-up is moot).
- **devrpc `FetchReputation` RPC + Snapshot** → deprioritized (dev-server-only;
  revisit `listchannels` later).
- **Channel-lifecycle feed (`ChannelSource`/channelnotifier)** → only needed for
  bucket sizing; dropped. Per-scid state is created lazily on first HTLC event.

## Step 1 — `reputation/` package (trimmed from reference)
Carry over from the reference (verbatim or lightly trimmed):
- `decaying_average.go` — `DecayingAverage` (LDK-conformant; unchanged).
- `revenue.go` — `aggregatedWindowAverage`. The decaying-average math is
  LDK-conformant, but the warm-up divisor follows the **BOLT proposal's
  exponential factor** rather than LDK's integer-elapsed-windows divisor (see
  "Warm-up divergence" below).
- `config.go` — trim to `ResolutionPeriod`, `RevenueWindow`,
  `ReputationMultiplier`, `RevenueWindowCount` (+ `Validate`). Drop the
  bucket-percentage fields. `ReputationMultiplier` (12) sizes the
  outgoing-reputation window only; `RevenueWindowCount` (>=6, default 6) is a
  distinct count sizing the incoming-revenue aggregated average, per the
  proposal (LDK reuses the multiplier for both; we keep them separate).
- `channel.go` — `channelReputation` trimmed to `outgoingReputation`,
  `incomingRevenue`, `pendingHTLCs`. Drop the 3 buckets + `lastCongestionMisuse`.
- `htlc.go` — `pendingHTLC{fee, accountable, addedAt, incomingCltv}`, `htlcRef`;
  `effectiveFee`, `opportunityCost`, `inFlightRisk`. Drop `bucketAssigned`.
- `decision.go` — the **isolation** verdict only:
  `sufficient = outgoingReputation − thisHTLCRisk ≥ incomingRevenueThreshold`.
  Drop the bucket decision tree.
- `manager.go` — `OnForward`/`OnSettle`/`OnFail`; lazy per-scid channel creation;
  add-on-forward / resolve-on-settle-or-fail → update reputation; compute + log
  the isolation decision. Keep the injected clock + height source (needed for
  `inFlightRisk` worst-case hold). Drop buckets/persistence/feed.
  **Async worker (non-interference):** the hooks are non-blocking — each captures
  its event (timestamp + cached height captured AT the hook) and does a
  best-effort send to a bounded buffered channel; a single worker goroutine
  (started in `Start`, drained in `Stop`) owns the channel-state maps and does
  ALL mutation/FP-math/logging/GC. On a full queue the hook drops the event,
  bumps a dropped counter and emits a rate-limited warn. Because only the worker
  touches the maps, they need no mutex against the switch goroutine
  (`cachedHeight` stays atomic).
- `clock.go`, `log.go`.

Drop entirely: `buckets.go`, `channels.go`, `store.go`, `snapshot.go`,
`manager_startup.go` (+ their tests). Salvage non-bucket/non-persistence cases
from `manager_review_test.go` into `manager_test.go`.

Pending-HTLC tracking stays (minimal) — only to match forward→resolve so we can
compute `effectiveFee` on resolution. It is NOT summed for in-flight risk (the
decision is per-HTLC "in isolation").

## Step 2 — switch connection
- `htlcswitch/interfaces.go` — `ReputationManager` interface (3 methods).
- `htlcswitch/switch.go` — 3 nil-guarded hooks at the circuit layer (unchanged).
- `htlcswitch/reputation_guard.go` — panic guard (recover + self-disable).
- `htlcswitch/reputation_hooks_test.go` — seam tests (fire / nil no-op /
  local-skip / panic-survival).
- `reputation_adapter.go` — trimmed bridge (no `ChannelSource`, no
  `ForwardingLog`, no in-flight reconstruct).
- `server.go` — construct manager behind the flag; inject clock + height + logger.
- `log.go` — register the `REPM` sublogger.
- `lncfg/routing.go` + `sample-lnd.conf` — the off-by-default flag.
- Per-forward **isolation decision log line** (structured, greppable).

## OPEN DECISION — set the experimental `accountable` field?
carlaKC's step 2 lists "**Set experimental field based on `accountable` and
reputation signal**". George's restatement said "**read-only hooks** + mock
decision log". These differ:
- **Observe-only (recommended default here):** read the incoming accountable bit
  (needed for `effectiveFee`), log the decision, **do not** write the outgoing
  bit. Pure log-only, smallest diff, matches the reference's philosophy.
- **Set-field:** additionally set the outgoing `accountable` TLV based on the
  reputation decision — a wire behaviour change (touches
  `link.go:experimentalAccountability`), larger and no longer "read-only".

**DECIDED: observe-only / log-only.** Read the incoming accountable bit (for
`effectiveFee`), log the decision, do NOT write the outgoing bit. Setting the
field is deferred (revisit alongside enforcement / later steps).

## Warm-up divergence from LDK (intentional)
The incoming-revenue aggregated average divides the raw decayed sum by a warm-up
factor so a brief history does not read as an artificially low threshold. LDK
(rust-lightning PR #4468) divides by `min(elapsed_windows, windowCount)`. We
instead use the BOLT proposal's smooth exponential factor:

    periods = (now - start) / revenueWindow          // fractional
    warmup  = windowCount * (1 - exp(-periods / windowCount))
    if warmup < 1 { warmup = 1 }                      // guard periods->0
    value   = round(raw / warmup)

This is the **one intentional divergence** from LDK in this package. It avoids
the `periods -> 0` singularity and the early over-inflation of the integer
divisor. The LDK conformance vectors for `opportunity_cost`, `effective_fee` and
the core `decaying_average` are unaffected and continue to pass unchanged.

## Restart behaviour (accepted for this slice)
No persistence → reputation is in-memory and **resets on restart**, re-accruing
from live traffic (the self-bootstrapping model). In-flight HTLCs that resolve
after a restart are unmatched no-ops. Documented; persistence returns in step 3.

## Testing
- **Unit**: LDK conformance vectors (`decaying_average`, `opportunityCost`,
  `effectiveFee`); `Config.Validate`; manager event lifecycle
  (forward→settle/fail moves reputation/revenue); isolation-decision boundary.
- **Seam**: switch hook tests (fire once w/ correct keys, nil no-op, local-HTLC
  skip, forwarding survives a panicking manager).
- **itest**: forward through a node with `--routing.reputation` on; assert
  non-interference + the reputation decision **log line** (log-scrape via a
  harness helper). No exact-value RPC assertions (no RPC in this slice).

## Rough footprint
Far smaller than the reference (~2.3k external lines + 24 pkg files). Expect the
`reputation/` package ~halved, external touch = `htlcswitch` (interface + 3 hooks
+ guard + test), `server.go`, `log.go`, `lncfg`, `sample-lnd.conf`, and a lean
itest. No proto/generated code, no persistence, no buckets.
