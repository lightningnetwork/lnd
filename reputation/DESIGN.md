# Local Reputation Subsystem — Read-Only / Log-Only (LND)

Status: implemented (P1–P9), log-only, off by default
Branch: `local-reputation-subsystem`

### Implementation notes / corrections discovered against the LDK reference

While porting from rust-lightning PR #4468 the following were pinned down
(DESIGN text above predates them; they are the authoritative behaviour now):

- **`opportunity_cost` rounds, not floors.** LDK uses `f64 .round()`. We match
  it (`math.Round`); the vectors (91s→1, 135s→50, …) are round results.
- **`AggregatedWindowAverage` warm-up is an elapsed-windows divisor**, not the
  `1 − e^(…)` factor in §3/§6. LDK divides the inner decaying value by
  `min(max(1, windows_elapsed), window_count)`. We match LDK.
- **Revenue window count = `reputation_multiplier` (12)** in LDK #4468, not a
  separate `window_total` (6). We use `ReputationMultiplier` for both, matching
  the current reference (diverges from the BOLT write-up's separate 6).
- **General-bucket slot assignment** uses ChaCha20 (`x/crypto/chacha20`,
  IETF/96-bit nonce) with `max_attempts = per_channel_slots * 2` (LDK value, not
  ×10). Determinism/reload tests assert self-consistency, not byte-identity with
  LDK's Rust ChaCha20 (couldn't run LDK to capture its exact slot indices).
- **Lazy channel creation:** unlike LDK (strict `add_channel` before any HTLC),
  the Manager lazily creates per-channel state on first event using learned
  limits or a fallback, so log-only mode never drops an event for an unknown
  scid. ChannelSource still seeds limits at startup.
- **In-flight HTLCs are discarded on restart (deliberate, log-only).** The
  pending set starts empty; resolutions of HTLCs that spanned the restart are
  tolerated as no-ops. `ReplayInFlight` is implemented + unit-tested but
  **parked / not wired** — see §6/§9 for the trade-off and the future-enforcement
  caveat. Cold-start zero-state and warm-restart average reload are wired.

### Review fixes applied (independent code/architecture/test review)

- **Best block height is cached, not fetched on the hot path.** The height
  source (an RPC in production) is read off the forwarding path — refreshed at
  Start and every 30s by the maintenance loop into an atomic — so the
  `OnForward` hook never calls it while holding the manager mutex (was a
  hot-path stall risk). See `Manager.cachedHeight` / `refreshHeight`.
- **Decaying-average reads saturate on overflow.** `clampFloatToInt64` guards
  the `float64 → int64` cast in `decayingAverage.valueAt` /
  `aggregatedWindowAverage.valueAt`; `float64(MaxInt64)` rounds to 2^63 and the
  naive cast would flip a saturated value to `MinInt64`.
- **GC releases bucket occupancy.** `gcStalePendings` now frees the bucket
  resources an evicted pending reserved (shared `releaseBucketLocked` with the
  resolve path), so a missed resolution can't leak occupancy.
- **Failed persistence flush is retried.** `flush` re-marks channels dirty if
  `PersistChannels` errors, instead of silently dropping them.
- **Small / oversized channels are handled.** `max_accepted_htlcs` is clamped to
  the protocol max (483); channels below `minChannelHTLCsForBuckets` (13) get a
  single general bucket (per the BOLT small-channel recommendation) instead of
  degenerate zero-slot buckets, and `perChannelSlots` is capped at the bucket's
  total slots so assignment always succeeds.
- **The `REPM` subsystem logger is registered with lnd's root logger** (`log.go`,
  `AddSubLogger(root, reputation.Subsystem, ...)`). Without this the package's
  logger had no backend, so a *log-only* subsystem emitted nothing in a real
  node — found when the L3 itest asserted on a log line and it never appeared.
- **L3 itest asserts a computed-reputation log line.** `resolveHTLCLocked` emits
  an Info-level `"Reputation gained: outgoing=… eff_fee=… new_outgoing_reputation=…"`
  (and `"Reputation lost"` for negative effective fees) on every resolution that
  moves reputation; the itest matches on it after a successful forward and again
  after the warm restart, via the new `HarnessTest.AssertNodeLogContains` polling
  helper. L3 now verifies reputation is actually computed (positive gain), not
  just non-interference. Exact per-channel value assertions still await the P10
  RPC read-out.

### Seam hardening (L2/L3 follow-up review)

- **Panic boundary on the hooks (blast-radius guard).** The hooks run
  synchronously on the switch's forwarding goroutine, so a panic in the
  (log-only) subsystem would otherwise crash forwarding. The manager is wrapped
  by `htlcswitch.NewGuardedReputationManager` (in `htlcswitch/reputation_guard.go`,
  applied in `server.go` before the switch sees it, so `switch.go` call sites are
  untouched): each hook runs behind `recover()`, and on a panic the guard logs,
  permanently **disables** the subsystem (fail open), and forwarding continues.
  Proven by `TestGuardedReputationManagerRecovers` (fails without the guard) and
  `TestSwitchReputationPanicSurvives` (a forward survives a panicking manager).
- **Local-HTLC skip is now tested.** `TestSwitchReputationLocalSendSkipped`
  asserts a locally-originated HTLC never invokes the hooks (only true forwards
  are observed).
- **itest broadened + run.** `testLocalReputationLogOnly` now covers a
  successful forward, a downstream-failed forward (exercises `OnFail`), and a
  **warm restart** of the forwarding node, asserting forwarding is unaffected in
  every case. It runs green end-to-end on the btcd backend (~29s). Asserting
  computed reputation values still awaits the P10 read-out.
Scope of this effort: **P1–P9** core + **P10** optional (event tracking →
reputation math → sufficiency verdict → bucket accounting → persistence →
startup reconstruction), all **log-only** — the subsystem computes and records
decisions but never affects routing or the wire. See §8 for the phased targets.

**No historical read.** Reputation is built only from HTLCs observed live after
the subsystem is enabled (plus state persisted across restarts). We deliberately
do **not** seed reputation from `channeldb.ForwardingLog` or any other history.
This matches LDK exactly (see §6) and avoids the fidelity problems of a
settles-only, no-hold-time backfill.

## 1. Goal & non-goals

Implement [lightning/bolts#1280](https://github.com/lightning/bolts/pull/1280)
("Outgoing reputation and HTLC Accountability" / Local Resource Conservation) as
a self-contained LND subsystem that observes HTLC forwarding and computes:

- per-outgoing-channel **reputation** (decaying average of effective fees),
- per-incoming-channel **revenue threshold**,
- the **sufficiency** verdict (`reputation − in_flight_risk ≥ revenue_threshold`),
- resource **bucket** assignment (general / congestion / protected).

**Log-only:** every verdict and bucket decision is logged. Nothing is failed,
delayed, re-prioritised, or signalled on the wire. We do **not** set the outgoing
`accountable` bit from our decision — LND's existing `experimentalAccountability`
logic keeps doing that, and we merely *observe* the bit it produced.

Non-goals (explicit future work, not this effort):
- Enforcement: failing HTLCs on a `Fail` outcome, gating buckets.
- Driving the outgoing `accountable` signal from reputation.
- A reliable enforcement hook (see §4 caveat).

Reference implementation studied: rust-lightning PRs
[#4232](https://github.com/lightningdevkit/rust-lightning/pull/4232) (merged —
`accountable` wire signal),
[#4409](https://github.com/lightningdevkit/rust-lightning/pull/4409) (the engine,
`lightning/src/ln/resource_manager.rs`),
[#4468](https://github.com/lightningdevkit/rust-lightning/pull/4468)
(persistence + read-only ChannelManager integration). LDK's read-only
integration is the closest analogue to what we are building.

## 2. Integration seam — explicit hooks at the switch circuit layer

We feed the subsystem through **three explicit hooks in `htlcswitch/switch.go`**,
all at the circuit-matching layer where LND already records
`channeldb.ForwardingEvent`s. This was chosen over subscribing to the
`HtlcNotifier` because the notifier is best-effort async pub/sub (can drop
messages under load), whereas the switch's circuit matching is synchronous and
authoritative — the switch *must* match every resolution to its circuit, so
matching and once-only dedup come for free (`closeCircuit` /
`ErrCircuitClosing` / nil-circuit guards).

| Hook | Site | Data available |
|---|---|---|
| `OnForward` | `handlePacketAdd` (`switch.go:2851`) | `packet.incomingChanID`, `outgoingChanID`, `incomingAmount`, `amount`, `incomingTimeout` (incoming cltv), `htlc.PaymentHash`, `htlc.CustomRecords[106823]` (accountable) |
| `OnSettle` | `handlePacketSettle`, `circuit.Outgoing != nil` branch (`switch.go:3078`) | `PaymentCircuit`: `Incoming`/`Outgoing` keys, `IncomingAmount`, `OutgoingAmount`, `PaymentHash` |
| `OnFail` | `handlePacketFail` (`switch.go:3110`) | same `PaymentCircuit` via `closeCircuit` |

Each is a single guarded call:

```go
if s.cfg.ReputationManager != nil {
    s.cfg.ReputationManager.OnForward(ctx, reputation.ForwardEvent{...})
}
```

Local (non-forwarded) HTLCs are already filtered here by the existing
`hop.Source` / `localHTLC` checks, so the subsystem only ever observes real
forwards.

**External footprint, total:**
1. `htlcswitch/switch.go` — 3 guarded one-line calls + one optional config field
   (`ReputationManager` interface, nil when disabled).
2. `htlcswitch/interfaces.go` — add the `ReputationManager` interface to the
   `Config` (mirrors how `HtlcNotifier` is wired).
3. `server.go` — construct the manager, inject a `BestBlockHeight() uint32`
   getter + kvdb backend + logger + the channel feed (below), wire
   `Start()`/`Stop()` into the lifecycle.
4. `lncfg` — one experimental flag, default **off**.

**Second read-only seam — channel lifecycle (needed from P6 for bucket sizing).**
Buckets are sized per channel from `MaxAcceptedHtlcs` / `MaxPendingAmount`
(`channeldb/channel.go:511,523`), so the manager needs the analogue of LDK's
`add_channel`/`remove_channel`: enumerate active channels at startup to read
those two limits, and subscribe to `channelnotifier.ChannelNotifier`
(`OpenChannelEvent` / `ClosedChannelEvent` — same read-only pub/sub family as
`HtlcNotifier`) to add/remove channels live. Injected as a small read interface.
Reputation/revenue (P3–P5) don't need this — per-scid averages are created
lazily on first HTLC event; only bucket sizing needs capacity limits. Holder vs.
remote side of the limit is pinned at P6 (match LDK's `get_holder_max_*`).

Everything else lives in `reputation/`. The interfaces keep the switch's
dependency a black box: it can only call observation methods.

### accountable bit — known simplification

At `handlePacketAdd` the switch sees the **incoming** accountable bit
(`htlc.CustomRecords[106823]`, values `0`/`7`). The spec's `effective_fee` keys
on the **outgoing** (offered) bit, computed later in
`link.go:experimentalAccountability`. LND relays the bit 1:1 (a forwarder MUST
echo an incoming accountable), so incoming ≈ outgoing for relayed HTLCs. LDK
#4232 made the identical collapse and called the info loss acceptable. We adopt
the same simplification; upgrade path is to capture the outgoing bit at
`link.go:1663` and thread it through later if the distinction is ever needed.

## 3. Package layout (`reputation/`)

Mirrors LDK's `resource_manager.rs`, Go-idiomatic:

```
reputation/
  config.go            Config{GeneralPct, CongestionPct, ResolutionPeriod,
                         RevenueWindow, ReputationMultiplier}; defaults; Validate()
  manager.go           Manager: channels map, OnForward/OnSettle/OnFail,
                         Start/Stop, BestBlockHeight + clock hooks, GC of pendings
  decaying_average.go  DecayingAverage (decay_rate = 0.5^(2/window_secs),
                         per LDK #4468's true-half-life rework)
  revenue.go           aggregatedWindowAverage (warmup_factor =
                         1 - e^(-elapsed/tracked); window_total ≥ 6)
  channel.go           channelReputation: outgoingReputation, incomingRevenue,
                         pendingHTLCs, 3 buckets; SufficientReputation()
  htlc.go              pendingHTLC, htlcRef; effectiveFee(), opportunityCost(),
                         inFlightRisk()
  buckets.go           generalBucket (ChaCha20 salted slot assignment),
                         bucketResources (congestion/protected), allocations
  decision.go          decision tree -> Outcome{Forward(accountable)|Fail,
                         Bucket}; LOG ONLY
  channels.go          ChannelSource: startup enumeration + channelnotifier
                         subscription; per-channel bucket sizing (from P6)
  store.go             persistence interface + kvdb impl (no historical read)
  clock.go             injected clock interface (deterministic in tests)
  log.go               subsystem logger ("REPM")
  *_test.go            unit + table-vector tests (port LDK vectors)
```

### spec → LDK → LND term map

| Spec | LDK `resource_manager.rs` | LND `reputation/` |
|---|---|---|
| decaying average | `DecayingAverage{value,decay_rate}` | `DecayingAverage` |
| `incoming_revenue_threshold` (warmup, ≥6 win) | `AggregatedWindowAverage` | `aggregatedWindowAverage` |
| `opportunity_cost` | `opportunity_cost()` | `opportunityCost()` |
| `effective_fee` (accountable/settle matrix) | `effective_fees()` | `effectiveFee()` |
| `in_flight_htlc_risk` (worst-case hold) | `htlc_in_flight_risk()` | `inFlightRisk()` |
| sufficiency `rep − inflight ≥ threshold` | `Channel::sufficient_reputation()` | `channelReputation.SufficientReputation()` |
| general/congestion/protected + ChaCha20 slots | `GeneralBucket`/`BucketResources` | `generalBucket`/`bucketResources` |
| lifecycle hooks | `add/remove_channel`, `add/resolve_htlc` | `OnForward`/`OnSettle`/`OnFail` |

## 4. Manager event flow

### Concurrency model

Event *processing* is **synchronous and in-memory**, *not* a lossy async queue —
that would reintroduce the message loss we chose explicit hooks to avoid. The 3
hooks call `Manager` methods inline on the switch's goroutine; matching, math,
and bucket accounting are O(1) map+arithmetic ops under a mutex (microseconds).
This mirrors the switch's existing pattern of taking `s.fwdEventMtx` inline to
append a `channeldb.ForwardingEvent` (`switch.go:3085`).

The **only async part is persistence**: a background goroutine (the subsystem's
`Start()`/`Stop()` lifecycle) flushes dirty state to kvdb on a ticker + on
shutdown, batched like the switch's `flushForwardingEvents`. The flush snapshots
under the lock then writes outside it, so forwarding never blocks on disk I/O.
Fallback if contention is ever measured: sharded per-channel locks. Default is
drop-free and reliable.

### Per-event flow

`Manager` keys pending HTLCs by `(IncomingCircuit, OutgoingCircuit)` (= the
switch's circuit identity).

- **`OnForward`** → compute `fee = AmtIn − AmtOut`,
  `inFlightRisk(incoming_cltv, BestBlockHeight())`; look up the outgoing
  channel's `outgoingReputation` and the incoming channel's `incomingRevenue`
  threshold; classify sufficient/insufficient; run the bucket decision tree
  (occupancy tracked, never rejects); store the `pendingHTLC`; **log** the
  would-be `Outcome`.
- **`OnSettle`** → match the pending; `resolution_time = settle_ts − add_ts`;
  `effectiveFee()` using observed accountable; `outgoingReputation.Add(effFee)`;
  `incomingRevenue.Add(fee)` (settle only); release bucket; drop pending; log.
- **`OnFail`** → match the pending; `effectiveFee = −opportunity_cost`
  (accountable) or `0` (not); no revenue update; congestion-misuse stamp if
  slow; release bucket; drop pending; log.

`effectiveFee(fee, resolution_time, accountable, settled)`:
- accountable + settled: `fee − opportunity_cost`
- accountable + failed: `−opportunity_cost`
- unaccountable + settled within `resolution_period`: `fee`
- else: `0`

`opportunity_cost = max(0, (resolution_time − resolution_period)/resolution_period) * fee`
`inFlightRisk = opportunity_cost(maximum_hold_seconds, fee)` where
`maximum_hold_seconds = (incoming_cltv − height_added) * 10 * 60`.

### Caveats designed around

- **Block height** for `inFlightRisk` comes from an injected
  `BestBlockHeight() uint32` getter (server has the chain). 10-min blocks
  assumed, per spec.
- **Stale pendings.** If a resolution is somehow missed, a pending leaks. GC any
  pending past its `maximum_hold_seconds`. (Less likely than with the notifier
  because switch matching is authoritative, but kept as a safety net.)
- **Non-strict forwarding / multiple channels:** the event carries the real
  selected `OutgoingCircuit`, so reputation is tracked on the SCID actually
  used, matching the spec's multiple-channels note.
- **Enforcement** would need these same synchronous hooks to be able to *return*
  a decision and have the switch act on it — out of scope here, but the hook
  placement already sits at the right decision point for a future flip.

## 5. Resource bucketing (accounting only)

Implemented fully, but only to **track occupancy and log assignment** — never to
reject. Defaults: general 40%, congestion 20%, protected 40% of both
`max_accepted_htlcs` and `max_htlc_value_in_flight_msat`. These two per-channel
limits are read from channel state via the channel-lifecycle seam (§2): at
startup enumeration + on `channelnotifier` open events.

- **General bucket**: per-`(incoming_scid, outgoing_scid)` slot subset via
  ChaCha20 keyed by a per-channel salt, nonce = `incoming_scid[0:4] || outgoing_scid`,
  read 4 bytes → `uint32 % total_slots` until `general_slot_allocation` distinct
  slots chosen. `general_slot_allocation = max(5, total*5/100)`. Salt persisted.
- **Congestion bucket**: one in-flight HTLC per outgoing channel; blocks an
  outgoing channel for ~2 weeks if it previously held a congestion HTLC longer
  than `resolution_period`; single-HTLC size cap.
- **Protected bucket**: reserved for sufficient-reputation channels; may spill
  into other buckets when full.

Decision tree (logged, not enforced): incoming accountable → require sufficient
reputation → protected else general; not accountable → general if available,
else protected (if sufficient), else congestion (if eligible), else `Fail`.

## 6. Persistence & startup (P8–P9)

No historical read. Reputation accrues only from live observation; the only
thing that survives a process restart is the persisted decaying-average state.
This is exactly LDK's model — verified in rust-lightning PR #4468 at head
`670c9045`:

- Cold start (`channelmanager.rs:17799-17822`) builds a **fresh**
  `DefaultResourceManager` and calls `add_channel(...)` per channel; the only
  production code that ever feeds the averages is `add_value` at
  `resource_manager.rs:1042`/`:1078`, both inside `resolve_htlc` — there is no
  path from any forwarding/payment log into reputation state. LDK core keeps no
  forwarding-history store at all.

We adopt the same: a freshly-enabled LND node starts every channel at **zero
reputation / zero revenue** and warms up purely from live traffic over the
rolling windows. We accept the ~24-week warmup blind spot rather than do a
lossy (settles-only, no-hold-time) backfill from `channeldb.ForwardingLog`.

### Persisted state (own kvdb namespace)

Per channel: `outgoingReputation` (`DecayingAverage`), `incomingRevenue`
(`aggregatedWindowAverage`), general-bucket salts, congestion-misuse timestamps,
and channel params (`max_accepted_htlcs`, `max_htlc_value_in_flight_msat`).
**Not** persisted: pending HTLCs / live bucket occupancy — discarded on restart
(the pending set starts empty; see the decision below).

`DecayingAverage` reads recompute `decay_rate` from the window; writes store the
value + `last_updated`.

### Startup (both cold and warm)

- **Warm restart**: reload persisted averages.
- **Cold start (first-ever enable)**: no persisted state → build empty per-channel
  state via the equivalent of `add_channel`. Zero reputation everywhere.
- **In-flight HTLCs are discarded on restart** (the pending set starts empty) —
  see the decision below.

### Decision: discard in-flight HTLCs on restart (log-only)

We deliberately do **not** reconstruct currently-in-flight HTLCs on restart.
Rationale and trade-off:

- **Why we can.** Reconstructing them requires joining the switch **circuit map**
  (which has only keys + amounts) with each open channel's live commitment HTLC
  set to recover the **CLTV** and the **accountable** custom record — a
  non-trivial reach into `lnwallet`/channel internals from `server.go`. For a
  log-only subsystem the payoff doesn't justify it.
- **The flaw (bounded, self-healing).** Resolutions of HTLCs that spanned the
  restart fire `OnSettle`/`OnFail` with no matching pending and are tolerated as
  no-ops (`Manager.resolve`), so their fee/penalty is not applied to
  reputation/revenue; and in-flight risk + bucket occupancy under-count until
  those HTLCs drain. The affected set is only the HTLCs locked at the instant of
  restart (a handful vs a 24-week window of thousands), and it self-heals within
  one max-hold window. No leak, no double-count, no negative drift.
- **⚠️ Only valid while log-only.** Once **enforcement** is enabled, discarding
  in-flight risk across a restart becomes an attack surface (restart to wipe a
  peer's accountability). At that point the server MUST enumerate the live
  circuit map ∪ channel HTLC sets and call the parked, already-tested
  `Manager.ReplayInFlight(...)`. Revisit this decision then.

## 7. Config flag

One experimental flag in `lncfg`, default **off** (e.g. `routing.reputation` or
behind a build tag). When off, the switch's `ReputationManager` is nil and all
hooks are no-ops with zero overhead.

## 8. Phasing (10 PR-sized targets) + tests per stage

Every target is log-only, no historical read, off by default, and ships its own
tests so coverage grows monotonically. Each builds strictly on the previous, so
each PR is independently reviewable and leaves `master` green. A
test-injectable **clock** and **`BestBlockHeight()`** getter are introduced in
P1 so every later stage is deterministic. Each target below states its *goal*,
what it *builds*, what it *depends on*, its *boundary* (what it deliberately does
NOT do yet), and its *tests*.

### P1 — Scaffolding & inert seam
- **Goal:** land the package and the switch seam with zero behavioral change, so
  all later phases plug into a stable structure.
- **Builds:** the `reputation/` package; `Config` + `DefaultConfig()` +
  `Validate()`; the `ReputationManager` interface (`OnForward`/`OnSettle`/
  `OnFail` + `Start`/`Stop`) added to `htlcswitch.Config`; the 3 nil-guarded hook
  calls in `switch.go`; a skeleton `Manager` taking an injected clock, a
  `BestBlockHeight()` getter and a logger; the off-by-default `lncfg` flag;
  `server.go` construction gated on the flag.
- **Depends on:** nothing.
- **Boundary:** no state, no math, no buckets — the hooks exist but do nothing.
  Flag off ⇒ `ReputationManager` is nil ⇒ the switch is byte-for-byte unchanged.
- **Tests:** `Config.Validate()` table (bad pct sums, zero windows); compile-time
  `var _ ReputationManager = (*Manager)(nil)`; `Manager` construct/`Start`/`Stop`
  smoke; nil-manager hook calls are no-ops in a switch test (no panic);
  flag-off path constructs a nil manager.

### P2 — Event ingestion & HTLC lifecycle
- **Goal:** turn raw hook calls into a tracked, matched HTLC lifecycle — the
  substrate every later calculation sits on.
- **Builds:** `pendingHTLC`, `htlcRef`; the pending map keyed by
  `(IncomingCircuit, OutgoingCircuit)`; `OnForward` inserts a pending,
  `OnSettle`/`OnFail` match it, compute `resolution_time` from the injected clock,
  and remove it; stale-pending GC (evict past `maximum_hold_seconds`); structured
  logging of every observed event.
- **Depends on:** P1.
- **Boundary:** records and matches only — computes no reputation. Fees/outcomes
  are logged, not scored. An unmatched resolve (a missed add) is logged and
  tolerated, never fatal.
- **Tests:** insert/match/remove units; resolution-time from a controllable
  clock; GC eviction past max-hold; unmatched-resolve no-op; table-driven
  synthetic forward→settle/fail sequences asserting pending-map transitions.

### P3 — Math primitives
- **Goal:** the two decaying-average data structures in isolation, producing
  LDK-identical numbers.
- **Builds:** `DecayingAverage` (`decay_rate = 0.5^(2/window_secs)`,
  `value_at(t)`, `add_value(v,t)`); `aggregatedWindowAverage` (warmup
  `1 − e^(−elapsed/tracked)`, `window_total ≥ 6`). Arithmetic semantics chosen to
  match LDK exactly: `i64` saturating adds, `f64` decay, integer-floor.
- **Depends on:** nothing (pure); must land before P4 consumes it.
- **Boundary:** pure types, not yet referenced by the manager.
- **Tests:** port LDK vectors — decay over elapsed time, half-life at window
  midpoint, `add_value`, monotonic-time guard (backward time errors), warmup
  factor across periods; saturation / large-value edges.

### P4 — Reputation accrual (outgoing channel)
- **Goal:** make resolutions move an outgoing channel's reputation — the core
  unforgeable-history signal.
- **Builds:** `opportunityCost`, `effectiveFee` (the 4-branch matrix),
  `inFlightRisk`; `channelReputation` with `outgoingReputation` (a
  `DecayingAverage`); per-scid channel state created lazily on first event;
  `OnSettle`/`OnFail` now compute `effectiveFee` (from observed accountable +
  `resolution_time`) and call `outgoingReputation.Add(...)`.
- **Depends on:** P2 (lifecycle), P3 (averages).
- **Boundary:** reputation accrues and is logged, but nothing consumes it yet —
  no verdict, no revenue side. `inFlightRisk` is *introduced* here as a pure,
  vector-tested function but is first *consumed* in P5.
- **Tests:** `opportunityCost` exact LDK vectors (10s→0, 91s→1, 135s→50,
  180s→100, 900s→900); `effectiveFee` full matrix (accountable × settled/failed ×
  fast/slow, all 4 branches); `inFlightRisk` from cltv/height; integration test
  asserting `outgoingReputation` moves by the expected `effective_fee` after a
  forward+resolve (clock + height injected).

### P5 — Revenue threshold & sufficiency verdict
- **Goal:** complete the incoming×outgoing comparison that classifies a forward.
- **Builds:** `channelReputation.incomingRevenue` (`aggregatedWindowAverage`),
  updated on settle (`+fee`); `SufficientReputation()` =
  `outgoingReputation − in_flight_risk ≥ incomingRevenue_threshold`, summing
  in-flight risk across the outgoing channel's pending **accountable** HTLCs;
  `OnForward` now computes and logs the per-forward sufficient/insufficient
  verdict.
- **Depends on:** P4.
- **Boundary:** verdict computed and logged; no bucket assignment yet, nothing
  enforced.
- **Tests:** inequality boundary (rep just above/below threshold); in-flight risk
  flipping a sufficient channel to insufficient; revenue accrues on settle only;
  scenario table asserting the logged verdict.

### P6 — Channel feed & general bucket
- **Goal:** introduce the channel-capacity input and the first (and trickiest)
  bucket.
- **Builds:** the channel-lifecycle read seam — a `ChannelSource` interface
  (startup enumeration of `MaxAcceptedHtlcs`/`MaxPendingAmount` +
  `channelnotifier` open/close subscription), wired in `server.go`; per-channel
  bucket sizing (40/20/40 defaults); `generalBucket` with ChaCha20 salted
  per-`(in,out)` slot assignment (nonce `incoming_scid[0:4]||outgoing_scid`,
  `general_slot_allocation = max(5, total*5/100)`, per-channel persisted salt) +
  occupancy tracking; `OnForward`/resolve reserve/release general slots; logs the
  assignment. Pins the holder-vs-remote limit side (match LDK `get_holder_max_*`).
- **Depends on:** P5 (verdict), P1 wiring.
- **Boundary:** general-bucket occupancy tracked + logged only; congestion/
  protected buckets and the full decision tree are P7. Never rejects.
- **Tests:** channel add/remove sizes buckets from injected limits (fake
  `ChannelSource`); slot-assignment determinism (same salt+scids → identical set;
  salt reload → identical, port LDK); low-collision sanity across many channels;
  occupancy reserve-on-add / release-on-resolve.

### P7 — Congestion, protected buckets & decision tree
- **Goal:** complete the resource model and the full "as-if" forwarding decision.
- **Builds:** `bucketResources` for congestion + protected; congestion
  eligibility (one in-flight HTLC per outgoing scid; ~2-week block after a slow
  congestion HTLC; single-HTLC size cap) + misuse tracking; protected reserved
  for sufficient-reputation channels with spillover into other buckets when full;
  the full decision tree → `Outcome{Forward(accountable)|Fail, Bucket}`;
  `OnForward` now assigns every HTLC to a bucket via the tree and logs the full
  decision.
- **Depends on:** P6.
- **Boundary:** complete shadow accounting + decision; still occupancy-only,
  never rejects, never sets the accountable bit.
- **Tests:** decision-tree table over every branch (accountable vs not ×
  reputation suff/insuff × bucket availability → expected `Outcome`+`Bucket`);
  congestion misuse (slow HTLC blocks that outgoing scid ~2 weeks; one-in-flight
  rule); protected spillover into other buckets.

### P8 — Persistence
- **Goal:** make accrued reputation survive a process restart.
- **Builds:** `store.go` — a persistence interface + kvdb impl in its own
  namespace; serialize/reload `outgoingReputation`, `incomingRevenue`,
  general-bucket salts, congestion-misuse timestamps, and channel params; the
  batched ticker flush + flush-on-`Stop`, snapshotting under the lock and writing
  outside it (per §4). `decay_rate` recomputed on read, not stored. NOT
  persisted: pending HTLCs / live occupancy (discarded on restart — see §6/P9).
- **Depends on:** P3–P7 (the state there is to persist).
- **Boundary:** averages durable across restart; in-flight state is not restored.
- **Tests:** round-trip per persisted type; full-store reload == pre-restart
  state (golden); empty / first-run DB read; `decay_rate` recompute on read.

### P9 — Startup reconstruction
- **Goal:** correctly initialize on both first-enable and restart.
- **Builds:** cold start (no persisted state) → empty per-channel state via the
  channel feed (zero reputation everywhere); warm restart → reload persisted
  averages. **In-flight HTLCs are discarded on restart** (pending set starts
  empty; pre-restart resolves no-op) — a deliberate log-only trade-off; see §6.
  `ReplayInFlight(...)` is implemented + unit-tested but **parked / not wired**,
  retained for the future enforcement build. No historical read.
- **Depends on:** P8 (presence/absence of persisted state drives cold-vs-warm).
- **Boundary:** subsystem initialized after a restart with persisted averages but
  an empty pending set; still log-only.
- **Tests:** zero-start builds empty channels for all; warm-restart reloads
  persisted averages → consistent state; `ReplayInFlight` unit-tested in
  isolation (parked path); **end-to-end itest** in `lntest/itest` driving real
  forwards through a 3-hop chain and asserting logged reputation/bucket state
  (log-only, asserts nothing about routing).

### P10 *(optional)* — Operator debug surface
- **Goal:** let operators inspect the shadow state to evaluate pre-enforcement
  behavior against real traffic.
- **Builds:** an `lncli`/RPC (or structured log dump) exposing per-channel
  reputation, revenue threshold, sufficiency verdict, and bucket occupancy;
  optionally a subscription stream of verdicts.
- **Depends on:** P5 (verdict), P7 (buckets).
- **Boundary:** read-only introspection; no behavior change.
- **Tests:** output-shape unit test of the dump command/RPC; light itest if an
  RPC is added.

## 9. Testing philosophy

Coverage target: the `reputation/` package is pure/deterministic by design (clock
and block height injected), so it should reach high unit coverage on its own
(math, buckets, decision tree, persistence round-trips). The htlcswitch surface
is kept to 3 thin hooks specifically so it needs no new switch-level tests beyond
confirming nil-manager no-ops and that hooks fire on forward/settle/fail. The
single integration test (P9 itest) validates the seam end-to-end against a live
node.

## 10. Open questions

- Default config values — adopt LDK's (`resolution_period` 90s,
  `revenue_window` ~2 weeks, `reputation_multiplier` 12, `window_total` 6) vs.
  LND-tuned.
- kvdb namespace: dedicated top-level bucket in `channeldb` vs. a separate store
  owned by the package.
- Whether to expose the log-only verdicts over an RPC subscription for
  operators analysing pre-enforcement behaviour (could fold into P10).
