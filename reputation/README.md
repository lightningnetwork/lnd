reputation
==========

[![Build Status](http://img.shields.io/travis/lightningnetwork/lnd.svg)](https://travis-ci.org/lightningnetwork/lnd)
[![MIT licensed](https://img.shields.io/badge/license-MIT-blue.svg)](https://github.com/lightningnetwork/lnd/blob/master/LICENSE)
[![GoDoc](https://img.shields.io/badge/godoc-reference-blue.svg)](http://godoc.org/github.com/lightningnetwork/lnd/reputation)

The reputation package implements local reputation tracking to help mitigate
channel jamming, following the scoring recommended in [BOLT
\#1280](https://github.com/lightning/bolts/pull/1280) (local resource
conservation). A forwarding node uses it to build an unforgeable history of how
each channel has behaved as an outgoing peer, so that it can later distinguish
peers that are likely being used to jam its channels from those that are not.

The package is **observational only**: it watches the HTLCs the node forwards,
maintains a per-channel reputation score, and logs the decision it would make
for each HTLC. It never affects forwarding or alters the wire.

## Reputation scoring

Every forwarded HTLC contributes an `effective_fee` to its outgoing channel's
reputation, adjusted for how long it was held: HTLCs that resolve within a
`resolution_period` contribute their full fee, while slower ones are penalised
by an `opportunity_cost` that grows with the overrun. Unaccountable HTLCs can
only ever help reputation, never harm it.

Three quantities determine whether a channel has sufficient reputation for a
given HTLC:

  * **Outgoing channel reputation**: the sum of effective fees the outgoing
    channel has earned over a long rolling window, tracked as a decaying
    average.
  * **Incoming channel revenue threshold**: the routing revenue the incoming
    channel has generated over a shorter window, aggregated over several
    windows so a peer cannot cheaply move its own threshold.
  * **In-flight risk**: the worst-case opportunity cost of the HTLC assuming it
    is held until just before its incoming CLTV expiry.

An HTLC's outgoing channel is considered to have sufficient reputation when:

    outgoing_channel_reputation - in_flight_risk >= incoming_revenue_threshold

This is evaluated two ways for each forward: against the HTLC's own risk alone,
and against that plus the risk of the accountable HTLCs already in flight on the
outgoing channel. Both verdicts are logged at debug level (subsystem `REPM`),
as is the reputation change applied on each resolution, since these lines fire
once per forwarded HTLC.

The rolling windows are implemented as decaying averages to avoid storing
per-HTLC history; see `decaying_average.go`.

## Integration with the switch

The subsystem observes forwarding through three read-only hooks the switch
calls at the circuit layer: `OnForward` when it commits to forwarding an HTLC,
and `OnSettle`/`OnFail` when the HTLC resolves. The hooks run synchronously and
do only a handful of map lookups and floating-point operations, so they sit on
the forwarding path without a background worker.

When the subsystem is disabled the switch skips the hooks behind a nil check.
When it is enabled the manager is wrapped in a panic boundary before being
handed to the switch, so a bug in this (log-only) package can never take down
HTLC forwarding.

Every resolution removes its own pending HTLC, so a pending entry that outlives
the worst case time it could be held for means a resolution was never reported
to us. Such entries are logged as a warning and deliberately left in place
rather than swept away, so the underlying bug stays visible.

## Persistence

Channel state is persisted through the `Store` interface so that peers keep
the reputation they built when the node restarts. The SQL implementation is
used when the node runs with `db.use-native-sql`; otherwise a no-op store is
used, reputation lives in memory only and re-accrues from live traffic after
a restart.

Only the two averages are stored, each as its running value and the time it
was last updated, plus the revenue start time for the warm-up factor. Decay is
applied lazily on read, so restoring those verbatim is all that is needed for
the downtime to be accounted for: the first read after a restart decays the
value over the whole gap. Timestamps that lie in the future on load (the clock
went backwards) are clamped to the load time, and channels whose averages
have both decayed to zero are dropped rather than restored.

Channels whose state changed are written every minute and on shutdown, so an
unclean shutdown loses at most the last minute of changes. When a channel
closes its state is removed from memory and from the store.

Pending HTLCs are not persisted. On startup they are rebuilt from the switch's
in-flight circuits, so in-flight risk survives a restart too.

## Operational notes

The subsystem is enabled by default and can be disabled with the
`routing.no-reputation` configuration flag.

## Installation and Updating

```shell
$  go get -u github.com/lightningnetwork/lnd/reputation
```
