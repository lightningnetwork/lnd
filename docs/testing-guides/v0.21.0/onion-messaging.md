# Onion Messaging + Rate Limiting — v0.21.0 RC Testing Guide

**PRs:** #9868, #10089 (basic forwarding), #10612 (pathfinding), #10713 (rate limiting + channel-presence gate), #10754 (loopback drop)
**Risk:** headline
**Audience:** node operators, routing-node operators
**Backends affected:** all
**Networks:** regtest (primary), signet

For the design and configuration model behind onion-message rate
limiting, read
[`docs/onion_message_rate_limiting.md`](../../onion_message_rate_limiting.md)
first. This guide tests the operational behavior it describes.

## What this feature does

v0.21.0 adds basic support for peer-to-peer onion message
**forwarding**. lnd does not yet ship a user-facing tool for
**constructing** onion messages from the operator side — the
`SendOnionMessage` RPC exists but takes pre-built `path_key`/`onion`
bytes and has no `lncli` wrapper. End-to-end RC testing of onion
messaging therefore relies on placing an lnd node **between two
non-lnd nodes** (Core Lightning, Eclair) that *do* expose
construct-and-send commands. The lnd node is the system under test;
the non-lnd nodes are drivers.

Incoming onion messages on lnd pass through three defenses, in
order:

1. **Loopback drop** (#10754): if the resolved next hop is the same
   peer the message arrived from, drop it.
2. **Channel-presence gate** (#10713): drop messages from peers
   without at least one fully open channel, unless
   `protocol.onion-msg-relay-all=true`.
3. **Token-bucket rate limiters** (#10713): per-peer and global,
   byte-denominated, applied in series (per-peer first).

## Why it matters / what could break

- **Channel-presence gate** is the Sybil defense. A regression
  here lets cheap-identity attackers burn forwarding resources for
  free.
- **Rate limiters** cap operator-borne bandwidth cost. A
  regression in the byte-denominated accounting or the per-peer /
  global ordering reintroduces the asymmetry the feature exists to
  prevent.
- **Loopback drop** closes a traffic-amplification vector — a
  missed drop means a hostile peer can bounce messages back at us.
- **Startup validation** (`burst < 65535 bytes`, partial-zero
  configurations rejected) — silently accepting an invalid config
  would leave operators thinking they had protection.

## Prerequisites

- **lnd build:** v0.21.0-beta.rc1 or newer.
- **Topology:**
  ```
  Eclair (A, sender) ── lnd (SUT) ── Eclair / CLN (C, recipient)
  ```
  with optional **Eclair (B, no-channel peer)** connected to lnd
  for the channel-presence gate scenarios.
- **Primary driver: Eclair**, because `sendonionmessage` is a
  single high-level call that takes a route + hex message and
  builds the onion internally:
  ```bash
  eclair-cli sendonionmessage \
      --nodeIds=<lnd_pubkey>,<recipient_pubkey> \
      --message=<hex>
  ```
  Reference: <https://acinq.github.io/eclair/#sendonionmessage>.
- **Alternative driver: Core Lightning (v24.11+).** Two-step:
  1. `lightning-cli createonion --hops='[...]' --assocdata=00...0
     --onion_size=<om_size>` builds a Sphinx packet for the route.
     Reference:
     <https://docs.corelightning.org/reference/createonion>.
  2. `lightning-cli injectonionmessage --path_key=<pk>
     --message=<hex>` causes CLN to behave as if it had just
     received the onion from a peer, unwrapping and forwarding it
     to the next hop (lnd, in our topology). Reference:
     <https://docs.corelightning.org/reference/injectonionmessage>.
- **Tools:** `lncli`, `eclair-cli`, `lightning-cli` (optional),
  `bitcoin-cli`, `jq`.

Shell variables:
```
LND_PUBKEY=$(lncli getinfo | jq -r '.identity_pubkey')
A_PUBKEY=$(eclair-cli getinfo | jq -r '.nodeId')        # sender
B_PUBKEY=...                                            # no-channel sender
C_PUBKEY=...                                            # recipient
```

## Setup

```bash
# 1. Start all nodes. lnd with default onion-msg limits.
# 2. Connect peers:
#       Eclair A  ↔ lnd
#       Eclair B  ↔ lnd  (no channel — for S2)
#       lnd       ↔ Recipient C
#    Use eclair-cli connect / lncli connect.
# 3. Open and confirm channels A↔lnd and lnd↔C.
#    Do NOT open B↔lnd.

# Setup verification on lnd:
lncli listchannels | jq '.channels | length'                # ≥ 2
lncli listpeers    | jq '.peers   | length'                 # ≥ 3 (A, B, C)
grep -E "onion-msg|OnionMsg" ~/.lnd/logs/*/lnd.log | head   # confirm config loaded
```

## Scenarios

### S1: Forward an onion message through lnd — happy path

**Goal:** A driver-built onion message from Eclair A travels A →
lnd → C and is received by C with no drops on lnd.

**Steps:**
```bash
# Eclair builds and sends a 2-hop onion message: A → lnd → C.
eclair-cli sendonionmessage \
    --nodeIds=$LND_PUBKEY,$C_PUBKEY \
    --message=$(printf 'hello' | xxd -p)
```

**Pass/Fail signal:**
- **PASS** if recipient C logs receipt of an onion message in the
  same time window (Eclair logs `Received onion message`; for CLN
  recipients, the `onion_message_recv` hook fires). lnd's log
  shows no drop or rate-limit events.
- **FAIL** if C receives nothing, or lnd logs a
  `channel-presence-gate` drop (A has a channel — the gate must
  not trip), or a rate-limit log appears under default
  settings for a single small message.

---

### S2: Channel-presence gate drops a no-channel sender

**Goal:** Eclair B is connected to lnd but has no channel; its
onion message is dropped at lnd's gate.

**Steps:**
```bash
# From Eclair B (no channel with lnd), attempt a send through lnd.
eclair-cli -a $B_AUTH sendonionmessage \
    --nodeIds=$LND_PUBKEY,$C_PUBKEY \
    --message=$(printf 'should-drop' | xxd -p)
```

**Pass/Fail signal:**
- **PASS** if (a) C does not receive the message and (b) lnd's log
  records a drop attributable to the channel-presence gate (search
  for `no fully open channel` or the equivalent log key — verify
  exact wording against the v0.21.0 build).
- **FAIL** if C receives the message (gate broken) or no drop log
  appears for B's pubkey (silent drop with no audit trail).

---

### S3: `relay-all` bypasses the channel-presence gate

**Goal:** With `protocol.onion-msg-relay-all=true`, the no-channel
sender from S2 is no longer gated. Rate limiters still apply.

**Steps:**
```bash
# Restart lnd with: protocol.onion-msg-relay-all=true
# Re-run the S2 send.
eclair-cli -a $B_AUTH sendonionmessage \
    --nodeIds=$LND_PUBKEY,$C_PUBKEY \
    --message=$(printf 'now-allowed' | xxd -p)
```

**Pass/Fail signal:**
- **PASS** if C receives the message and lnd's log no longer
  records a channel-presence drop. A per-peer rate-limiter
  bucket should now exist for B's pubkey (verify by tripping it,
  per S4, with B as the sender).
- **FAIL** if lnd still drops at the gate (escape hatch broken) or
  if rate limiting is also bypassed when only the gate should be.

---

### S4: Per-peer rate limiter trips

**Goal:** Eclair A hammering lnd over its per-peer cap should get
dropped once the bucket empties, with a one-shot info log.

**Steps:**
```bash
# Restart lnd with a tight per-peer cap:
#   protocol.onion-msg-peer-kbps=100
#   protocol.onion-msg-peer-burst-bytes=65540

# Fire onion messages near the spec maximum as fast as the script
# can drive Eclair. Use a payload that pads close to 32 KiB so each
# message debits the bucket substantially.
PAYLOAD=$(head -c 32000 /dev/urandom | xxd -p | tr -d '\n')
for i in $(seq 1 50); do
    eclair-cli sendonionmessage \
        --nodeIds=$LND_PUBKEY,$C_PUBKEY \
        --message=$PAYLOAD &
done
wait
```

**Pass/Fail signal:**
- **PASS** if (a) lnd's log contains exactly **one**
  `per-peer onion message rate limit engaged` info line (or the
  v0.21.0 equivalent — verify the exact wording) for A's pubkey,
  (b) subsequent drops are at trace level only, and (c) C's
  receive count is bounded by the configured rate over the test
  window.
- **FAIL** if no drops occur (limiter disabled) or the info log is
  emitted repeatedly (log-flooding regression).

---

### S5: Global rate limiter trips

**Goal:** Multiple senders, each under their per-peer cap,
collectively trip the global cap.

**Steps:**
```bash
# Restart lnd with the per-peer limiter loose and the global tight:
#   protocol.onion-msg-peer-kbps=1024
#   protocol.onion-msg-peer-burst-bytes=262144
#   protocol.onion-msg-global-kbps=200
#   protocol.onion-msg-global-burst-bytes=131080

# Drive sends from A, B (relay-all enabled to allow B), and a third
# peer if available — all simultaneously, each under its per-peer
# budget.
```

**Pass/Fail signal:**
- **PASS** if lnd's log contains exactly one
  `global onion message rate limit engaged` info line (no peer
  prefix), and subsequent drops are at trace level.
- **FAIL** if no global drops occur, or the line is emitted with a
  peer prefix (mis-attribution).

---

### S6: Startup rejects invalid limiter configs

**Goal:** Mixed-zero (rate=0 with burst>0, or vice versa) and
undersized-burst configs fail startup, as documented in the
rate-limiting design doc. **No driver required.**

**Steps:** Start lnd with each of these in turn and capture exit
status:

| Config | Expected to reject? |
|---|---|
| `peer-kbps=0`, `peer-burst-bytes=262144` | yes (rate 0, burst > 0) |
| `peer-kbps=100`, `peer-burst-bytes=0`    | yes (rate > 0, burst 0) |
| `peer-kbps=100`, `peer-burst-bytes=32768`| yes (burst < 65535)     |
| `peer-kbps=0`,   `peer-burst-bytes=0`    | no  (cleanly disabled)  |

**Pass/Fail signal:**
- **PASS** if the three reject rows fail startup with a clear
  error message naming the misconfigured option **before** the
  gRPC endpoint comes up, and the disabled row starts cleanly.
- **FAIL** if any of the three invalid configs starts (silent
  misconfiguration), or the disabled config errors out.

---

### S7: Loopback drop

**Goal:** An onion message whose resolved next hop is the sending
peer is dropped at lnd, not forwarded back.

**Steps:**
```bash
# Eclair A constructs a route lnd → A — i.e. A is the recipient,
# lnd is the only intermediate hop. lnd will receive from A and
# resolve A as the next hop.
eclair-cli sendonionmessage \
    --nodeIds=$LND_PUBKEY,$A_PUBKEY \
    --message=$(printf 'loopback' | xxd -p)
```

**Pass/Fail signal:**
- **PASS** if lnd's log records a loopback drop (search for
  `next hop is sending peer` or the v0.21.0 equivalent), and A
  does not receive the message back over the inbound connection.
- **FAIL** if A receives the message back from lnd (the loopback
  drop did not engage) or lnd silently drops with no audit trail.

## Failure investigation

- **Subsystems:** `PEER`, `DISC`, `CRTR`, and the onion-message
  subsystem (verify exact name in v0.21.0 — probably `ONMSG` or
  similar). Set to `debug` for diagnosis.
- **Useful greps:**
  - `grep -i "onion message" lnd.log`
  - `grep -i "rate limit" lnd.log`
  - `grep -i "channel-presence" lnd.log`
- **Driver-side observability:** Eclair logs at `INFO` show
  outbound `sendonionmessage` calls and inbound message events. CLN
  surfaces inbound messages via the `onion_message_recv` hook.

## Related itests (not the RC test surface, but useful references)

- `itest/lnd_onion_message_test.go` — `testOnionMessage`
- `itest/lnd_onion_message_forward_test.go` —
  `testOnionMessageForwarding` with `buildForwardNextNodePath`,
  `buildForwardSCIDPath`, `buildConcatenatedPath`
- `itest/config_onion_ratelimit_test.go` — limiter config
- `onionmessage/test_utils.go` — `BuildOnionMessage` helper
  (`*testing.T`-only)

These exercise the same behaviors via Go-side construction and are
the maintainers' authoritative test. RC testers shouldn't need to
run them; this guide covers what's observable from a real
deployment with mixed-implementation drivers.

## Out of scope

- Pathfinding for onion messages (#10612) — used internally by lnd
  but not exposed via a user-facing RPC in v0.21.0, so not
  directly testable from outside this release. Confirm via itests.
- BOLT-12 offers / blinded-payment-route construction — separate
  feature, not in v0.21.0.
- lnd-as-sender of onion messages — v0.21.0 is forwarding-only
  from the operator's perspective. The `SendOnionMessage` RPC is
  low-level and has no `lncli` wrapper.
