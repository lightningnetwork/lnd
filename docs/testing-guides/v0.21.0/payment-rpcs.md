# New Payment-Adjacent RPCs — v0.21.0 RC Testing Guide

**PRs:** #10666 (`DeleteForwardingHistory`), #10436 (MuSig2 coordinator nonces), #10296 (`EstimateFee` inputs), #10520 (HTLC event invoice failures), #10543 (`SubscribeChannelEvents` update events)
**Risk:** new-rpc
**Audience:** RPC clients, routing nodes, MuSig2 coordinator integrators, LSPs
**Backends affected:** all
**Networks:** regtest (primary)

## What this feature does

v0.21.0 ships five small RPC additions/updates relevant to payment
and channel flows. They are bundled here because each is too narrow
for its own guide, but each has a clear pass/fail signal worth
checking on the RC.

1. **`router.DeleteForwardingHistory`** (#10666). Operator RPC to
   purge old forwarding events. Cutoff timestamp must be at least 1
   hour in the past.
2. **`MuSig2RegisterCombinedNonce` / `MuSig2GetCombinedNonce`**
   (#10436). Lets a coordinator pre-aggregate MuSig2 nonces
   externally and register the result. MuSig2 v1.0.0rc2 only.
3. **`EstimateFee` with `inputs`** (#10296). Explicit input
   selection for fee estimation; new `inputs` field on
   `EstimateFeeRequest`, new `--utxos` flag on
   `lncli estimatefee`.
4. **HTLC event invoice-level failure detail** (#10520). routerrpc
   HTLC event subscribers now receive specific failure causes for
   invoice-validation failures instead of `UNKNOWN`.
5. **`SubscribeChannelEvents` update events** (#10543).
   `SubscribeChannelEvents` now emits a channel update event for
   state changes, not only open/close/active/inactive.

## Why each matters / what could break

- **`DeleteForwardingHistory`** is destructive. The 1-hour guard
  must hold; off-by-one or misinterpreted timestamps could wipe
  recent data.
- **MuSig2 combined-nonce RPCs** are signing-protocol territory.
  Wrong nonce aggregation produces invalid signatures.
- **`EstimateFee` inputs**: a request that names a non-existent or
  spent UTXO must fail clearly, not silently produce a meaningless
  estimate.
- **HTLC invoice failure detail**: routing nodes that grep failures
  out of subscriber streams will mis-classify if `UNKNOWN` still
  surfaces for invoice-validation cases.
- **`SubscribeChannelEvents` update events**: any client iterating
  over event-type values may need to handle a new variant. If the
  daemon emits a malformed event, subscribers can disconnect or
  crash.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Peers:** Alice and Bob (with a channel for the payment-adjacent
  scenarios); Carol for forwarding-history scenarios so Alice can
  forward Bob → Carol traffic.
- **MuSig2 coordinator client** for S2 — typically the `signrpc`
  test client from `signer/musig2_test.go` or your own.
- **Tools:** `lncli`, `grpcurl`, `jq`, `bitcoin-cli`.

## Scenarios

### S1: `DeleteForwardingHistory` deletes old events; 1-hour guard rejects recent cutoffs

**Goal:** Confirm both the success path and the safety guard.

**Steps:**
```bash
# Drive some forwards through Alice (need a 3-node setup).
# Wait a bit, then query forwarding history.
$LNCLI_A fwdinghistory | jq '.forwarding_events | length' > /tmp/fwd-pre.txt

# Capture a timestamp at least 1h in the past.
CUTOFF_OK=$(date -d '2 hours ago' +%s)         # GNU date
# macOS: CUTOFF_OK=$(date -v-2H +%s)
CUTOFF_BAD=$(date -d '5 minutes ago' +%s)

# Delete old events (success).
grpcurl ... -d "{\"end_time_ns\": \"$((CUTOFF_OK*1000000000))\"}" \
    routerrpc.Router/DeleteForwardingHistory

# Try a recent cutoff (should error).
grpcurl ... -d "{\"end_time_ns\": \"$((CUTOFF_BAD*1000000000))\"}" \
    routerrpc.Router/DeleteForwardingHistory
echo $?
```

(Replace the field name with whatever the proto actually exposes;
verify against `routerrpc.proto` for v0.21.0.)

**Pass/Fail signal:**
- **PASS** if (a) the first call succeeds and `fwdinghistory`
  afterward shows fewer events than before, and (b) the second call
  fails with a clear error message about the 1-hour minimum.
- **FAIL** if the recent-cutoff call succeeds (the safety guard is
  broken).

---

### S2: `MuSig2RegisterCombinedNonce` / `MuSig2GetCombinedNonce` round-trip

**Goal:** A coordinator can register a pre-aggregated combined nonce
and later retrieve it for a session.

**Steps:** Run the coordinator-based MuSig2 flow end-to-end with at
least two signing participants. After the coordinator aggregates
nonces externally, call `MuSig2RegisterCombinedNonce` with the
session ID and combined nonce. From a participant, call
`MuSig2GetCombinedNonce` and verify the returned value.

**Pass/Fail signal:**
- **PASS** if `MuSig2GetCombinedNonce` returns the same bytes
  registered, and the subsequent partial-sign / finalize completes
  with a valid signature.
- **FAIL** if the combined nonce roundtrips wrong, if the finalize
  produces an invalid signature, or if the call rejects MuSig2
  v1.0.0rc2 sessions.

---

### S3: `EstimateFee` with explicit `inputs`

**Goal:** A fee estimate that names specific UTXOs uses those UTXOs;
naming a spent or non-existent UTXO errors cleanly.

**Steps:**
```bash
# Pick two confirmed UTXOs on Alice.
$LNCLI_A listunspent --min_confs=1 | jq '.utxos[] | .outpoint'

# Estimate fee using --utxos.
$LNCLI_A estimatefee --conf_target=6 \
    --utxos="$UTXO1" --utxos="$UTXO2" \
    --addr_to_amount='{"<ADDR>": 50000}'

# Try a bogus UTXO.
$LNCLI_A estimatefee --conf_target=6 \
    --utxos="0000000000000000000000000000000000000000000000000000000000000000:0" \
    --addr_to_amount='{"<ADDR>": 50000}'
echo $?
```

**Pass/Fail signal:**
- **PASS** if the first call returns a fee estimate consistent with
  using exactly those two UTXOs, and the second call fails with a
  clear "input not found" or "not spendable" message.
- **FAIL** if the second call silently returns an estimate
  (ignoring the bad input), or the first uses different UTXOs than
  requested.

---

### S4: HTLC event subscribers see invoice-level failure detail (not `UNKNOWN`)

**Goal:** routerrpc HTLC event stream now provides specific reasons
for invoice-validation failures.

**Steps:**
- Subscribe to `routerrpc.SubscribeHtlcEvents` on Bob (the
  recipient).
- From Alice, attempt to pay a Bob invoice in a way that
  invoice-validation rejects (e.g. expired invoice, wrong
  preimage attempt, amount mismatch). Repeat for each failure mode
  you want to test.
- Capture the failure-detail field from each emitted event.

**Pass/Fail signal:**
- **PASS** if every invoice-validation failure surfaces a specific
  reason (e.g. `INVOICE_EXPIRED`, `INCORRECT_PAYMENT_AMOUNT`,
  `INVOICE_ALREADY_CANCELED`), not the legacy `UNKNOWN`.
- **FAIL** if any of these still emit `UNKNOWN`.

---

### S5: `SubscribeChannelEvents` emits update events on state changes

**Goal:** Confirm the new event variant fires for the
state-change cases it covers.

**Steps:**
- Subscribe to `lnrpc.SubscribeChannelEvents` on Alice via a
  long-running grpcurl session.
- Drive state changes:
  - Open a channel → expect existing `pending_open_channel` and
    `open_channel` events.
  - Push a few payments → expect the new update event(s).
  - Coop-close → expect existing close events.
- Inspect every emitted event's `type` field.

**Pass/Fail signal:**
- **PASS** if at least one event with the new `update` type fires
  during the test window, and the payload references the correct
  channel.
- **FAIL** if no update event fires, or if a malformed event causes
  the subscriber stream to error / disconnect.

## Failure investigation

- **Subsystems:** `RPCS`, `ROUTING`, `CRTR`, `SIGN`.
- **Useful greps:** `DeleteForwardingHistory`,
  `MuSig2RegisterCombinedNonce`, `EstimateFee`, `HtlcEvent`,
  `ChannelEvent`.
- **Proto-level surface:** check
  `lnrpc/routerrpc/router.proto` (forwarding history, HTLC events),
  `lnrpc/signrpc/signer.proto` (MuSig2 coordinator), and
  `lnrpc/lightning.proto` (`EstimateFee`, `SubscribeChannelEvents`)
  to confirm field names match the calls above before recording a
  FAIL.

## Related itests

- Each of these RPCs typically has a corresponding itest. Verify in
  `itest/` (e.g. `itest/lnd_forward_test.go`,
  `itest/lnd_musig2_test.go`).

## Out of scope

- Payment SQL migration — separate guide
  ([`payment-sql-migration.md`](./payment-sql-migration.md)).
- BOLT-12 / offers — not in v0.21.0.
- Performance characteristics of `EstimateFee` with many inputs.
