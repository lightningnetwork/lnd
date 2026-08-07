# Release Notes
- [Bug Fixes](#bug-fixes)
- [New Features](#new-features)
    - [Functional Enhancements](#functional-enhancements)
    - [RPC Additions](#rpc-additions)
    - [lncli Additions](#lncli-additions)
- [Improvements](#improvements)
    - [Functional Updates](#functional-updates)
    - [RPC Updates](#rpc-updates)
    - [lncli Updates](#lncli-updates)
    - [Breaking Changes](#breaking-changes)
    - [Performance Improvements](#performance-improvements)
    - [Deprecations](#deprecations)
- [Technical and Architectural Updates](#technical-and-architectural-updates)
    - [BOLT Spec Updates](#bolt-spec-updates)
    - [Testing](#testing)
    - [Database](#database)
    - [Code Health](#code-health)
    - [Tooling and Documentation](#tooling-and-documentation)
- [Contributors (Alphabetical Order)](#contributors-alphabetical-order)

# Bug Fixes

* [Bounded the memory used while syncing the channel
  graph](https://github.com/lightningnetwork/lnd/pull/10992). A peer replying
  to our `query_channel_range` could previously make us buffer an
  unpredictable number of short channel IDs, as the only limit was a coarse
  67MB cap on the bytes a single zlib-compressed reply could decompress to.
  Replies are now capped at a precise number of short channel IDs, both
  per-message and in aggregate across a single query, and the accumulated
  reply state is released as soon as any reply fails validation so that a
  peer cannot pin it by deliberately forcing an error.

* [Refined invoice update
  handling](https://github.com/lightningnetwork/lnd/pull/11024) across MPP, AMP,
  and legacy payment paths, including keysend records and preimage-dependent
  settlement outcomes.

* [Fixed a data race](https://github.com/lightningnetwork/lnd/pull/11019) in the
  legacy cooperative close state machine, which was advanced from both the link
  goroutine and the peer goroutine with nothing synchronizing the two. The link
  now reports a flushed channel to the peer's channel manager instead of driving
  the closer itself, so every step of a close runs on a single goroutine. The
  same change has the RBF closer validate the remote party's delivery script in
  all cases, rather than only when an upfront shutdown script was on record for
  that peer, and rejects an absent script instead of treating it as nothing to
  check.

* Outgoing contest resolvers now [retain the corresponding incoming HTLC
  expiry](https://github.com/lightningnetwork/lnd/pull/11032) when transitioning
  to timeout resolution, allowing the sweeper to continue using an
  expiry-aware confirmation target.

# New Features

## Functional Enhancements

## RPC Additions

* The [HTLC interceptor](https://github.com/lightningnetwork/lnd/pull/10942) now
  exposes the next hop of a blinded route that identifies it by node ID
  (`next_node_id`) rather than by channel.

## lncli Additions

# Improvements

## Functional Updates

* [The HTLC forward interceptor now validates](https://github.com/lightningnetwork/lnd/pull/11028)
  that derived auto-fail heights are within the supported range before they are
  exposed through the interceptor API.

## RPC Updates

* `ForwardHtlcInterceptRequest.outgoing_requested_chan_id` now holds a reserved
  sentinel value (`18446744073709551615`, all bits set) when the
  [HTLC interceptor](https://github.com/lightningnetwork/lnd/pull/10942) reports
  a blinded forward that identifies the next hop by node ID. The sender of such
  a forward requests no channel, so a zero value here would make a client that
  detects the exit hop by a zero channel ID classify the forward as a final
  receive. Clients that switch on this field must handle the sentinel and read
  `outgoing_requested_node_id` for the next hop.

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/10942) where an
  lnd node acting as a relaying node (including the introduction node) in a
  blinded path failed to forward the payment when the next hop was identified by
  node ID (`next_node_id`) rather than a short channel ID. The next hop's public
  key is now resolved to one of our channels with that peer using non-strict
  forwarding.

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* bitromortac
* Olaoluwa Osuntokun
* Ziggie
