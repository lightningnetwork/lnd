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
- [Contributors (Alphabetical Order)](#contributors)

# Bug Fixes

* [Fixed a panic](https://github.com/lightningnetwork/lnd/pull/10914) in the
  DNS fallback SRV lookup, which unconditionally type-asserted each DNS Answer
  record to `*dns.SRV` and crashed the daemon when the response contained a
  non-SRV record. Non-SRV records are now skipped, and an empty `LookupHost`
  result for the shim no longer triggers an out-of-bounds index.

- [Fixed on-chain forward interceptor
  settlement](https://github.com/lightningnetwork/lnd/pull/10895) after the
  incoming channel force closes. Held forwards are now tracked as off-chain or
  on-chain entries, allowing an on-chain re-offer to replace the old off-chain
  hold so settlement reaches the witness beacon. Go callers of the exported
  `htlcswitch.InterceptedPacket` type should use the new `Deadline` field to
  distinguish off-chain auto-fail heights from on-chain settlement deadlines,
  or `AutoFailHeight()` if they only need the legacy flattened value.

# New Features

## Functional Enhancements

## RPC Additions

* The [HTLC interceptor](https://github.com/lightningnetwork/lnd/pull/10942) now
  exposes the next hop of a blinded route that identifies it by node ID
  (`next_node_id`) rather than by channel.

## lncli Additions

# Improvements
## Functional Updates

* lnd now [validates the CLTV expiry of HTLCs at the final
  hop](https://github.com/lightningnetwork/lnd/pull/10927). A final HTLC whose
  CLTV expiry falls outside the node's receive policy is failed back, bringing
  the final hop in line with the CLTV delta limits already enforced on the
  forwarding path.
  As part of this change, the channel policy `TimeLockDelta` is now validated
  against LND's supported forwarding bounds: any node that previously set a
  per-channel `TimeLockDelta` greater than `2016` (the maximum default value)
  will now have its `UpdateChannelPolicy` request rejected, and must lower the
  value accordingly below the specified maximum.

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

* Erick Cestari
* Ziggie
