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

* TLV decoders [now reject malformed records with incorrect
  lengths](https://github.com/lightningnetwork/lnd/pull/10249). Fixed-size
  records such as the inbound fee, Musig2 nonce and short channel ID are
  validated against their declared length, so a record whose length does not
  match is no longer silently accepted and re-encoded differently.

* Peer connections [now rate limit inbound ping replies and bound outgoing
  message queue growth](https://github.com/lightningnetwork/lnd/pull/11130),
  preventing peer-controlled resource exhaustion.

* Channel updates carrying [inbound fees now sign the same bytes that are
  broadcast](https://github.com/lightningnetwork/lnd/pull/11130), preventing
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

# New Features

## Functional Enhancements

## RPC Additions

## lncli Additions

# Improvements

## Functional Updates

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
* Erick Cestari
* LNBiG
* Yong Yu
* Ziggie
