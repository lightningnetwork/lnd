# RBF Cooperative Close on Taproot Channels — v0.21.0 RC Testing Guide

**PRs:** #10063 (RBF coop close + taproot/MuSig2), #10763 (overlay narrowing)
**Risk:** headline
**Audience:** node operators, channel-acceptor clients
**Backends affected:** all
**Networks:** regtest (primary), signet

> ⚠️ **Authoring note:** Please verify the exact CLI mechanism for
> triggering successive RBF iterations on a coop close (re-running
> `closechannel` with a higher fee vs. a dedicated bump RPC) against
> the latest behavior before publishing. The scenarios below assume
> re-running `closechannel` re-enters the state machine and produces
> a new `ClosingComplete`.

## What this feature does

v0.21.0 extends the RBF cooperative close protocol
(`--protocol.rbf-coop-close`, introduced earlier) to simple taproot
channels. Each RBF iteration produces a fresh `ClosingComplete` /
`ClosingSig` pair with new MuSig2 partial signatures, using the JIT
(just-in-time) nonce pattern: the closer's nonce is bundled with its
signature in `ClosingComplete`, and the closee rotates its nonce via
`NextCloseeNonce` in `ClosingSig` for every round. The state machine
stores the `MusigPartialSig` and invalidates nonces after each
signing round to prevent reuse.

A follow-up (#10763) narrows the RBF coop-close auto-enable to
*skip* taproot-overlay channels, since the RBF close state machine
does not yet thread through the `AuxCloser` hook overlay channels
rely on.

## Why it matters / what could break

- **Nonce reuse across RBF rounds** is a hard MuSig2 violation — if
  it happens, signatures become forgeable. Watch for it.
- A regression on the JIT nonce ordering surfaces as repeated
  `ClosingComplete` round-trips that never produce a confirming
  transaction.
- Taproot-overlay channels accidentally entering the RBF flow will
  produce nil-pointer dereferences or aux-close build failures.
- A non-taproot peer that signals `--protocol.rbf-coop-close` should
  still complete a normal (non-MuSig2) RBF coop close; regressions
  in the channel-type dispatch could break that path too.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer, on both peers.
- **Backend:** `bitcoind` (regtest); a real fee market makes this
  easier to test on signet.
- **Peers:** Alice and Bob, both started with:
  ```
  protocol.simple-taproot-chans=1
  protocol.rbf-coop-close=1
  ```
- **Tools:** `lncli`, `bitcoin-cli`, `jq`.

Shell variables: same as
[`production-taproot-channels.md`](./production-taproot-channels.md).

## Setup

```bash
# 1. With both nodes started under the prerequisites, open a
#    production taproot channel from Alice to Bob.
$LNCLI_A openchannel \
    --node_key=$BOB_PUB \
    --local_amt=5000000 \
    --channel_type=taproot-final

bitcoin-cli -regtest generatetoaddress 6 $ADDR

CP=$($LNCLI_A listchannels | \
    jq -r '.channels[] | select(.remote_pubkey=="'$BOB_PUB'") | .channel_point')
echo "channel_point=$CP"

# Setup verification:
$LNCLI_A listchannels | \
    jq '.channels[] | select(.remote_pubkey=="'$BOB_PUB'") | .commitment_type'
# Expected: "SIMPLE_TAPROOT_FINAL"
```

## Scenarios

### S1: First-round RBF coop close on a taproot channel

**Goal:** A single `closechannel` call on a `taproot-final` channel
produces a `ClosingComplete` / `ClosingSig` exchange with MuSig2
partial signatures and a valid closing tx in the mempool.

**Steps:**
```bash
# Stop generating blocks; we want the close tx to sit in mempool.
$LNCLI_A closechannel \
    --funding_txid=${CP%:*} --output_index=${CP##*:} \
    --sat_per_vbyte=2 \
    --max_fee_rate=200 &

# Wait briefly for the exchange.
sleep 5

# Inspect mempool for the closing tx.
bitcoin-cli -regtest getrawmempool | jq 'length'
```

**Pass/Fail signal:**
- **PASS** if `getrawmempool` shows exactly 1 transaction and Alice's
  log contains a `ClosingComplete` message sent to Bob.
- **FAIL** if no tx in mempool after 10s, or Alice's log shows
  `unable to derive musig partial sig` or
  `received nonce of wrong type`.

---

### S2: RBF bump produces a new closing tx with a higher fee

**Goal:** Trigger a second round. The new closing tx must
(a) double-spend the first, (b) use a higher fee, and (c) be signed
with **different** MuSig2 nonces.

**Steps:**
```bash
# Capture the first closing tx fee.
TXID1=$(bitcoin-cli -regtest getrawmempool | jq -r '.[0]')
FEE1=$(bitcoin-cli -regtest getmempoolentry $TXID1 | jq -r '.fees.base')

# Trigger a second round at a higher fee rate.
$LNCLI_A closechannel \
    --funding_txid=${CP%:*} --output_index=${CP##*:} \
    --sat_per_vbyte=10 \
    --max_fee_rate=200 &

sleep 5

TXID2=$(bitcoin-cli -regtest getrawmempool | jq -r '.[0]')
FEE2=$(bitcoin-cli -regtest getmempoolentry $TXID2 | jq -r '.fees.base')
```

**Pass/Fail signal:**
- **PASS** if all three hold:
  - `$TXID2 != $TXID1` (new tx),
  - `$FEE2 > $FEE1` (higher fee),
  - Bob's log contains a `NextCloseeNonce` field on the new `ClosingSig`
    that differs from the previous round's nonce.
- **FAIL** if the txid is unchanged, fee did not increase, or the
  nonce on `ClosingSig` is reused across rounds (this is the
  critical bug to catch — search Bob's log for the previous-round
  nonce hex and confirm it does *not* reappear).

---

### S3: Final close confirms

**Steps:**
```bash
bitcoin-cli -regtest generatetoaddress 6 $ADDR
sleep 2
$LNCLI_A listchannels | \
    jq '.channels[] | select(.remote_pubkey=="'$BOB_PUB'")' | jq length
```

**Pass/Fail signal:**
- **PASS** if the channel is no longer in `listchannels` on either
  side and the second-round tx confirmed on chain.
- **FAIL** if the channel is still listed, or the closing tx in the
  mined block does not match `$TXID2` (an older round confirmed —
  fee-bump replacement failed).

---

### S4: Taproot-overlay channel must NOT auto-enable RBF coop close

**Goal:** Regression guard for #10763. If a taproot-overlay channel
enters the RBF coop close path, the auxiliary close hook will not
fire and the close will misbehave.

**Steps:**
- Start Alice and Bob with
  `protocol.simple-taproot-overlay-chans=1` (in addition to RBF
  coop), and an aux-close client registered.
- Open a taproot-overlay channel.
- Initiate a coop close.

**Pass/Fail signal:**
- **PASS** if the close completes via the legacy (non-RBF) coop close
  path — verified by log line `using legacy coop close` (or similar)
  on the closer side, and the `AuxCloser` hook being invoked.
- **FAIL** if Alice's log shows the RBF state machine being entered
  for an overlay channel, or the close transaction is built without
  the aux-close additions.

---

### S5: Non-taproot peer over RBF coop close (cross-check)

**Goal:** Confirm RBF coop close still works on a non-taproot channel
when both peers signal `--protocol.rbf-coop-close`. Catches dispatch
regressions in the taproot/MuSig2 vs. legacy code split.

**Steps:**
- Open an `anchors` channel (default) between Alice and Bob.
- Run S1 + S2 again on this channel.

**Pass/Fail signal:**
- **PASS** if both rounds complete and the fee-bumped tx replaces
  the original in mempool — same as S2, but no MuSig2 logs expected.
- **FAIL** if the close hangs or errors with a MuSig2-related
  message on a non-taproot channel.

## Failure investigation

- **Logs (Alice and Bob):** set `PEER`, `LNWL`, `CRTR` to `debug`.
  Grep for `closing_complete`, `closing_sig`, `musig`,
  `NextCloseeNonce`, `ClosingComplete`.
- **State machine state:** `lncli pendingchannels` —
  `pending_force_closing_channels` and `waiting_close_channels` reflect
  intermediate states.
- **Nonce reuse detection:** dump each round's nonce hex from logs;
  any repeat across rounds for the same channel is a bug.

## Related itests

- `itest/lnd_rbf_coop_test.go` (or equivalent — verify the exact file
  in v0.21.0).
- `peer/musig_nonce_order_test.go` for nonce-ordering unit coverage.

## Out of scope

- Plain (non-RBF) cooperative close on taproot — see
  [`production-taproot-channels.md` scenario S5](./production-taproot-channels.md).
- Force close (unilateral) on taproot.
- Splice — not in v0.21.0.
