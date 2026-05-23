# Reorg-Safe Channel Closes + `MinCLTVDelta` Change — v0.21.0 RC Testing Guide

**PRs:** #10331 (reorg-safe closes + MinCLTVDelta raise), #10509 (new PendingChannels fields)
**Risk:** high-regression
**Audience:** node operators, integrators with custom CLTV invoice flows, RPC clients tracking close progress
**Backends affected:** all
**Networks:** regtest (primary), signet, mainnet

## What this feature does

Two coupled changes in v0.21.0 alter the close lifecycle:

1. **Reorg protection on channel closes** (#10331). Previously, any
   channel close was considered final the moment the spending
   transaction was detected. v0.21.0 now waits between 3 and 6
   confirmations before resolving a channel as closed, scaled
   linearly with channel capacity up to the non-wumbo maximum
   (~0.168 BTC). Wumbo channels always require 6 confirmations.

2. **New `PendingChannels` fields** (#10509). `WaitingCloseChannel`
   now exposes `blocks_til_close_confirmed` (countdown) and
   `close_height` (block height the close-tx was first confirmed
   at), so clients can render progress.

The MinCLTVDelta bump (also in #10331) is the breaking part:

3. **`MinCLTVDelta` raised from 18 to 24**, providing more safety
   margin above `DefaultFinalCltvRejectDelta` (19 blocks). Custom
   CLTV deltas in the 18–23 range on `addinvoice` are now rejected.
   The default of 80 is unchanged. Existing invoices created on
   prior versions continue to work normally.

## Why it matters / what could break

- A client that polls `closedchannels` and expects entries to
  appear immediately on spend will now see a multi-block lag.
- A client that uses `WaitingCloseChannel` but doesn't render the
  new fields will under-inform users (cosmetic).
- An RPC client or wallet that always passes a custom
  `cltv_expiry_delta` (e.g. 20) on `addinvoice` will now get
  rejection errors at the daemon. Watch for surprised integrators.
- Wallets that decode incoming invoices created before the upgrade
  with `cltv_expiry_delta < 24` must still honor them — verify the
  payee/sender side is permissive even though the issuer side is now
  strict.
- If the scaling math regresses (wrong conf count chosen for a given
  capacity), the close finalizes too early or too late.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Backend:** `bitcoind` regtest. Reorgs on regtest are scripted
  via `invalidateblock`/`reconsiderblock`.
- **Peers:** Alice and Bob; a third node Carol for the MinCLTV
  test (so the rejection error path is reachable end-to-end).
- **Tools:** `lncli`, `bitcoin-cli`, `jq`.

## Setup

```bash
# 1. Open three channels Alice ↔ Bob of different capacities to
#    exercise the scaling logic:
#    - small:  500_000 sat
#    - medium: 5_000_000 sat
#    - wumbo:  20_000_000 sat (requires --protocol.wumbo-channels)
$LNCLI_A openchannel --node_key=$BOB_PUB --local_amt=500000
$LNCLI_A openchannel --node_key=$BOB_PUB --local_amt=5000000
$LNCLI_A openchannel --node_key=$BOB_PUB --local_amt=20000000  # wumbo

bitcoin-cli -regtest generatetoaddress 6 $ADDR
```

## Scenarios

### S1: Small channel — close requires ~3 confirmations

**Goal:** A 500k-sat channel close lingers in `waiting_close_channels`
until the scaled-conf threshold is reached.

**Steps:**
```bash
# Pick the 500k channel.
CP=$($LNCLI_A listchannels | \
    jq -r '.channels[] | select(.capacity=="500000") | .channel_point')

$LNCLI_A closechannel \
    --funding_txid=${CP%:*} --output_index=${CP##*:} \
    --sat_per_vbyte=2

# Mine 1 block (close-tx confirms).
bitcoin-cli -regtest generatetoaddress 1 $ADDR

# Inspect waiting-close state.
$LNCLI_A pendingchannels | jq '.waiting_close_channels[]'
```

**Pass/Fail signal:**
- **PASS** if (a) the channel is in `waiting_close_channels`, (b)
  `close_height` equals the block height the close-tx confirmed at,
  (c) `blocks_til_close_confirmed` equals roughly 3 (the lower
  scaled-conf bound for a small channel), and (d) after mining 2
  more blocks the channel moves to `closedchannels`.
- **FAIL** if the channel finalizes immediately (no waiting state)
  or if `blocks_til_close_confirmed` is missing/zero from the start.

---

### S2: Mid-size channel — close requires more confs than S1

**Steps:** Same as S1 against the 5M-sat channel.

**Pass/Fail signal:**
- **PASS** if `blocks_til_close_confirmed` is strictly greater than
  S1's value (the scaling actually scales) and bounded above by 6.
- **FAIL** if it's identical to S1's value (no scaling), or > 6.

---

### S3: Wumbo channel — close always requires 6 confirmations

**Steps:** Same as S1 against the 20M-sat channel.

**Pass/Fail signal:**
- **PASS** if `blocks_til_close_confirmed` is exactly 6 the moment
  the close-tx confirms.
- **FAIL** if anything other than 6.

---

### S4: Force-close also respects the new conf requirement

**Goal:** Reorg protection applies to unilateral closes too, not
just coop closes.

**Steps:**
- Open another small channel; have Alice force-close it.
- Inspect `pending_force_closing_channels` after 1 conf.

**Pass/Fail signal:**
- **PASS** if the force-close also stays pending for the scaled
  number of confs.
- **FAIL** if force-close finalizes immediately on first
  confirmation (regression — half the fix doesn't help if the other
  half is missing).

---

### S5: Reorg before close-conf threshold rewinds the close

**Goal:** A reorg that re-disconfirms the close-tx before the
threshold should rewind the channel to "still open" rather than
incorrectly considering it closed.

**Steps:**
```bash
# Coop-close a channel; mine 1 confirmation only.
$LNCLI_A closechannel --funding_txid=... --output_index=... --sat_per_vbyte=2
bitcoin-cli -regtest generatetoaddress 1 $ADDR

# Reorg the block out.
TIP=$(bitcoin-cli -regtest getbestblockhash)
bitcoin-cli -regtest invalidateblock $TIP

# Mine a competing block (no close-tx).
bitcoin-cli -regtest generatetoaddress 1 $OTHER_ADDR
```

**Pass/Fail signal:**
- **PASS** if Alice's daemon notices the disconfirmation (log
  contains `reorg detected` or similar) and `pendingchannels`
  reports the close as no longer confirmed (e.g. `close_height`
  cleared or the channel reverts to active). Then re-mining the
  close-tx re-progresses the close.
- **FAIL** if the close stays "almost final" despite the reorg
  invalidating it, or if Alice's chain-watch panics.

---

### S6: `MinCLTVDelta` rejects new invoices with custom delta 18–23

**Goal:** The breaking-change side of #10331. Verify both the
rejection path and the error message.

**Steps:**
```bash
# Default delta: should succeed.
$LNCLI_A addinvoice --amt=1000 --cltv_expiry_delta=80 | jq -r '.payment_request' > /tmp/inv-ok.txt
echo $?

# Custom delta in the new-rejected band: should fail.
$LNCLI_A addinvoice --amt=1000 --cltv_expiry_delta=20
echo $?
```

**Pass/Fail signal:**
- **PASS** if delta=80 succeeds and delta=20 fails with a clear
  error message naming the new minimum (24). Non-zero exit code on
  the 20 call.
- **FAIL** if delta=20 silently accepts (the breaking change didn't
  land), or if the error message is opaque (doesn't help operators
  fix their config).

---

### S7: Existing pre-upgrade invoices with delta < 24 still pay

**Goal:** Backwards-compatibility on the receiving side — a node
running v0.21 must still settle an invoice with `cltv_expiry_delta=20`
that was issued by a pre-0.21 node (or by an external invoice
generator).

**Steps:**
- From a pre-0.21 build of Bob (or from any node still on v0.20),
  generate an invoice with `cltv_expiry_delta=20`.
- Pay it from Alice (running v0.21).

**Pass/Fail signal:**
- **PASS** if the payment succeeds; final hop accepts the HTLC.
- **FAIL** if Alice's payer side rejects, or if Bob's older node
  fails to accept the inbound HTLC due to a v0.21-injected delta.

## Failure investigation

- **Subsystems:** `CRTR`, `CNCT`, `HSWC`, `BCST`.
- **Useful log lines:** `reorg`, `close confirmation`, `cltv_expiry`,
  `blocks_til`.
- **State to query:** `pendingchannels`, `closedchannels`,
  `decodepayreq <bolt11>` to inspect a captured invoice's delta.
- **Scaling math regression:** if S1/S2/S3 don't show monotonically
  increasing conf counts, dump the capacity-to-conf mapping from
  the daemon's log at close time and cross-check against #10331.

## Related itests

- `itest/lnd_channel_force_close_test.go` and
  `itest/lnd_channel_open_test.go` — close path coverage.
- `itest/lnd_payment_test.go` — invoice-delta validation.

## Out of scope

- Force-close fee-bumping behavior — see `bumpforceclosefee` flows,
  unrelated to this guide.
- The closed-channel tombstone on sqlite/postgres — separate guide
  ([`closed-channel-tombstone.md`](./closed-channel-tombstone.md)).
- The legacy non-scaled close logic (no longer present in v0.21.0).
