# Onion Message Rate Limiting

Your node forwards an onion message. The peer on the other side paid
nothing. You paid for the bandwidth that carried it, the CPU that peeled
one Sphinx layer off it, and the disk I/O that checked its HMAC against
the replay database. That asymmetry is the whole problem this document
is about.

This guide explains how lnd bounds the cost of forwarded onion messages
and why each piece of the defense exists. The audience is operators who
want to understand what the knobs do before turning them, and
contributors who want the shape of the system before they read the code.

## The adversary

Picture a peer that sends you a maximum-size onion message as fast as
its link allows. Nothing in the protocol asks it to pay per message, so
it keeps going. Forwarding is the escape hatch a spammer wants: free
bandwidth, reachable anywhere on the network, impossible to charge for
individually.

Now picture a peer that does this from a thousand identities at once. A
single-peer bandwidth cap does not stop it. Ten thousand peers each
sending a modest trickle can still saturate any aggregate cap you
choose, if new identities are free to mint.

lnd's defense runs in two layers. The first turns peer identity into a
capital cost so that mint-at-will stops working. The second caps the
bandwidth any one peer and the node as a whole can burn on onion
messages. The two layers compose: cheap Sybils hit the first gate, a
legitimate peer that goes off the rails hits the second.

## Layer one: the channel-presence gate

Before the rate limiters see an incoming onion message, lnd asks a
blunt question about the peer that sent it: does this peer have at
least one fully open channel with us? If not, the message is dropped.
No tokens debited, no Sphinx work done, no replay lookup issued.

A funded channel is the cheapest thing lnd has that an attacker cannot
fake. Opening one costs an on-chain transaction fee and locks up
capital in a funding output. That cost is small for an honest peer
that plans to use the channel; it is prohibitive for an attacker that
wants ten thousand disposable identities. The gate inherits that
economic property for free.

Pending channels do not satisfy the gate. They are deliberately
excluded: a pending channel is one where we have sent or received a
funding transaction but the channel is not yet confirmed, so the
capital is not yet locked in. Pending channels are also cheap to open
and, under adversarial conditions, easy to leave stuck. Counting them
would hand the attacker the same free-identity primitive the gate
exists to remove.

The gate runs on a hot path — every onion message pays its cost. It
reads one atomic counter that shadows the count of fully-open channels
for the peer, which keeps the check to a single `Load` instruction. No
map iteration, no lock.

## Layer two: the rate limiters

Past the gate, two token-bucket limiters bound the bandwidth onion
messages can consume.

- The **per-peer** limiter draws from one bucket per peer key. A peer
  that opens its throttle to the wall hits this first.
- The **global** limiter draws from a single bucket shared across the
  whole node. A thousand peers each sending just under their per-peer
  cap hit this.

The two run in series: per-peer first, global second. A peer whose
own bucket is empty never gets to debit the global bucket on a
rejected attempt, so a hostile peer cannot drain the shared budget by
sending messages it knows will be dropped.

### Tokens are bytes, not messages

The buckets hold bytes. Each onion message debits its on-the-wire
size. A small control message pays proportionally less of the budget
than a spec-maximum 32 KiB one. The configured limits therefore
reflect bandwidth, not message counts — which is the thing operators
actually care about when they decide how much of their pipe to give
away.

The alternative — counting raw messages — lets an attacker choose the
worst cost profile and pay a fixed count for it. Byte accounting
denies the attacker that optimization.

### Per-peer state lives across disconnect

The per-peer bucket does not reset when the peer disconnects. If it
did, a hostile peer could cycle the connection to roll its bucket
back to full. Instead, the bucket is keyed by the peer's compressed
public key and retained. The memory footprint is bounded by the
channel-presence gate: only peers with a channel ever allocate a
per-peer bucket in the first place, so the set cannot grow
unboundedly through connection churn.

## The escape hatch: `protocol.onion-msg-relay-all`

Some operators run nodes that are supposed to accept onion messages
from everywhere: test nodes, public relays, research nodes. For those
cases `protocol.onion-msg-relay-all=true` disables the
channel-presence gate. Messages from peers with no channel are
admitted into the rate-limiter pipeline as if the gate were not
there. The rate limiters still apply.

The tradeoff is explicit: you trade the Sybil-resistance property of
the gate for reachability. With `relay-all=true`, a peer that costs
nothing to spin up can now burn a full per-peer byte budget on each
identity. The global limiter is the only line of defense left. Only
enable this if you understand that and are willing to sit behind the
global cap alone.

The default is `false`. For an ordinary routing or personal node,
leaving it off is the right choice.

## The knobs

All four onion-message rate-limit settings live under the `protocol`
section.

| Flag | Default | Meaning |
| --- | --- | --- |
| `protocol.onion-msg-peer-kbps` | `512` | Per-peer sustained rate in decimal kilobits per second. |
| `protocol.onion-msg-peer-burst-bytes` | `262144` | Per-peer token bucket depth in bytes. |
| `protocol.onion-msg-global-kbps` | `5120` | Global sustained rate in decimal kilobits per second. |
| `protocol.onion-msg-global-burst-bytes` | `1638400` | Global token bucket depth in bytes. |
| `protocol.onion-msg-relay-all` | `false` | If true, skip the channel-presence gate. |

### Rules at startup

A few configurations are invalid and rejected before the node starts:

- A rate with a zero burst, or a burst with a zero rate, is rejected.
  Either both are positive (the limiter is enabled) or both are zero
  (the limiter is disabled). Mixing the two would silently disable
  the limiter and leave the operator thinking they had protection.
- A burst smaller than the maximum-sized onion message wire size
  (65535 bytes) is rejected. A bucket that cannot fit a single valid
  message would reject every call, regardless of the rate.

Both of these are startup errors, not runtime ones — the node fails
fast so that the operator sees the mistake immediately.

### Default sizing

The defaults assume a routing node that wants onion-message forwarding
to be present but not dominant.

- `0.5 Mbps` per peer, `5 Mbps` globally. An individual peer is
  capped well below a typical residential uplink; the aggregate is
  sized so onion message forwarding cannot dwarf the bandwidth a
  routing node uses for payments.
- `256 KiB` per-peer burst, `1.6 MiB` global burst. Both are many
  multiples of a spec-max message, so short bursts of legitimate
  traffic are absorbed without rejection, and the long-term rate
  remains bounded by the kbps settings.
- At default rates, ten peers pushing their per-peer allowance
  simultaneously exactly fills the global budget. Push the per-peer
  rate down or the global rate up if you expect many onion-active
  peers.

## Operator recipes

**I want to disable the per-peer limiter, keep only the global cap.**
Set both per-peer values to zero:

```
protocol.onion-msg-peer-kbps=0
protocol.onion-msg-peer-burst-bytes=0
```

The global limiter still runs. Be aware you have given up the per-peer
fairness: one loud peer can consume the entire global budget.

**I want to disable all rate limiting on this feature.** Set all four
values to zero. Read the adversary section above first; if you are not
sure whether you want this, you do not want this.

**I want to run a public relay that accepts onion messages from
anyone.** Enable the relay-all flag and raise the global cap:

```
protocol.onion-msg-relay-all=true
protocol.onion-msg-global-kbps=51200
protocol.onion-msg-global-burst-bytes=16384000
```

You are now defended by the global cap alone. Monitor traffic.

**I run a hobbyist node and never use onion messages myself.**
Leave the defaults. Onion-message traffic will land below the caps
and you will never notice them.

## What happens when a limiter trips

The first time either limiter trips, lnd emits a one-shot info log so
you can tell that a cap has become load-bearing.

- Per-peer trips log with the peer's own log prefix, so you know
  which peer drove it.
- Global trips log without a peer prefix — the shared bucket is not
  attributable to any single peer.

Subsequent drops are logged at trace level. A sustained attack
therefore does not flood your logs; the single info line already tells
you the system is engaged.

The limiter tracks a per-limiter drop counter you can read from the
logs or through diagnostics. Rising drop counts on the per-peer side
usually mean you should look at which peer is responsible; rising
drops on the global side usually mean the aggregate cap is too tight
for your traffic.

## Further reading

- Release notes for the feature:
  [#10713](https://github.com/lightningnetwork/lnd/pull/10713).
- Onion message specification in the BOLTs:
  [BOLT 4](https://github.com/lightning/bolts/blob/master/04-onion-routing.md).
