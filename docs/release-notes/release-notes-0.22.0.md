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

* Bitcoind outbound peer health checks [now use](https://github.com/lightningnetwork/lnd/pull/10686)
  `getnetworkinfo.connections_out` instead of `getpeerinfo`. The same PR also
  [clarifies](https://github.com/lightningnetwork/lnd/issues/10568) the ZMQ
  port-mismatch warnings so they no longer suggest that the connection failed.

* [Restored insta-dispatch of `CLOSED_CHANNEL` on the first confirmation of a
  cooperative close](https://github.com/lightningnetwork/lnd/pull/10794).
  After the multi-conf reorg-aware close dispatch landed,
  `SubscribeChannelEvents` no longer emitted `CLOSED_CHANNEL` until the full
  required confirmation depth was reached. The chain watcher now fires an
  early `CLOSED_CHANNEL` event over the channel notifier as soon as the coop
  close spend lands on chain, restoring the v0.20.1 behavior, while the
  channel arbitrator suppresses the duplicate event that would otherwise be
  emitted from `MarkChannelClosed` at the final confirmation depth.

# New Features

## Functional Enhancements

## RPC Additions

* The `routerrpc.EstimateRouteFee` RPC now supports [restricting fee estimates
  to specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `outgoing_chan_ids` field in `RouteFeeRequest`.

## lncli Additions

* The `estimateroutefee` command now supports [restricting fee estimates to
  specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `--outgoing_chan_id` flag.

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

* The fundee now [enforces the BOLT-02 bound on
  `push_msat`](https://github.com/lightningnetwork/lnd/pull/10765),
  rejecting incoming `open_channel` messages where `push_msat` exceeds
  `1000 * funding_satoshis`. Oversized pushes were previously caught
  later in the reservation flow as a funder-balance-dust error; they now
  surface a clearer, spec-aligned error string up front.

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Boris Nagaev
* Erick Cestari
