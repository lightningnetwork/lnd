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

## RPC Additions

* The `routerrpc.EstimateRouteFee` RPC now supports [restricting fee estimates
  to specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `outgoing_chan_ids` field in `RouteFeeRequest`.

* [Added a `raw_tx_hex` field to the `PendingSweep`
  response](https://github.com/lightningnetwork/lnd/pull/10670) returned by
  `walletrpc.PendingSweeps`. The field contains the serialized hex of the most
  recent sweep transaction spending the input, allowing callers (notably
  consumers of `BumpFee`) to inspect or rebroadcast the in-flight sweep
  without having to scrape the mempool.

## lncli Additions

* The `estimateroutefee` command now supports [restricting fee estimates to
  specific first-hop outgoing
  channels](https://github.com/lightningnetwork/lnd/pull/10501) via the new
  `--outgoing_chan_id` flag.

# Improvements

## Functional Updates

## RPC Updates

## lncli Updates

* `lncli wallet bumpfee` now prints a hint pointing users at `lncli wallet
  pendingsweeps` to inspect the in-flight sweep transaction, including its
  raw hex
  ([#10670](https://github.com/lightningnetwork/lnd/pull/10670)).

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
