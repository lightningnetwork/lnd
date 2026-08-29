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

* Peer connections [now rate limit inbound ping replies and bound outgoing
  message queue growth](https://github.com/lightningnetwork/lnd/pull/11090),
  preventing peer-controlled resource exhaustion.

* Channel updates carrying [inbound fees now sign the same bytes that are
  broadcast](https://github.com/lightningnetwork/lnd/pull/11090), preventing
  remote signature failures. Forwarded updates also preserve unknown signed
  TLV extensions.

* Channel funding attempts [now return
  cleanly](https://github.com/lightningnetwork/lnd/pull/11035) when their
  pending wallet reservation is no longer present.

* Zero-block [`query_channel_range` and `reply_channel_range`
  messages](https://github.com/lightningnetwork/lnd/pull/11035) now retain their
  first block height when calculating a defensive range boundary, and dense
  first blocks no longer produce zero-block reply prefixes.

* [`GetTransactions`
  pagination](https://github.com/lightningnetwork/lnd/pull/11075) now handles
  overflowing offset and limit combinations without reaching a slice-bounds
  panic.

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/10869) where an
  incoming HTLC resolver could treat a foreign commitment spend as its own
  success transaction and offer a phantom input to the sweeper.

* Native SQL invoice migration [now correctly associates legacy AMP invoice
  HTLCs](https://github.com/lightningnetwork/lnd/pull/11106) with their AMP
  sub-invoices. Previously, the HTLC rows were inserted without those
  associations, causing verification to fail and the migration transaction to
  roll back.

* [Fixed a lock order inversion](https://github.com/lightningnetwork/lnd/pull/11008)
  between `PsbtFundingVerify` and `handleFundingCancelRequest` in the wallet.
  With PSBT or batch funding the two could deadlock the wallet's single
  `requestHandler` goroutine, which permanently disabled all channel funding for
  the whole node: no new channel could be opened, and channels whose funding
  transaction confirmed stayed in the `channelReadySent` opening state forever,
  never added to the graph and never announced. Only a restart recovered.

* [Fixed an issue](https://github.com/lightningnetwork/lnd/pull/11140) where the
  incoming side of a forwarded dust HTLC could remain stuck.

* [Fixed a panic](https://github.com/lightningnetwork/lnd/pull/11122) in the
  REST WebSocket proxy, where a `Sec-Websocket-Protocol` header carrying an
  allowed field name without the `+` delimiter caused an index out of range
  while the header was being forwarded to the backend. The header is now parsed
  as the comma separated list of sub protocols it is, so a bare protocol name
  can also no longer pick up the value of an unrelated sub protocol in the same
  list.

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

* The REST WebSocket proxy now [bounds the size of incoming
  messages](https://github.com/lightningnetwork/lnd/pull/11122) using
  `MaxWsMsgSize`, the limit that was already applied to the responses it writes
  back out. Oversized frames are rejected from their header rather than read in
  full.

## RPC Updates

## lncli Updates

## Breaking Changes

## Performance Improvements

## Deprecations

# Technical and Architectural Updates

## BOLT Spec Updates

## Testing

## Database

## Code Health

## Tooling and Documentation

# Contributors (Alphabetical Order)

* Boris Nagaev
* Gijs van Dam
* LNBiG
* Yong Yu
* Ziggie
