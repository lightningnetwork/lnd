# Production Simple Taproot Channels — v0.21.0 RC Testing Guide

**PRs:** #9985 (production support), #10763 (acceptor + RBF coop follow-up), #10672 (private-taproot funding script bug fix)
**Risk:** headline
**Audience:** node operators, LSPs, wallet integrators, channel-acceptor clients
**Backends affected:** all
**Networks:** regtest (primary), signet, mainnet

## What this feature does

v0.21.0 adds the production (final) variant of simple taproot
channels, negotiated via feature bits 80/81. Production taproot
channels use a more optimized commitment script
(`OP_CHECKSIGVERIFY` instead of `OP_CHECKSIG` + `OP_DROP`) and encode
MuSig2 nonces in `channel_reestablish` and `revoke_and_ack` as a map
keyed by the funding TXID.

The nonce type used by a channel is auto-detected from the negotiated
channel type, not the peer's advertised feature bits. The RPC
channel acceptor now also reports production taproot opens with the
`SIMPLE_TAPROOT_FINAL` commitment type across every combination of
the `scid-alias` and `zero-conf` modifiers.

## Why it matters / what could break

- Misnegotiation between staging-taproot (existing) and final-taproot
  (new) peers → channel open fails or, worse, opens with mismatched
  script versions and force-closes at first commitment.
- Wrong nonce encoding on `channel_reestablish` after a reconnect →
  peers cannot resume the channel; surface as repeated reconnect
  loops with `channel_reestablish` errors in the log.
- Channel-acceptor clients seeing `UNKNOWN_COMMITMENT_TYPE` instead
  of `SIMPLE_TAPROOT_FINAL` for production taproot opens with
  `scid-alias` or `zero-conf` modifiers (the bug #10763 fixed —
  guard against regression).
- Private taproot channels with a v1 gossip entry whose funding
  script gets reconstructed as legacy P2WSH on restart (#10672).
  Surfaces as missed-spend detection.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Backend:** `bitcoind` (regtest).
- **Peers:** Alice and Bob, both started with `--protocol.simple-taproot-chans`.
- **Tools:** `lncli`, `bitcoin-cli`, `jq`.

Shell variables used below:
```
ALICE_RPC=localhost:10001
BOB_RPC=localhost:10002
ALICE_MAC=~/.alice/data/chain/bitcoin/regtest/admin.macaroon
BOB_MAC=~/.bob/data/chain/bitcoin/regtest/admin.macaroon
ALICE_DIR=~/.alice
BOB_DIR=~/.bob
LNCLI_A="lncli --rpcserver=$ALICE_RPC --macaroonpath=$ALICE_MAC --lnddir=$ALICE_DIR"
LNCLI_B="lncli --rpcserver=$BOB_RPC --macaroonpath=$BOB_MAC --lnddir=$BOB_DIR"
```

Both nodes must run with at least:
```
protocol.simple-taproot-chans=1
```

## Setup

```bash
# 1. Start a clean bitcoind in regtest and mine a few blocks.
bitcoin-cli -regtest createwallet test
ADDR=$(bitcoin-cli -regtest getnewaddress)
bitcoin-cli -regtest generatetoaddress 200 $ADDR

# 2. Start Alice and Bob with --protocol.simple-taproot-chans.
#    (Use your usual two-node regtest setup. The flag matters.)

# 3. Connect Alice to Bob.
BOB_PUB=$($LNCLI_B getinfo | jq -r '.identity_pubkey')
$LNCLI_A connect $BOB_PUB@127.0.0.1:9736

# 4. Fund Alice's on-chain wallet.
ALICE_ADDR=$($LNCLI_A newaddress p2tr | jq -r '.address')
bitcoin-cli -regtest sendtoaddress $ALICE_ADDR 1
bitcoin-cli -regtest generatetoaddress 6 $ADDR

# Setup verification:
$LNCLI_A walletbalance | jq '.confirmed_balance'
# Expected: "100000000" (1 BTC in sats)
```

## Scenarios

### S1: Open a production (final) taproot channel — happy path

**Goal:** Verify that a `taproot-final` channel opens, confirms, and
reports `SIMPLE_TAPROOT_FINAL` as its commitment type.

**Steps:**
```bash
$LNCLI_A openchannel \
    --node_key=$BOB_PUB \
    --local_amt=5000000 \
    --channel_type=taproot-final

# Mine to confirm.
bitcoin-cli -regtest generatetoaddress 6 $ADDR
```

**Expected:**
- `openchannel` returns a funding-txid; no error.
- After 6 confirmations, the channel appears in `listchannels` on
  both sides with `commitment_type == "SIMPLE_TAPROOT_FINAL"`.

**Pass/Fail signal:**
```bash
$LNCLI_A listchannels | \
    jq '.channels[] | select(.remote_pubkey=="'$BOB_PUB'") | .commitment_type'
```
- **PASS** if the output is `"SIMPLE_TAPROOT_FINAL"`.
- **FAIL** if `"SIMPLE_TAPROOT"` (staging), `"ANCHORS"`, or any
  other value — that means negotiation fell back to the wrong type.

---

### S2: Reconnect a production taproot channel — `channel_reestablish` round-trip

**Goal:** Confirm the map-based nonce encoding keyed by funding TXID
survives a reconnect. This is the most likely place for production
taproot to regress, because nonce-type detection now flows from the
negotiated channel type instead of peer feature bits.

**Steps:**
```bash
# With the S1 channel up, disconnect and reconnect Bob.
$LNCLI_A disconnect $BOB_PUB
sleep 2
$LNCLI_A connect $BOB_PUB@127.0.0.1:9736
sleep 3
```

**Expected:**
- Reconnection completes.
- `listpeers` shows Bob back as connected.
- The channel from S1 still reports `active: true`.

**Pass/Fail signal:**
```bash
$LNCLI_A listchannels | \
    jq '.channels[] | select(.remote_pubkey=="'$BOB_PUB'") | .active'
```
- **PASS** if the output is `true`.
- **FAIL** if `false`, or if Alice's log contains
  `unable to handle upstream reestablish msg` or
  `received nonce of wrong type` — the nonce-type auto-detection
  regressed.

---

### S3: Send a payment over a production taproot channel

**Goal:** End-to-end HTLC settlement on the new commitment type.

**Steps:**
```bash
INV=$($LNCLI_B addinvoice --amt=10000 | jq -r '.payment_request')
$LNCLI_A payinvoice --force $INV
```

**Pass/Fail signal:**
```bash
$LNCLI_A listpayments | jq '.payments[-1].status'
```
- **PASS** if the output is `"SUCCEEDED"`.
- **FAIL** otherwise (in particular `"IN_FLIGHT"` for more than ~10s
  on regtest indicates a stuck HTLC).

---

### S4: RPC channel acceptor reports `SIMPLE_TAPROOT_FINAL`

**Goal:** Regression guard for #10763 — the acceptor must report
production taproot opens with the correct commitment type for every
combination of scid-alias and zero-conf modifiers, not
`UNKNOWN_COMMITMENT_TYPE`.

**Steps:**
- Register an RPC channel acceptor against Bob (any external client
  using `lnrpc.Lightning.ChannelAcceptor` bidi stream). Have it log
  the `ChannelAcceptRequest.commitment_type` field and accept.
- From Alice, open four channels in turn, each with a different
  combination of flags on `openchannel`:
  1. `--channel_type=taproot-final`
  2. `--channel_type=taproot-final --zero_conf`
  3. `--channel_type=taproot-final --scid_alias`
  4. `--channel_type=taproot-final --zero_conf --scid_alias`

  (Zero-conf and SCID-alias also require the relevant `--protocol.*`
  flags and `--protocol.option-scid-alias` on both nodes; consult
  [`docs/zero_conf_channels.md`](../../zero_conf_channels.md).)

**Pass/Fail signal:**
- **PASS** if the acceptor logs `commitment_type == SIMPLE_TAPROOT_FINAL`
  for all four opens.
- **FAIL** if any open shows `UNKNOWN_COMMITMENT_TYPE`,
  `SIMPLE_TAPROOT` (staging), or anything else.

---

### S5: Cooperative close (non-RBF) of a production taproot channel

**Goal:** Plain coop close still works on `taproot-final`. RBF
coop-close is covered separately in
[`rbf-taproot-coop-close.md`](./rbf-taproot-coop-close.md).

**Steps:**
```bash
CP=$($LNCLI_A listchannels | \
    jq -r '.channels[] | select(.remote_pubkey=="'$BOB_PUB'") | .channel_point')
$LNCLI_A closechannel --funding_txid=${CP%:*} --output_index=${CP##*:}
bitcoin-cli -regtest generatetoaddress 6 $ADDR
```

**Pass/Fail signal:**
- **PASS** if the channel disappears from `listchannels` on both
  sides and the close-tx is mined.
- **FAIL** if the close hangs, force-closes instead of cooperating,
  or the close transaction fails to relay.

## Failure investigation

- **Logs to grep on either side:**
  - `grep -iE "taproot|musig|reestablish" ~/.alice/logs/bitcoin/regtest/lnd.log`
  - Subsystems to set to `debug`: `PEER`, `CNCT`, `HSWC`, `LNWL`.
- **Channel-state introspection:**
  - `lncli listchannels --include_channel_status_flags` — look at
    `commitment_type` and `local_chan_reserve_sat`.
  - `lncli pendingchannels` — for in-flight opens, check
    `commitment_type` matches what was requested.
- **Prior regressions to watch for:**
  - #10672 — private taproot channels with v1 gossip rebuilding
    their funding script as legacy P2WSH on restart. Surfaces as
    failed spend detection during force-close.
  - #10763 — acceptor `UNKNOWN_COMMITMENT_TYPE` for production
    taproot + scid-alias/zero-conf combinations.

## Related itests

- `itest/lnd_open_channel_test.go` — taproot open paths.
- `itest/lnd_taproot_test.go` — taproot-specific HTLC and close flows.
- `itest/lnd_channel_force_close_test.go` — force-close on taproot.

## Out of scope

- RBF cooperative close on taproot — see
  [`rbf-taproot-coop-close.md`](./rbf-taproot-coop-close.md).
- Splice on taproot channels — not in v0.21.0.
- Taproot overlay channels (`--protocol.simple-taproot-overlay-chans`)
  — separate commitment type, not the focus of this guide.
