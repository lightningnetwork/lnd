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

* [Fixed a bug](https://github.com/lightningnetwork/lnd/pull/10782)
  that could be encountered during co-op closes whereby
  `ChanStatusCoopBroadcasted` was set before a close transaction
  actually existed. As a side effect, channels in shutdown
  negotiation now remain in `ListChannels` (as inactive) until
  the close transaction is actually broadcast, and
  `WaitingCloseChannel.ClosingTx` is never empty.

# New Features

## Functional Enhancements

* When making an on-chain send, callers can now specify a
  [`change_address`](https://github.com/lightningnetwork/lnd/pull/10810) to
  control where any change output is sent. This enables workflows where a user
  wants to drain a specific address across multiple transactions — by pinning
  the change address, every sweep transaction returns leftover funds to a
  known, controlled address rather than a fresh wallet-derived one. The field
  is mutually exclusive with `send_all`.

## RPC Additions

* The `routerrpc.EstimateRouteFee` RPC now supports [restricting fee estimates
  to specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `outgoing_chan_ids` field in `RouteFeeRequest`.

* [`SendCoins`](https://github.com/lightningnetwork/lnd/pull/10810) now accepts
  a `change_address` field on `SendCoinsRequest`. When set, change is sent to
  the specified address instead of a wallet-derived one. Cannot be combined
  with `send_all`.

* [`SendMany`](https://github.com/lightningnetwork/lnd/pull/10810) now accepts
  a `change_address` field on `SendManyRequest` with the same semantics.

* [`WalletKit.SendOutputs`](https://github.com/lightningnetwork/lnd/pull/10810)
  now accepts a `change_address` field on `SendOutputsRequest` with the same
  semantics.

## lncli Additions

* The `estimateroutefee` command now supports [restricting fee estimates to
  specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `--outgoing_chan_id` flag.

* `sendcoins` now accepts a
  [`--change_address`](https://github.com/lightningnetwork/lnd/pull/10810) flag
  to specify a custom destination for change. Mutually exclusive with
  `--sweepall`.

* `sendmany` now accepts a
  [`--change_address`](https://github.com/lightningnetwork/lnd/pull/10810) flag
  with the same semantics.

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
